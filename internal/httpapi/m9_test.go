package httpapi

// ===== M9 报告分享 · 接口级 TC（TC-M9-01~08 双层形态的接口侧；库侧见 internal/store/m9_test.go）=====
//
// ★ 账号：主账号 = receiver+qc+production 并集（qc 对 report.* 与 rpt.* 均为 ALL）
//	⇒ 走生成 / 刷新 / 撤销 / 同步 / 读；★ 另绑一个**纯 receiver**（rpt.view=READ、
//	report.generate=NONE）专验「读入口挂 rpt.view」（A14 / §6-7）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 快照目录每例独立（t.Setenv JX_REPORT_DIR = t.TempDir()）。
// ★ reportd 的 200/404 与访问日志见 cmd/reportd/main_test.go；服务器 smoke 见回执。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m9HRecvOpenID  = "ou_test_m9_recv" // 纯 receiver：rpt.view=READ / report.generate=NONE
	m9HOtherCust   = "9435"
	m9HOtherCustNm = "M9接口别家客户"
	m9HRemark      = "M9接口内部备注不外传"
)

// m9HTTPWipe 清 M9 接口测试数据（★ 先子后父、精确匹配）。
func m9HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_share_access WHERE report_id IN
	       (SELECT id FROM b_share_report WHERE created_by IN (?, ?))`,
		m8HTTPOpenID, m8HMgmtOpenID)
	q(`DELETE FROM b_share_report WHERE created_by IN (?, ?)`, m8HTTPOpenID, m8HMgmtOpenID)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m9HOtherCust)
}

// m9HTTPEnv = M8 接口夹具（主数据 / 角色 / 链路 helper）+ M9 清场 + 独立快照目录。
func m9HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m9HTTPWipe(t, st)
	t.Cleanup(func() { m9HTTPWipe(t, st) })
	e, ck := m8HTTPEnv(t)
	t.Setenv("JX_REPORT_DIR", t.TempDir())
	bindRole(t, st, m9HRecvOpenID, "receiver")
	return e, ck
}

// m9HGenerate 生成报告（失败即 Fatal），返回 row。
func m9HGenerate(t *testing.T, e *env, ck string, batchID int64) map[string]interface{} {
	t.Helper()
	resp, body := e.do("POST", "/api/report/generate", ck, fmt.Sprintf(
		`{"scope_type":"按批次","scope":{"batch_id":%d},"title":"M9接口检验报告"}`, batchID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生成报告应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("生成报告未返回 row: %v", body)
	}
	return row
}

// m9HSnapshot 读快照 HTML（不存在即 Fatal）。
func m9HSnapshot(t *testing.T, rep map[string]interface{}) string {
	t.Helper()
	token := fmt.Sprint(rep["token"])
	b, err := os.ReadFile(filepath.Join(os.Getenv("JX_REPORT_DIR"), "served", token+".html"))
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if len(b) == 0 {
		t.Fatalf("★ 快照文件为空")
	}
	return string(b)
}

func m9HSnapshotExists(rep map[string]interface{}) bool {
	_, err := os.Stat(filepath.Join(os.Getenv("JX_REPORT_DIR"), "served",
		fmt.Sprint(rep["token"])+".html"))
	return err == nil
}

// m9HReportStatus 查库取状态。
func m9HReportStatus(t *testing.T, st *store.Store, id int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_share_report WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatalf("查报告状态失败: %v", err)
	}
	return s
}

// m9HChain 造链：合格车次 + 生产批（带内部 remark）+ 投料 + 作业段 + 成品 + 出货。
// 返回 (批 id, 批 row)。
func m9HChain(t *testing.T, e *env, ck string, cust, inMat, outMat int64) (int64, map[string]interface{}) {
	t.Helper()
	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m6HPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))
	resp, body := e.do("POST", "/api/prod/batches", ck, fmt.Sprintf(
		`{"customer_id":%d,"input_material_id":%d,"planned_output_material_id":%d,
		  "batch_date":"2026-10-01","remark":%q}`, cust, inMat, outMat, m9HRemark))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建生产批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	batch, _ := body["row"].(map[string]interface{})
	batchID := numOf(batch["id"])
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags[0]["code"]))
	if resp, body := e.do("POST", fmt.Sprintf("/api/prod/batches/%d/operations", batchID), ck,
		`{"operator":"内部操作员X","start_at":"2026-10-04 08:00:00","end_at":"2026-10-04 12:00:00","output_weight":1.5}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("记作业段应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	_, fgCodes := m8HMakeLot(t, e, ck, batchID, 1)
	ship := m8HShip(t, e, ck, fgCodes)
	// ★ 出场登记（ship_at 非空）—— 客户对账报表只计「已出场」的单（D5 口径）
	if resp, body := e.do("POST",
		fmt.Sprintf("/api/ship/shipments/%d/depart", numOf(ship["id"])), ck,
		`{"plate_no":"湘A·M9001","driver":"报告司机"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("出场登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	return batchID, batch
}

// ===== TC-M9-01 正常：生成报告 =====

// TC-M9-01：选批次经接口生成 ⇒ 行 + 快照 + 唯一链接；读入口 200；
// 非法 scope_type（按车次）/ 缺 batch_id ⇒ 400（A2 / A14）。
func TestTC_M9_01_HTTP_GenerateSnapshot(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)

	row := m9HGenerate(t, e, ck, batchID)
	if !regexp.MustCompile(`^RP\d{6}-\d{3}$`).MatchString(fmt.Sprint(row["report_no"])) {
		t.Fatalf("★★ report_no 形态错: %v", row["report_no"])
	}
	if fmt.Sprint(row["status"]) != store.ReportStatusActive {
		t.Fatalf("status 应「有效」，实际 %v", row["status"])
	}
	if len(fmt.Sprint(row["token"])) < 32 {
		t.Fatalf("★★ token 长度应 ≥32，实际 %v", row["token"])
	}
	if fmt.Sprint(row["url"]) == "" {
		t.Fatalf("★ 应返回唯一链接")
	}
	if !m9HSnapshotExists(row) {
		t.Fatalf("★★★ 快照应落 served/")
	}
	html := m9HSnapshot(t, row)
	if !strings.Contains(html, "检测结果") || !strings.Contains(html, "投料明细") {
		t.Fatalf("★ 快照应含追溯链与检测表板块")
	}

	// 读入口（rpt.view / LevelRead）
	id := numOf(row["id"])
	if resp, body := e.do("GET", "/api/report/list", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("报告列表应 200，实际 %d body=%v", resp.StatusCode, body)
	} else if cnt, _ := body["count"].(float64); int(cnt) < 1 {
		t.Fatalf("列表应含本报告，实际 %v", body)
	}
	if resp, body := e.do("GET", fmt.Sprintf("/api/report/%d", id), ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("报告详情应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// 非法入参 ⇒ 400
	if resp, body := e.do("POST", "/api/report/generate", ck,
		`{"scope_type":"按车次","scope":{"batch_id":1}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ scope_type=按车次 应 400（一期不做），实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", "/api/report/generate", ck,
		`{"scope_type":"按批次","scope":{}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 batch_id 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== TC-M9-02 ★ 正常：刷新 = 新 token 新链接 =====

func TestTC_M9_02_HTTP_RefreshNewToken(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	cust, inMat, outMat := m8HMasterIDs(t, st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)

	old := m9HGenerate(t, e, ck, batchID)
	oldID := numOf(old["id"])

	resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/refresh", oldID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("刷新应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	fresh, _ := body["row"].(map[string]interface{})
	if fresh == nil {
		t.Fatalf("刷新未返回 row: %v", body)
	}
	if fmt.Sprint(body["superseded_report_no"]) != fmt.Sprint(old["report_no"]) {
		t.Fatalf("★ superseded_report_no 应为旧编号，实际 %v", body["superseded_report_no"])
	}
	if fmt.Sprint(fresh["token"]) == fmt.Sprint(old["token"]) {
		t.Fatalf("★★★ 刷新必须新 token")
	}
	if fmt.Sprint(fresh["report_no"]) == fmt.Sprint(old["report_no"]) {
		t.Fatalf("★★ 刷新应取新编号")
	}
	if got := m9HReportStatus(t, st, oldID); got != store.ReportStatusRevoked {
		t.Fatalf("★★★ 旧行应「已撤销」，实际 %q", got)
	}
	if m9HSnapshotExists(old) {
		t.Fatalf("★★★ 旧快照必须移出 served/")
	}
	if !m9HSnapshotExists(fresh) {
		t.Fatalf("★★★ 新快照应在 served/")
	}
}

// ===== TC-M9-03 异常：撤销后不可访问 =====

func TestTC_M9_03_HTTP_RevokeMovesSnapshot(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	cust, inMat, outMat := m8HMasterIDs(t, st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)

	row := m9HGenerate(t, e, ck, batchID)
	id := numOf(row["id"])

	// 空 reason ⇒ 400（单步撤销，无发起 / 审批两段式）
	if resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/revoke", id), ck,
		`{"reason":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 reason 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/revoke", id), ck,
		`{"reason":"客户要求撤回"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("撤销应 200（单步生效），实际 %d body=%v", resp.StatusCode, body)
	}
	if got := m9HReportStatus(t, st, id); got != store.ReportStatusRevoked {
		t.Fatalf("★★★ 撤销后 status 应「已撤销」，实际 %q", got)
	}
	if m9HSnapshotExists(row) {
		t.Fatalf("★★★ 撤销后快照必须移出 served/（公网侧因此 404）")
	}
	// 再撤销 ⇒ 409
	if resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/revoke", id), ck,
		`{"reason":"再来"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复撤销应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== TC-M9-04 边界：有效期末日 =====

func TestTC_M9_04_HTTP_ExpiryBoundaryAndSweep(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	cust, inMat, outMat := m8HMasterIDs(t, st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)
	today := time.Now()

	tRow := m9HGenerate(t, e, ck, batchID)
	yRow := m9HGenerate(t, e, ck, batchID)
	if resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/expires", numOf(tRow["id"])), ck,
		fmt.Sprintf(`{"expires_at":%q}`, today.Format("2006-01-02"))); resp.StatusCode != http.StatusOK {
		t.Fatalf("设有效期应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", fmt.Sprintf("/api/report/%d/expires", numOf(yRow["id"])), ck,
		fmt.Sprintf(`{"expires_at":%q}`, today.AddDate(0, 0, -1).Format("2006-01-02"))); resp.StatusCode != http.StatusOK {
		t.Fatalf("设有效期应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	resp, body := e.do("POST", "/api/report/expire/sweep", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("清扫应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if swept, _ := body["swept"].(float64); int(swept) != 1 {
		t.Fatalf("★★ 应恰清扫 1 条，实际 %v", body)
	}
	nos, _ := body["report_nos"].([]interface{})
	if len(nos) != 1 || fmt.Sprint(nos[0]) != fmt.Sprint(yRow["report_no"]) {
		t.Fatalf("清扫编号应为昨日那条，实际 %v", body["report_nos"])
	}

	// 今日仍有效 + 快照在
	if got := m9HReportStatus(t, st, numOf(tRow["id"])); got != store.ReportStatusActive {
		t.Fatalf("★★★ 末日应仍「有效」，实际 %q", got)
	}
	if !m9HSnapshotExists(tRow) {
		t.Fatalf("★★★ 有效报告快照应在 served/")
	}
	// 昨日已过期 + 快照移出
	if got := m9HReportStatus(t, st, numOf(yRow["id"])); got != store.ReportStatusExpired {
		t.Fatalf("★★★ 过期后 status 应「已过期」，实际 %q", got)
	}
	if m9HSnapshotExists(yRow) {
		t.Fatalf("★★★ 过期报告快照必须移出 served/")
	}
	// 幂等
	resp, body = e.do("POST", "/api/report/expire/sweep", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("二次清扫应 200，实际 %d", resp.StatusCode)
	}
	if swept, _ := body["swept"].(float64); int(swept) != 0 {
		t.Fatalf("★★★ sweep 应幂等，第二次 swept=0，实际 %v", body)
	}
}

// ===== TC-M9-05 ★ 安全：页内不含其它客户数据 =====

func TestTC_M9_05_HTTP_NoOtherCustomerData(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	custA, inMat, outMat := m8HMasterIDs(t, st)

	// 另一家客户 + 其批
	if _, err := st.DB().ExecContext(context.Background(), `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, ?, '启用', 1, 1, ?)`, m9HOtherCust, m9HOtherCustNm, m8HTTPOpenID); err != nil {
		t.Fatalf("建别家客户失败: %v", err)
	}
	var custB int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m9HOtherCust).
		Scan(&custB); err != nil {
		t.Fatalf("查别家客户失败: %v", err)
	}
	batchAID, batchA := m9HChain(t, e, ck, custA, inMat, outMat)
	batchB := m6HBatch(t, e, ck, custB, inMat, outMat)

	row := m9HGenerate(t, e, ck, batchAID)
	html := m9HSnapshot(t, row)
	for _, w := range []string{
		m9HOtherCustNm,
		fmt.Sprint(batchB["human"]), fmt.Sprint(batchB["code"]),
	} {
		if w != "" && strings.Contains(html, w) {
			t.Fatalf("★★★ 快照命中他客户内容 %q", w)
		}
	}
	if !strings.Contains(html, fmt.Sprint(batchA["human"])) {
		t.Fatalf("快照应含本批人读行")
	}
}

// ===== TC-M9-06 ★ 架构：快照白名单 =====

func TestTC_M9_06_HTTP_SnapshotWhitelist(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)

	row := m9HGenerate(t, e, ck, batchID)
	html := m9HSnapshot(t, row)
	black := []string{
		m9HRemark,    // 生产批 remark
		"内部操作员X",     // 作业段 operator（★ 白名单只有班组）
		m8HTTPOpenID, // 操作痕迹 created_by
		"紧急放行",       // 紧急放行留痕
		"留样",         // 样品 / 留样
		"审计",         // 审计
	}
	for _, w := range black {
		if strings.Contains(html, w) {
			t.Fatalf("★★★ 快照命中黑名单内部字段 %q", w)
		}
	}
}

// ===== TC-M9-07 正常：访问留日志 =====

func TestTC_M9_07_HTTP_AccessLogSync(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	st := e.st
	cust, inMat, outMat := m8HMasterIDs(t, st)
	batchID, _ := m9HChain(t, e, ck, cust, inMat, outMat)

	row := m9HGenerate(t, e, ck, batchID)
	id := numOf(row["id"])
	token := fmt.Sprint(row["token"])
	ts := time.Now().Format(time.RFC3339Nano)

	// 模拟 reportd 追加（★ 不 rename，与 byte-offset 同步配套）
	logPath := filepath.Join(os.Getenv("JX_REPORT_DIR"), "access.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("打开访问日志失败: %v", err)
	}
	lines := []string{
		ts + "\t" + token + "\t203.0.113.5\t200\tMozilla/5.0",
		ts + "\t" + token + "\t203.0.113.5\t200\tMozilla/5.0",
		ts + "\tunknowntoken00000000000000000000000000\t203.0.113.6\t404\tcurl/8",
	}
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("写访问日志失败: %v", err)
		}
	}
	_ = f.Close()

	resp, body := e.do("POST", "/api/report/access/sync", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("同步应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if p, _ := body["parsed"].(float64); int(p) != 3 {
		t.Fatalf("parsed 应 3，实际 %v", body)
	}
	if ins, _ := body["inserted"].(float64); int(ins) != 1 {
		t.Fatalf("★★★ inserted 应 1，实际 %v", body)
	}
	if sk, _ := body["skipped"].(float64); int(sk) != 2 {
		t.Fatalf("skipped 应 2（重复 + 未知 token），实际 %v", body)
	}

	// 读入口有该行
	resp, body = e.do("GET", fmt.Sprintf("/api/report/%d/access", id), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读访问日志应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	rows, _ := body["rows"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("★★★ 访问日志应 1 行，实际 %d", len(rows))
	}
	r0, _ := rows[0].(map[string]interface{})
	if fmt.Sprint(r0["ip"]) != "203.0.113.5" || fmt.Sprint(r0["ua"]) != "Mozilla/5.0" {
		t.Fatalf("访问日志字段错乱: %+v", r0)
	}

	// 二次同步幂等
	resp, body = e.do("POST", "/api/report/access/sync", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("二次同步应 200，实际 %d", resp.StatusCode)
	}
	if ins, _ := body["inserted"].(float64); int(ins) != 0 {
		t.Fatalf("★★★ 二次同步不得新增，实际 %v", body)
	}
}

// ===== TC-M9-08 ★ 让步口径：对外不披露、内部照旧标注 =====

func TestTC_M9_08_HTTP_ConcessionNotDisclosed(t *testing.T) {
	e, ck := m9HTTPEnv(t)
	cust, inMat, outMat := m8HMasterIDs(t, e.st)

	truck, bags := m6HTruck(t, e, ck, cust, inMat, 1)
	m8HConcessionPass(t, e, ck, truck, fmt.Sprint(bags[0]["code"]))
	batch := m6HBatch(t, e, ck, cust, inMat, outMat)
	batchID := numOf(batch["id"])
	m6HFeed(t, e, ck, batchID, fmt.Sprint(bags[0]["code"]))
	_, fgCodes := m8HMakeLot(t, e, ck, batchID, 1)
	m8HShip(t, e, ck, fgCodes)

	row := m9HGenerate(t, e, ck, batchID)
	html := m9HSnapshot(t, row)
	low := strings.ToLower(html)
	for _, w := range []string{"让步", "concession"} {
		if strings.Contains(low, w) {
			t.Fatalf("★★★ 对外快照命中禁用词 %q（D20）", w)
		}
	}

	// ★ 内部档案照旧显著标注（两者同时成立）
	resp, arch := e.do("GET", fmt.Sprintf("/api/trace/batch/%d", batchID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读内部档案应 200，实际 %d", resp.StatusCode)
	}
	if arch["concession_used"] != true {
		t.Fatalf("★★★ 内部档案必须仍标 concession_used=true，实际 %v", arch["concession_used"])
	}
}

// ===== A14：读入口只挂 rpt.view ＋ 级别正确 =====

// A14 / §6-7：纯 receiver（rpt.view=READ、report.generate=NONE）⇒
// 读 200、生成 403、分享管理 403；qc（全 ALL）⇒ 全通；未登录 ⇒ 401。
func TestM9HTTPPermLevelsAndReadEntry(t *testing.T) {
	e, _ := m9HTTPEnv(t)
	st := e.st
	recv := e.login(m9HRecvOpenID)
	qc := e.login(m8HTTPOpenID)

	// 未登录 ⇒ 401
	if resp, _ := e.do("GET", "/api/report/list", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录读报告列表应 401，实际 %d", resp.StatusCode)
	}
	// 纯 receiver：读入口挂 rpt.view（含 READ）⇒ 200
	if resp, body := e.do("GET", "/api/report/list", recv, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("★★★ receiver 读报告列表应 200（读入口挂 rpt.view），实际 %d body=%v",
			resp.StatusCode, body)
	}
	// 纯 receiver：report.generate=NONE ⇒ 生成 403
	if resp, body := e.do("POST", "/api/report/generate", recv,
		`{"scope_type":"按批次","scope":{"batch_id":1}}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ receiver 生成应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	// 纯 receiver：report.share.manage=NONE ⇒ 撤销入口 403
	if resp, body := e.do("POST", "/api/report/expire/sweep", recv, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ receiver 清扫应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	// qc：report.* = ALL ⇒ 生成通（用不存在的批次 ⇒ 404 而非 403）
	if resp, body := e.do("POST", "/api/report/generate", qc,
		`{"scope_type":"按批次","scope":{"batch_id":99999999}}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("★★ qc 生成权限应放行（查无批次 ⇒ 404），实际 %d body=%v", resp.StatusCode, body)
	}
	// 权限摘要 4 点齐
	resp, body := e.do("GET", "/api/report/perm-summary", qc, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("权限摘要应 200，实际 %d", resp.StatusCode)
	}
	pts, _ := body["points"].(map[string]interface{})
	for _, code := range []string{"report.generate", "report.share.manage", "rpt.view", "rpt.export"} {
		v, ok := pts[code]
		if !ok || fmt.Sprint(v) == "NONE" {
			t.Fatalf("★★ 权限摘要应含 %s 且非 NONE，实际 %v", code, pts)
		}
	}
	_ = st
}
