package store

// ===== M9 报告分享 · 库侧 TC（TC-M9-01~08 + A11/A12 关键不变量）=====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 上游链路复用 M6/M8 夹具（真实链：车 → 取样 → 结论 → 投料 → 作业段 → 成品 → 出货）；
//	检测结果数值行属上游对象夹具 ⇒ 直写库补一行（§5 允许）。
// ★ 快照目录每例独立（t.Setenv JX_REPORT_DIR = t.TempDir()）⇒ 跨用例不互相污染（§6-13）。
// ★ 清理：b_share_access 先于 b_share_report（先子后父），按 created_by 精确匹配。
// ★ 接口级 TC 见 internal/httpapi/m9_test.go；reportd 行为见 cmd/reportd/main_test.go。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

const (
	m9OtherCustCode  = "9431" // TC-M9-05 的「另一家客户」
	m9OtherCustName  = "M9别家客户"
	m9TestItemCode   = "M9T1"
	m9TestItemName   = "M9测试项目"
	m9InternalRemark = "M9内部备注不外传"
)

// m9Wipe 清掉 M9 库侧测试数据（★ 先子后父）。
func m9Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := m6StoreOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_share_access WHERE report_id IN
	       (SELECT id FROM b_share_report WHERE created_by = ?)`, me)
	q(`DELETE FROM b_share_report WHERE created_by = ?`, me)
	q(`DELETE FROM m_test_item WHERE code = ? AND created_by = ?`, m9TestItemCode, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m9OtherCustCode)
}

// m9Fixture = M8 夹具（M6 全链 + 出货清场）+ M9 清场 + 独立快照目录。
func m9Fixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	st, cust, inMat, outMat := m8Fixture(t)
	m9Wipe(t, st)
	t.Cleanup(func() { m9Wipe(t, st) })
	t.Setenv("JX_REPORT_DIR", t.TempDir())
	return st, cust, inMat, outMat
}

// m9Item 建检测项目字典行（快照检测表要显示项目名）。
func m9Item(t *testing.T, st *Store) int64 {
	t.Helper()
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO m_test_item (code, name, unit, value_type, decimals, status, version, is_current, created_by)
VALUES (?, ?, '%', '数值', 2, '启用', 1, 1, ?)`, m9TestItemCode, m9TestItemName, m6StoreOpenID)
	if err != nil {
		t.Fatalf("建检测项目失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// m9Chain 造一条完整链：合格车次 + 检测结果行 + 生产批 + 投料 + 作业段 + 成品批袋 + 出货。
// 返回生产批与成品袋码。
func m9Chain(t *testing.T, st *Store, cust, inMat, outMat int64, withResult bool) (ProductionBatch, []string) {
	t.Helper()
	ctx := context.Background()

	truck, bags := m6Pass(t, st, cust, inMat, 1)
	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags[0].Code)
	team := int64(88)
	if _, err := st.AddOperation(ctx, batch.ID, OperationInput{
		TeamID: &team, Operator: "内部操作员X",
		StartAt: "2026-10-02 08:00:00", EndAt: "2026-10-02 12:00:00",
		OutputWeight: &tons{milli: 5000},
	}, m6Actor()); err != nil {
		t.Fatalf("记作业段失败: %v", err)
	}
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{
		PackSpec: "吨袋", NetWeight: &tons{milli: 10000},
	}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	fgBags, err := st.GenerateFgBags(ctx, lot.ID, 2, m6Actor())
	if err != nil {
		t.Fatalf("生成成品袋失败: %v", err)
	}
	codes := []string{fgBags[0].Code, fgBags[1].Code}
	m8Ship(t, st, codes, true)

	if withResult {
		itemID := m9Item(t, st)
		inspID := m8InspIDOfTruck(t, st, truck.ID)
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_inspection_result (inspection_id, item_id, state, value_num, unit, judge, created_by)
VALUES (?,?, '已测', 98.5, '%', '合格', ?)`, inspID, itemID, m6StoreOpenID); err != nil {
			t.Fatalf("造检测结果失败: %v", err)
		}
	}
	return batch, codes
}

// m8InspIDOfTruck 取车次最新检测单 id。
func m8InspIDOfTruck(t *testing.T, st *Store, truckID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM b_inspection WHERE target_type = '车次' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, truckID).Scan(&id); err != nil {
		t.Fatalf("查车次检测单失败: %v", err)
	}
	return id
}

// m9Generate 生成一份报告（失败即 Fatal）。
func m9Generate(t *testing.T, st *Store, batchID int64) Report {
	t.Helper()
	rep, err := st.GenerateReport(context.Background(), GenerateReportInput{
		ScopeType: ReportScopeBatch, Scope: ReportScope{BatchID: batchID},
		Title: "M9检验报告",
	}, m6Actor())
	if err != nil {
		t.Fatalf("生成报告失败: %v", err)
	}
	return rep
}

// m9Snapshot 读快照 HTML（不存在即 Fatal）。
func m9Snapshot(t *testing.T, rep Report) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(reportDir(), filepath.FromSlash(rep.SnapshotPath)))
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("★★ 快照文件为空")
	}
	return string(b)
}

func m9CountReports(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_share_report WHERE created_by = ?`,
		m6StoreOpenID).Scan(&n); err != nil {
		t.Fatalf("统计报告失败: %v", err)
	}
	return n
}

// writeAccessLog 追加访问日志行（★ 模拟 reportd 的 O_APPEND 追加，不 rename）。
func writeAccessLog(t *testing.T, lines []string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(reportDir(), "access.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("打开访问日志失败: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("写访问日志失败: %v", err)
		}
	}
}

// ===== TC-M9-01 正常：生成报告 =====

// TC-M9-01 正常：选批次生成 ⇒ b_share_report 1 行、report_no 形态 RP+YYMMDD-NNN、
// status=有效、token 长度 ≥32；★ 快照落 served/ 且非空；内容含追溯链与检测表（A2）。
func TestTC_M9_01_Store_GenerateSnapshot(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	batch, codes := m9Chain(t, st, cust, inMat, outMat, true)
	rep := m9Generate(t, st, batch.ID)

	// ① 行级断言
	if n := m9CountReports(t, st); n != 1 {
		t.Fatalf("★★ 应恰 1 行报告，实际 %d", n)
	}
	re := regexp.MustCompile(`^RP\d{6}-\d{3}$`)
	if !re.MatchString(rep.ReportNo) {
		t.Fatalf("★★★ report_no 形态应 RP+YYMMDD+NNN，实际 %q", rep.ReportNo)
	}
	if rep.Status != ReportStatusActive {
		t.Fatalf("status 应「有效」，实际 %q", rep.Status)
	}
	if len(rep.Token) < 32 {
		t.Fatalf("★★ token 长度应 ≥32，实际 %d（%q）", len(rep.Token), rep.Token)
	}
	if rep.Scope.BatchID != batch.ID || rep.ScopeType != ReportScopeBatch {
		t.Fatalf("范围错乱: %+v", rep.Scope)
	}
	if rep.ExpiresAt == nil {
		t.Fatalf("★ 缺省有效期不得为 NULL（§6-6）")
	}

	// ② 快照落盘
	served := filepath.Join(reportDir(), "served", rep.Token+".html")
	stt, err := os.Stat(served)
	if err != nil || stt.Size() == 0 {
		t.Fatalf("★★★ 快照应存在于 served/ 且非空: %v", err)
	}

	// ③ 内容含追溯链与检测表
	html := m9Snapshot(t, rep)
	if !strings.Contains(html, batch.Human) {
		t.Fatalf("★★ 快照应含生产批人读行 %q", batch.Human)
	}
	if !strings.Contains(html, "检测结果") || !strings.Contains(html, m9TestItemName) {
		t.Fatalf("★★★ 快照应含检测结果表（含项目名），实际缺")
	}
	if !strings.Contains(html, "98.5") {
		t.Fatalf("★★ 快照应含检测数值 98.5")
	}
	if !strings.Contains(html, "投料明细") || !strings.Contains(html, "出货单") {
		t.Fatalf("★★ 快照缺追溯链板块")
	}
	_ = codes
	_ = ctx
}

// ===== TC-M9-02 ★ 正常：刷新 = 新 token 新链接 =====

// TC-M9-02 ★：刷新后 ① 新行 token ≠ 旧行 token；② 旧行 status=已撤销；
// ③ 旧快照已移出 served/；④ 新快照可服务（A3 / §6-4）。
func TestTC_M9_02_Store_RefreshNewToken(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)
	old := m9Generate(t, st, batch.ID)

	fresh, superseded, err := st.RefreshReport(ctx, old.ID, m6Actor())
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if superseded != old.ReportNo {
		t.Fatalf("superseded_report_no 应为 %q，实际 %q", old.ReportNo, superseded)
	}
	// ① 新 token ≠ 旧 token，且新行独立
	if fresh.Token == old.Token {
		t.Fatalf("★★★ 刷新必须新 token（不得复用旧链接）")
	}
	if fresh.ID == old.ID || fresh.ReportNo == old.ReportNo {
		t.Fatalf("★★ 刷新应新建行（新编号），实际同 id/no")
	}
	if n := m9CountReports(t, st); n != 2 {
		t.Fatalf("刷新后应 2 行，实际 %d", n)
	}

	// ② 旧行已撤销
	var oldStatus string
	if err := ctxErr(st, `SELECT status FROM b_share_report WHERE id = ?`, old.ID, &oldStatus); err != nil {
		t.Fatalf("读旧报告状态失败: %v", err)
	}
	if oldStatus != ReportStatusRevoked {
		t.Fatalf("★★★ 旧行 status 应「已撤销」，实际 %q", oldStatus)
	}

	// ③ 旧快照移出 served/ ④ 新快照在 served/
	if _, err := os.Stat(filepath.Join(reportDir(), "served", old.Token+".html")); !os.IsNotExist(err) {
		t.Fatalf("★★★ 旧快照必须移出 served/（err=%v）", err)
	}
	if _, err := os.Stat(filepath.Join(reportDir(), "served", fresh.Token+".html")); err != nil {
		t.Fatalf("★★★ 新快照应在 served/: %v", err)
	}
}

// ctxErr 小工具：QueryRowContext 扫一个字符串列。
func ctxErr(st *Store, query string, id int64, dst *string) error {
	return st.db.QueryRowContext(context.Background(), query, id).Scan(dst)
}

// ===== TC-M9-03 异常：撤销后不可访问 =====

// TC-M9-03 ★★：撤销 ⇒ status=已撤销 + 快照移出 served/（公网侧因此 404，
// reportd 侧见 cmd/reportd/main_test.go 与服务器 smoke）。reason 必填（A4 / §6-2）。
func TestTC_M9_03_Store_RevokeMovesSnapshot(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)
	rep := m9Generate(t, st, batch.ID)

	// reason 空 ⇒ 400
	if _, err := st.RevokeReport(ctx, rep.ID, "   ", m6Actor()); !errors.Is(err, ErrReportBadInput) {
		t.Fatalf("★★ 空 reason 应 ErrReportBadInput，实际 %v", err)
	}

	if _, err := st.RevokeReport(ctx, rep.ID, "客户要求撤回", m6Actor()); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	got, err := st.GetReport(ctx, rep.ID)
	if err != nil {
		t.Fatalf("读报告失败: %v", err)
	}
	if got.Status != ReportStatusRevoked {
		t.Fatalf("★★★ 撤销后 status 应「已撤销」，实际 %q", got.Status)
	}
	if _, err := os.Stat(filepath.Join(reportDir(), "served", rep.Token+".html")); !os.IsNotExist(err) {
		t.Fatalf("★★★ 撤销后快照必须移出 served/（旧链接天然 404），err=%v", err)
	}
	// 审计留 reason
	var reason string
	if err := st.db.QueryRowContext(ctx,
		`SELECT COALESCE(reason,'') FROM s_audit_log
		  WHERE entity='b_share_report' AND entity_id=? AND action='report_revoke'
		  ORDER BY id DESC LIMIT 1`, rep.ID).Scan(&reason); err != nil {
		t.Fatalf("查撤销审计失败: %v", err)
	}
	if reason != "客户要求撤回" {
		t.Fatalf("★ 审计应记 reason，实际 %q", reason)
	}
	// 重复撤销 ⇒ 状态冲突
	if _, err := st.RevokeReport(ctx, rep.ID, "再来一次", m6Actor()); !errors.Is(err, ErrReportState) {
		t.Fatalf("重复撤销应 ErrReportState，实际 %v", err)
	}
}

// ===== TC-M9-04 边界：有效期末日 =====

// TC-M9-04 ★★：expires_at = 今日 23:59:59.999 ⇒ sweep 后仍有效且快照在 served/；
// = 昨日 ⇒ sweep 后已过期且快照移出；★ sweep 可重复跑（第二次 swept=0）（A5 / §6-6）。
func TestTC_M9_04_Store_ExpiryBoundaryAndSweep(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()
	now := time.Now()

	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)

	today := m9Generate(t, st, batch.ID)
	if _, err := st.SetReportExpires(ctx, today.ID, now.Format("2006-01-02"), m6Actor()); err != nil {
		t.Fatalf("设今日有效期失败: %v", err)
	}
	yesterday := m9Generate(t, st, batch.ID)
	if _, err := st.SetReportExpires(ctx, yesterday.ID,
		now.AddDate(0, 0, -1).Format("2006-01-02"), m6Actor()); err != nil {
		t.Fatalf("设昨日有效期失败: %v", err)
	}

	swept, nos, err := st.SweepExpiredReports(ctx, m6Actor())
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if swept != 1 || len(nos) != 1 || nos[0] != yesterday.ReportNo {
		t.Fatalf("★★ 应恰清扫 1 条（昨日那条），实际 swept=%d nos=%v", swept, nos)
	}

	// 今日：仍有效 + 快照在
	tRow, err := st.GetReport(ctx, today.ID)
	if err != nil {
		t.Fatalf("读今日报告失败: %v", err)
	}
	if tRow.Status != ReportStatusActive {
		t.Fatalf("★★★ 末日 23:59:59.999 应仍「有效」，实际 %q", tRow.Status)
	}
	if _, err := os.Stat(filepath.Join(reportDir(), "served", tRow.Token+".html")); err != nil {
		t.Fatalf("★★★ 有效报告快照应仍在 served/: %v", err)
	}

	// 昨日：已过期 + 快照移出
	yRow, err := st.GetReport(ctx, yesterday.ID)
	if err != nil {
		t.Fatalf("读昨日报告失败: %v", err)
	}
	if yRow.Status != ReportStatusExpired {
		t.Fatalf("★★★ 过期报告 status 应「已过期」，实际 %q", yRow.Status)
	}
	if _, err := os.Stat(filepath.Join(reportDir(), "served", yRow.Token+".html")); !os.IsNotExist(err) {
		t.Fatalf("★★★ 过期报告快照必须移出 served/，err=%v", err)
	}

	// 幂等：第二次 swept=0
	swept2, _, err := st.SweepExpiredReports(ctx, m6Actor())
	if err != nil {
		t.Fatalf("二次清扫失败: %v", err)
	}
	if swept2 != 0 {
		t.Fatalf("★★★ sweep 必须幂等，第二次应 swept=0，实际 %d", swept2)
	}
}

// ===== TC-M9-05 ★ 安全：页内不含其它客户数据 =====

// TC-M9-05 ★★：两个客户各一批，给 A 出报告 ⇒ 快照不含 B 的客户名 / 批码 / 袋码（A6）。
func TestTC_M9_05_Store_NoOtherCustomerData(t *testing.T) {
	st, custA, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	batchA, _ := m9Chain(t, st, custA, inMat, outMat, false)

	// 客户 B + 批 B
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, ?, '启用', 1, 1, ?)`, m9OtherCustCode, m9OtherCustName, m6StoreOpenID)
	if err != nil {
		t.Fatalf("建另一家客户失败: %v", err)
	}
	custB, _ := res.LastInsertId()
	batchB := m6Batch(t, st, custB, inMat, outMat)

	repA := m9Generate(t, st, batchA.ID)
	html := m9Snapshot(t, repA)

	for _, forbidden := range []string{
		m9OtherCustName,
		batchB.Human, batchB.Code,
		batchA.Code, // 全码不外露：对外只给人读行
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("★★★ 快照命中不该出现的内容 %q", forbidden)
		}
	}
	if !strings.Contains(html, batchA.Human) {
		t.Fatalf("快照应含本批人读行")
	}
}

// ===== TC-M9-06 ★ 架构：快照白名单 =====

// TC-M9-06 ★★：快照不含内部字段 —— remark / 操作人 / created_by / 紧急放行 /
// 审计 / 留样 / 班组人员（A7 / §6-3 黑名单 ③）。
func TestTC_M9_06_Store_SnapshotWhitelist(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	// 生产批带 remark（内部字段）
	truck, bags := m6Pass(t, st, cust, inMat, 1)
	batch, err := st.CreateProductionBatch(ctx, CreateBatchInput{
		CustomerID: cust, InputMaterialID: inMat, PlannedOutputMaterialID: outMat,
		BatchDate: m6Date, Remark: m9InternalRemark,
	}, m6Actor())
	if err != nil {
		t.Fatalf("建生产批失败: %v", err)
	}
	m6Feed(t, st, batch.ID, bags[0].Code)
	team := int64(88)
	if _, err := st.AddOperation(ctx, batch.ID, OperationInput{
		TeamID: &team, Operator: "内部操作员X",
		StartAt: "2026-10-02 08:00:00", EndAt: "2026-10-02 12:00:00",
		OutputWeight: &tons{milli: 5000},
	}, m6Actor()); err != nil {
		t.Fatalf("记作业段失败: %v", err)
	}
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	fgBags, err := st.GenerateFgBags(ctx, lot.ID, 1, m6Actor())
	if err != nil {
		t.Fatalf("生成成品袋失败: %v", err)
	}
	m8Ship(t, st, []string{fgBags[0].Code}, false)

	rep := m9Generate(t, st, batch.ID)
	html := m9Snapshot(t, rep)

	black := []string{
		m9InternalRemark, // 生产批 remark
		"内部操作员X",         // 作业段 operator（★ 白名单只有班组）
		"投料工",            // 投料 operator
		m6StoreOpenID,    // created_by / 操作痕迹
		"紧急放行",           // urgent_* 留痕
		"留样",             // 样品 / 留样信息
		"审计",             // 审计信息
		batch.Code,       // 全码（对外只给人读行）
	}
	for _, w := range black {
		if strings.Contains(html, w) {
			t.Fatalf("★★★ 快照命中黑名单内部字段 %q（白名单投影被绕过）", w)
		}
	}
	_ = truck
}

// ===== TC-M9-07 正常：访问留日志 =====

// TC-M9-07 ★：写 access.log（模拟 reportd 追加）⇒ sync 后 b_share_access 有该行；
// ★ 再跑一次 sync 不产生重复行（幂等）；★ 未命中 token 计 skipped 不中断（A9 / §6-8）。
func TestTC_M9_07_Store_AccessLogSyncIdempotent(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)
	rep := m9Generate(t, st, batch.ID)

	ts := time.Now().Format(time.RFC3339Nano)
	writeAccessLog(t, []string{
		ts + "\t" + rep.Token + "\t198.51.100.7\t200\tMozilla/5.0\t",
		ts + "\t" + rep.Token + "\t198.51.100.7\t200\tMozilla/5.0\t",
		ts + "\tunknowntoken0000000000000000000000000\t198.51.100.9\t404\tcurl/8\t",
	})

	res, err := st.SyncReportAccess(ctx, m6Actor())
	if err != nil {
		t.Fatalf("同步访问日志失败: %v", err)
	}
	if res.Parsed != 3 {
		t.Fatalf("parsed 应 3，实际 %+v", res)
	}
	if res.Inserted != 1 {
		t.Fatalf("★★★ inserted 应 1（重复行与未知 token 不入库），实际 %+v", res)
	}
	if res.Skipped != 2 {
		t.Fatalf("skipped 应 2（重复 1 + 未知 token 1），实际 %+v", res)
	}

	rows, err := st.ListReportAccess(ctx, rep.ID)
	if err != nil {
		t.Fatalf("读访问日志失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("★★ 报告访问日志应 1 行，实际 %d", len(rows))
	}
	if rows[0].IP != "198.51.100.7" || rows[0].UA != "Mozilla/5.0" {
		t.Fatalf("访问日志字段错乱: %+v", rows[0])
	}
	if !rows[0].AccessedAt.Equal(time.Now()) && rows[0].AccessedAt.IsZero() {
		t.Fatalf("accessed_at 应落库: %+v", rows[0])
	}

	// ★ 再同步 ⇒ 不新增（幂等）
	res2, err := st.SyncReportAccess(ctx, m6Actor())
	if err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if res2.Inserted != 0 {
		t.Fatalf("★★★ 二次同步不得新增行，实际 %+v", res2)
	}
	rows2, _ := st.ListReportAccess(ctx, rep.ID)
	if len(rows2) != 1 {
		t.Fatalf("★★★ 幂等被破坏：访问日志变成 %d 行", len(rows2))
	}
}

// ===== TC-M9-08 ★ 让步口径：对外不披露、内部照旧标注 =====

// TC-M9-08 ★★：用让步接收料产出成品并出报告 ⇒ 快照不含「让步」/CONCESSION
// 任何变体，★ 同批内部档案 ConcessionUsed==true（两者同时成立，A8 / D20）。
func TestTC_M9_08_Store_ConcessionNotDisclosed(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()

	// 让步车次
	truck, bags := m6TruckWithBags(t, st, cust, inMat, 1)
	inspID := m6Inspect(t, st, truck.ID, bags[0].Code)
	if _, err := st.SetConclusion(ctx, ConclusionInput{
		InspectionID: inspID, Conclusion: InspConclusionCons,
		AuthorizedBy: "质量部长", CustNotifiedAt: "2026-10-02 10:00:00",
		CustContact: "采购部张工", CustChannel: "电话",
	}, m6Actor()); err != nil {
		t.Fatalf("出让步结论失败: %v", err)
	}

	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags[0].Code)
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{
		PackSpec: "吨袋", NetWeight: &tons{milli: 5000},
	}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	if _, err := st.GenerateFgBags(ctx, lot.ID, 1, m6Actor()); err != nil {
		t.Fatalf("生成成品袋失败: %v", err)
	}

	rep := m9Generate(t, st, batch.ID)
	html := m9Snapshot(t, rep)
	low := strings.ToLower(html)
	for _, w := range []string{"让步", "concession"} {
		if strings.Contains(low, w) {
			t.Fatalf("★★★ 对外快照命中禁用词 %q（D20：对外不披露）", w)
		}
	}

	// ★ 同批内部档案照旧显著标注（两者同时成立）
	arch, err := st.GetBatchArchive(ctx, batch.ID)
	if err != nil {
		t.Fatalf("读内部档案失败: %v", err)
	}
	if !arch.ConcessionUsed {
		t.Fatalf("★★★ 内部档案必须仍标 concession_used=true（D20 同时成立）")
	}
}

// ===== A11：取号超 999 明确报错 =====

// A11：当日序号已被 999 占满 ⇒ 生成明确报错（不进位、不吞）。
func TestReportSeqOverflowReported(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	ctx := context.Background()
	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)

	today := time.Now().Format("060102")
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO b_share_report (report_no, token, status, created_by)
VALUES (?, ?, '有效', ?)`, "RP"+today+"-999", "overflow-token-fixture-"+today, m6StoreOpenID); err != nil {
		t.Fatalf("造超限夹具失败: %v", err)
	}
	_, err := st.GenerateReport(ctx, GenerateReportInput{
		ScopeType: ReportScopeBatch, Scope: ReportScope{BatchID: batch.ID},
	}, m6Actor())
	if !errors.Is(err, ErrReportSeqOverflow) {
		t.Fatalf("★★★ 超 999 应 ErrReportSeqOverflow，实际 %v", err)
	}
}

// ===== A12：token 不可猜测 =====

// A12：连生成 5 次 ⇒ token 互不相同、长度 ≥32、不由 report_no / 序号派生。
func TestReportTokenUnpredictable(t *testing.T) {
	st, cust, inMat, outMat := m9Fixture(t)
	batch, _ := m9Chain(t, st, cust, inMat, outMat, false)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		rep := m9Generate(t, st, batch.ID)
		if len(rep.Token) < 32 {
			t.Fatalf("token 长度应 ≥32，实际 %d", len(rep.Token))
		}
		if seen[rep.Token] {
			t.Fatalf("★★★ token 不得重复")
		}
		if strings.Contains(rep.Token, rep.ReportNo) || strings.Contains(rep.Token, "-00") {
			t.Fatalf("★★ token 疑似由 report_no / 序号派生: %q / %q", rep.Token, rep.ReportNo)
		}
		seen[rep.Token] = true
	}
}

// ===== 纯单测（本机可跑，不依赖 DB）=====

// 快照渲染 + 禁用词闸：命中即拒绝生成（A8 的类型层第二道闸）。
func TestSnapshotGuardRejectsForbiddenWords(t *testing.T) {
	doc := &snapshotDoc{Title: "让步接收检验报告", ReportNo: "RP261009-001"}
	html, err := renderSnapshot(doc)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if err := guardSnapshot(html); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("★★★ 命中禁用词应被拒，实际 %v", err)
	}
	doc2 := &snapshotDoc{Title: "检验报告", ReportNo: "RP261009-001"}
	html2, err := renderSnapshot(doc2)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if err := guardSnapshot(html2); err != nil {
		t.Fatalf("正常内容不应被闸掉: %v", err)
	}
}

// expires_at 语义：到期日（含）= 该日 23:59:59.999；空 = 现在 + 30 天（同语义）。
func TestParseExpiresAtSemantics(t *testing.T) {
	now := time.Now()
	d, err := parseExpiresAt("2026-11-08", now)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if d.Format("2006-01-02 15:04:05.999") != "2026-11-08 23:59:59.999" {
		t.Fatalf("★★ 到期日应存该日 23:59:59.999，实际 %s", d.Format("2006-01-02 15:04:05.999"))
	}
	// 给了时间部分也按「到期日」归到当日末
	d2, err := parseExpiresAt("2026-11-08 10:30:00", now)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !d2.Equal(d) {
		t.Fatalf("★ 到期日语义应取日期部分，%s vs %s", d2, d)
	}
	// 空 = 现在 + 30 天的当日末
	d3, err := parseExpiresAt("", now)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := endOfDay(now.AddDate(0, 0, 30))
	if !d3.Equal(want) {
		t.Fatalf("★ 缺省应 = 生成时刻 + 30 天的 23:59:59.999，%s vs %s", d3, want)
	}
	// 非法输入 ⇒ 400
	if _, err := parseExpiresAt("banana", now); !errors.Is(err, ErrReportBadInput) {
		t.Fatalf("非法值应 ErrReportBadInput，实际 %v", err)
	}
}

// report_no / token 形态的辅助断言（纯单测）。
func TestReportNoPrefixShape(t *testing.T) {
	now := time.Now()
	got := fmt.Sprintf("RP%s-%03d", now.Format("060102"), 7)
	if !regexp.MustCompile(`^RP\d{6}-\d{3}$`).MatchString(got) {
		t.Fatalf("报告编号形态错: %q", got)
	}
	tok, err := newReportToken()
	if err != nil {
		t.Fatalf("生成 token 失败: %v", err)
	}
	if len(tok) < 32 {
		t.Fatalf("token 长度应 ≥32，实际 %d", len(tok))
	}
	if _, err := codec.Parse(tok); err == nil {
		t.Fatalf("token 不应是追踪码形态")
	}
}
