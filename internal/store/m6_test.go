package store

// ===== M6 生产与谱系 · 库侧 TC（TC-M6-01~09 + A13/A14/A15/A16 关键不变量）=====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip，
//	在测试服务器上以 JX_TEST_DB=1 执行（scripts/run_tc_server.sh）。
// ★ 夹具纪律（§6-25）：清理按账号精确匹配、**先子表后父表**（M6 子表多：
//	feed_record / batch_operation / fg_bag / rework 都指向父表）；
//	断言「某表有几行」一律限定**本批作用域**（batch_id / created_by），不全库 COUNT(*)。
// ★ 接口级 9 条 TC 见 internal/httpapi/m6_test.go；本文件补库侧不变量。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

const (
	m6StoreOpenID = "ou_test_m6_store"
	m6CustCode    = "9405"
	m6InMatCode   = "9405" // 原料
	m6OutMatCode  = "9406" // 成品
	m6Date        = "2026-10-01"
	m6DateSeg     = "261001"
)

// m6Wipe 清掉 M6 库侧测试数据。
//
// ★ 顺序硬约束：**先子后父** —— rework / fg_bag / fg_lot / feed / operation / label
//
//	都要在 b_production_batch 之前删；取样 / 检测 / 作废记录要在袋与车之前删。
func m6Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := m6StoreOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_rework WHERE new_batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)
	       OR src_batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me, me)
	q(`DELETE FROM b_fg_bag WHERE fg_lot_id IN (SELECT id FROM b_fg_lot WHERE created_by = ?)`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM b_feed_record WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_batch_operation WHERE batch_id IN (SELECT id FROM b_production_batch WHERE created_by = ?)`, me)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)

	q(`DELETE FROM b_inspection_file WHERE inspection_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_inspection_result WHERE inspection_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_obj_void WHERE entity = 'b_inspection'
	     AND entity_id IN (SELECT id FROM b_inspection WHERE created_by = ?)`, me)
	q(`DELETE FROM b_inspection WHERE created_by = ?`, me)
	q(`DELETE FROM b_sample_destroy WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample_lend WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample_retention WHERE sample_id IN (SELECT id FROM b_sample WHERE created_by = ?)`, me)
	q(`DELETE FROM b_sample WHERE created_by = ?`, me)
	q(`DELETE FROM b_sample_group WHERE created_by = ?`, me)
	q(`DELETE FROM b_obj_void WHERE entity = 'b_bag'
	     AND entity_id IN (SELECT id FROM b_bag WHERE created_by = ?)`, me)
	q(`DELETE FROM b_bag WHERE created_by = ?`, me)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?`, me)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?`, me)

	q(`DELETE FROM s_audit_log WHERE actor_open_id = ?`, me)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?, ?)`,
		"ou_test_m6_store_init", "ou_test_m6_store_approve")
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m6CustCode)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m6InMatCode, m6OutMatCode)
}

// m6Fixture 清场 + 建测试主数据（客户 9405 / 原料 9405 / 成品 9406）。
func m6Fixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	m6Wipe(t, st)
	t.Cleanup(func() { m6Wipe(t, st) })
	ctx := context.Background()

	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M6测试客户', '启用', 1, 1, ?)`, m6CustCode, m6StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M6测试原料', '原料', '启用', 1, 1, ?)`, m6InMatCode, m6StoreOpenID)
	if err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	inMatID, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M6测试成品', '成品', '启用', 1, 1, ?)`, m6OutMatCode, m6StoreOpenID)
	if err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	outMatID, _ := res.LastInsertId()
	return st, custID, inMatID, outMatID
}

func m6Actor() MDActor {
	return MDActor{OpenID: m6StoreOpenID, Name: "m6-store", Role: "production", IP: "127.0.0.1"}
}

// m6Batch 建一个生产批（固定链根日期 m6Date，可重复执行 ⇒ 每轮从批序 01 起）。
func m6Batch(t *testing.T, st *Store, custID, inMatID, outMatID int64) ProductionBatch {
	t.Helper()
	b, err := st.CreateProductionBatch(context.Background(), CreateBatchInput{
		CustomerID: custID, InputMaterialID: inMatID, PlannedOutputMaterialID: outMatID,
		BatchDate: m6Date, Remark: "M6夹具",
	}, m6Actor())
	if err != nil {
		t.Fatalf("建生产批失败: %v", err)
	}
	return b
}

// m6TruckWithBags 走批 3 真实链路造车 + 袋（本批账号）。
func m6TruckWithBags(t *testing.T, st *Store, custID, inMatID int64, bags int) (TruckLot, []Bag) {
	t.Helper()
	ctx := context.Background()
	n, err := st.CreateNotice(ctx, NoticeInput{
		CustomerID: custID, MaterialID: inMatID, BizType: BizTypeCG,
		ArriveDate: m6Date, EstBagCount: bags, PlateNo: "湘F·M6",
	}, m6Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID}, m6Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	if _, err := st.WeighTruck(ctx, truck.ID, WeighInput{
		Gross: tons{milli: 30000 + int64(bags)}, Tare: tons{milli: 5000},
	}, m6Actor()); err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	bs, err := st.GenerateBags(ctx, truck.ID, bags, m6Actor())
	if err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	return truck, bs
}

// m6Inspect 给车次建「取样组 + 检测单」，返回单 id（无结论 ⇒ 待检）。
func m6Inspect(t *testing.T, st *Store, truckID int64, bagCode string) int64 {
	t.Helper()
	ctx := context.Background()
	take, err := st.TakeSample(ctx, TakeInput{Code: bagCode}, m6Actor())
	if err != nil {
		t.Fatalf("扫码取样失败: %v", err)
	}
	var incID int64
	for _, s := range take.Samples {
		if s.Role == SampleRoleIncremental {
			incID = s.ID
		}
	}
	if incID == 0 {
		t.Fatalf("未取到份样: %+v", take.Samples)
	}
	g, err := st.CreateSampleGroup(ctx, GroupInput{
		TargetType: TargetTruck, TargetID: truckID, SampleIDs: []int64{incID},
	}, m6Actor())
	if err != nil {
		t.Fatalf("建取样组失败: %v", err)
	}
	_ = g
	insp, err := st.CreateInspection(ctx, CreateInspInput{
		TargetType: TargetTruck, TargetID: truckID, Inspector: "检验员六",
	}, m6Actor())
	if err != nil {
		t.Fatalf("建检测单失败: %v", err)
	}
	return insp.ID
}

// m6Pass 造「合格车次」：取样 + 建单 + 出合格结论。
func m6Pass(t *testing.T, st *Store, custID, inMatID int64, bags int) (TruckLot, []Bag) {
	t.Helper()
	truck, bs := m6TruckWithBags(t, st, custID, inMatID, bags)
	inspID := m6Inspect(t, st, truck.ID, bs[0].Code)
	if _, err := st.SetConclusion(context.Background(), ConclusionInput{
		InspectionID: inspID, Conclusion: InspConclusionPass,
	}, m6Actor()); err != nil {
		t.Fatalf("出合格结论失败: %v", err)
	}
	return truck, bs
}

// m6Feed 记一次投料（失败即 Fatal）。
func m6Feed(t *testing.T, st *Store, batchID int64, bagCode string) FeedRecord {
	t.Helper()
	f, err := st.FeedScan(context.Background(), batchID, FeedInput{
		BagCode: bagCode, Operator: "投料工",
	}, m6Actor())
	if err != nil {
		t.Fatalf("投料失败（袋 %s）: %v", bagCode, err)
	}
	return f
}

// m6CountFeeds 数某批的投料行（★ 本批作用域，不全库 COUNT）。
func m6CountFeeds(t *testing.T, st *Store, batchID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_feed_record WHERE batch_id = ?`, batchID).Scan(&n); err != nil {
		t.Fatalf("统计投料记录失败: %v", err)
	}
	return n
}

// ===== TC-M6-01 正常：建生产批 =====

// TC-M6-01 正常：建批 ⇒ 码形如 1C-CG-9405-9406-261001-01-000-000-X（物料段 = 计划产出
// 成品物料；批序 scope = 客户 + 成品物料 + 创建日；状态 = 进行中）。
func TestTC_M6_01_Store_BatchCodeShape(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	b := m6Batch(t, st, cust, inMat, outMat)

	p, err := codec.Parse(b.Code)
	if err != nil {
		t.Fatalf("批码不可解析: %v", err)
	}
	seg := p.Seg
	if seg.T != "C" || seg.BT != "CG" {
		t.Fatalf("T/BT 应为 C/CG，实际 %s/%s", seg.T, seg.BT)
	}
	if seg.Customer != m6CustCode {
		t.Fatalf("客户段应 %s，实际 %s", m6CustCode, seg.Customer)
	}
	if seg.Material != m6OutMatCode {
		t.Fatalf("★★ 物料段必须是计划产出**成品**物料 %s，实际 %s", m6OutMatCode, seg.Material)
	}
	if seg.Date != m6DateSeg {
		t.Fatalf("日期段应为链根日期 %s，实际 %s", m6DateSeg, seg.Date)
	}
	if seg.SEQ1 != "01" || seg.SEQ2 != "000" || seg.SEQ3 != "000" {
		t.Fatalf("序段应 01/000/000，实际 %s/%s/%s", seg.SEQ1, seg.SEQ2, seg.SEQ3)
	}
	if b.Status != BatchStatusRunning {
		t.Fatalf("状态应「进行中」，实际 %q", b.Status)
	}
	wantHuman, _ := codec.ToHuman(b.Code)
	if b.Human != wantHuman {
		t.Fatalf("人读行应 %s，实际 %s", wantHuman, b.Human)
	}
	if b.BatchDate != m6Date {
		t.Fatalf("batch_date 应 %s，实际 %s", m6Date, b.BatchDate)
	}

	// 同日同客户同成品物料 ⇒ 批序递增（scope 校验）
	b2 := m6Batch(t, st, cust, inMat, outMat)
	p2, err := codec.Parse(b2.Code)
	if err != nil {
		t.Fatalf("第二个批码不可解析: %v", err)
	}
	if p2.Seg.SEQ1 != "02" {
		t.Fatalf("第二个批序应 02，实际 %s", p2.Seg.SEQ1)
	}
}

// ===== TC-M6-02 正常：扫 3 个吨袋码投料 =====

// TC-M6-02 ★ 正常：扫 3 个吨袋码 ⇒ b_feed_record **3 行**且 batch_id 指对该批
// （★ 断言按本批作用域计数，非全库 COUNT）；袋状态同步置「已投料」。
func TestTC_M6_02_Store_FeedThreeBags(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	_, bags := m6Pass(t, st, cust, inMat, 3)
	batch := m6Batch(t, st, cust, inMat, outMat)

	for _, b := range bags {
		m6Feed(t, st, batch.ID, b.Code)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 3 {
		t.Fatalf("★★ 本批 b_feed_record 应 3 行，实际 %d", n)
	}
	for _, b := range bags {
		var status string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT status FROM b_bag WHERE id = ?`, b.ID).Scan(&status); err != nil {
			t.Fatalf("读袋状态失败: %v", err)
		}
		if status != BagStatusFed {
			t.Fatalf("袋 #%d 状态应「%s」，实际 %q", b.ID, BagStatusFed, status)
		}
	}
	// 投料行必须指对该批（batch_id 正确）
	var wrong int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_feed_record WHERE batch_id <> ? AND bag_id IN (?,?,?)`,
		batch.ID, bags[0].ID, bags[1].ID, bags[2].ID).Scan(&wrong); err != nil {
		t.Fatalf("统计错挂投料行失败: %v", err)
	}
	if wrong != 0 {
		t.Fatalf("★ 不得有投料行错挂到别的批，实际 %d 行", wrong)
	}
	// 一袋只投一次：重复扫同一袋 ⇒ 拒绝
	if _, err := st.FeedScan(ctx, batch.ID, FeedInput{BagCode: bags[0].Code}, m6Actor()); !errors.Is(err, ErrFeedDup) {
		t.Fatalf("★ 重复投料应 ErrFeedDup，实际 %v", err)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 3 {
		t.Fatalf("被拒后投料行应仍为 3，实际 %d", n)
	}
}

// ===== TC-M6-03 ★ 异常：待检的袋投料 =====

// TC-M6-03 ★ 异常：扫「待检」（无结论）车次的袋投料 ⇒ 拒绝（409），
// 且库中**无新增** feed_record、袋状态不变。
func TestTC_M6_03_Store_FeedRejectedWithoutConclusion(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()

	// 车 A：完全未建检测单（待检）
	truckA, bagsA := m6TruckWithBags(t, st, cust, inMat, 1)
	_ = truckA
	// 车 B：已建单但**未出结论**
	truckB, bagsB := m6TruckWithBags(t, st, cust, inMat, 1)
	m6Inspect(t, st, truckB.ID, bagsB[0].Code)

	batch := m6Batch(t, st, cust, inMat, outMat)

	for _, tc := range []struct {
		name string
		code string
	}{{"无检测单", bagsA[0].Code}, {"有单未出结论", bagsB[0].Code}} {
		if _, err := st.FeedScan(ctx, batch.ID, FeedInput{BagCode: tc.code}, m6Actor()); !errors.Is(err, ErrFeedNotReleased) {
			t.Fatalf("★ %s 的袋投料应 ErrFeedNotReleased（409），实际 %v", tc.name, err)
		}
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 0 {
		t.Fatalf("★★ 被拒后 b_feed_record 应 0 行，实际 %d", n)
	}
	for _, id := range []int64{bagsA[0].ID, bagsB[0].ID} {
		var status string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT status FROM b_bag WHERE id = ?`, id).Scan(&status); err != nil {
			t.Fatalf("读袋状态失败: %v", err)
		}
		if status != BagStatusInStock {
			t.Fatalf("被拒后袋 #%d 状态应「在库」，实际 %q", id, status)
		}
	}
	// 「不合格」结论同样拒绝
	var bInspID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM b_inspection WHERE target_type = '车次' AND target_id = ? ORDER BY id DESC LIMIT 1`,
		truckB.ID).Scan(&bInspID); err != nil {
		t.Fatalf("查检测单失败: %v", err)
	}
	if _, err := st.SetConclusion(ctx, ConclusionInput{
		InspectionID: bInspID, Conclusion: InspConclusionFail,
	}, m6Actor()); err != nil {
		t.Fatalf("出不合格结论失败: %v", err)
	}
	if _, err := st.FeedScan(ctx, batch.ID, FeedInput{BagCode: bagsB[0].Code}, m6Actor()); !errors.Is(err, ErrFeedNotReleased) {
		t.Fatalf("★★ 不合格结论的袋投料应拒绝，实际 %v", err)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 0 {
		t.Fatalf("全程 b_feed_record 应 0 行，实际 %d", n)
	}
}

// ===== TC-M6-04 边界：紧急放行的料投料 =====

// TC-M6-04 边界：★ 生效的紧急放行（init + approve 两笔、两人）⇒ 允许且留痕；
// ★ 反例：只有 init 一笔 ⇒ **仍拒**。
func TestTC_M6_04_Store_FeedUrgentRelease(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	truck, bags := m6TruckWithBags(t, st, cust, inMat, 2)
	batch := m6Batch(t, st, cust, inMat, outMat)

	initiator := MDActor{OpenID: "ou_test_m6_store_init", Name: "生产发起", Role: "production", IP: "127.0.0.1"}
	approver := MDActor{OpenID: "ou_test_m6_store_approve", Name: "管理层审批", Role: "management", IP: "127.0.0.1"}

	// 只有 init ⇒ 不生效 ⇒ 拒
	if _, err := st.UrgentReleaseInit(ctx, "b_truck_lot", truck.ID, "料急先行放行", initiator); err != nil {
		t.Fatalf("发起紧急放行失败: %v", err)
	}
	if _, err := st.FeedScan(ctx, batch.ID, FeedInput{BagCode: bags[0].Code}, m6Actor()); !errors.Is(err, ErrFeedNotReleased) {
		t.Fatalf("★★ 只有 init 一笔必须仍拒（ErrFeedNotReleased），实际 %v", err)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 0 {
		t.Fatalf("只有 init 时投料行应 0，实际 %d", n)
	}

	// approve（另一人）⇒ 生效 ⇒ 允许
	if _, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck.ID, "同意", approver); err != nil {
		t.Fatalf("审批紧急放行失败: %v", err)
	}
	f := m6Feed(t, st, batch.ID, bags[0].Code)
	if f.TruckID != truck.ID {
		t.Fatalf("投料行的车次应 %d，实际 %d", truck.ID, f.TruckID)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 1 {
		t.Fatalf("生效后投料行应 1，实际 %d", n)
	}
	// 留痕：b_feed_record 有行 + 审计有 feed 动作
	var auditN int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM s_audit_log WHERE entity = 'b_feed_record'
		   AND action = 'feed' AND entity_id = ?`, f.ID).Scan(&auditN); err != nil {
		t.Fatalf("查投料审计失败: %v", err)
	}
	if auditN != 1 {
		t.Fatalf("投料审计应 1 笔，实际 %d", auditN)
	}
}

// ===== TC-M6-05 ★ 多对多：谱系 2×2 =====

// TC-M6-05 ★ 多对多：A 车料投给批 1、批 2；B 车料也投给批 2
// ⇒ 谱系表如实记录（★ 不得压成「批只挂一个袋」/「一车只挂一批」）。
func TestTC_M6_05_Store_GenealogyManyToMany(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()

	truckA, bagsA := m6Pass(t, st, cust, inMat, 2)
	truckB, bagsB := m6Pass(t, st, cust, inMat, 2)
	b1 := m6Batch(t, st, cust, inMat, outMat)
	b2 := m6Batch(t, st, cust, inMat, outMat)

	m6Feed(t, st, b1.ID, bagsA[0].Code) // A → 批 1
	m6Feed(t, st, b2.ID, bagsA[1].Code) // A → 批 2
	m6Feed(t, st, b2.ID, bagsB[0].Code) // B → 批 2

	if n := m6CountFeeds(t, st, b1.ID); n != 1 {
		t.Fatalf("批 1 投料行应 1，实际 %d", n)
	}
	// ★★ 批 2 必须有 2 行（来自 2 台车 / 2 个袋）—— 一车料可并入同一批
	if n := m6CountFeeds(t, st, b2.ID); n != 2 {
		t.Fatalf("★★ 批 2 投料行应 2（A、B 两车的袋），实际 %d", n)
	}
	// ★ 一车可拆多批：A 车的袋分投两个批
	var aBatches int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(DISTINCT f.batch_id)
  FROM b_feed_record f JOIN b_bag b ON b.id = f.bag_id
 WHERE b.truck_lot_id = ?`, truckA.ID).Scan(&aBatches); err != nil {
		t.Fatalf("统计 A 车去向失败: %v", err)
	}
	if aBatches != 2 {
		t.Fatalf("★★ A 车的料应去向 2 个批，实际 %d", aBatches)
	}
	// 反向谱系：袋 ⇒ 多条投料 / 多个批（结构上不限一条）
	g, err := st.GenealogyByBagCode(ctx, bagsA[0].Code)
	if err != nil {
		t.Fatalf("反查谱系失败: %v", err)
	}
	if g.BagID != bagsA[0].ID || len(g.Feeds) != 1 || g.Feeds[0].BatchID != b1.ID {
		t.Fatalf("反查谱系错乱: %+v", g)
	}
	_ = truckB
}

// ===== TC-M6-06 正常：跨班组作业段 =====

// TC-M6-06 正常：一个批记 2 段作业（两个班组）⇒ 2 条作业段、批仍为 1 个；
// seq 由服务端取号（1、2），uk_op_seq 生效。
func TestTC_M6_06_Store_TwoOperations(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	batch := m6Batch(t, st, cust, inMat, outMat)

	team1, team2 := int64(11), int64(22)
	op1, err := st.AddOperation(ctx, batch.ID, OperationInput{
		TeamID: &team1, Operator: "甲班", StartAt: "2026-10-01 08:00:00", EndAt: "2026-10-01 12:00:00",
		OutputWeight: &tons{milli: 1500}, Remark: "本段产出 1.5 吨",
	}, m6Actor())
	if err != nil {
		t.Fatalf("记第一段作业失败: %v", err)
	}
	op2, err := st.AddOperation(ctx, batch.ID, OperationInput{
		TeamID: &team2, Operator: "乙班", StartAt: "2026-10-01 13:00:00", EndAt: "2026-10-01 18:00:00",
	}, m6Actor())
	if err != nil {
		t.Fatalf("记第二段作业失败: %v", err)
	}
	if op1.Seq != 1 || op2.Seq != 2 {
		t.Fatalf("段序应 1、2，实际 %d、%d", op1.Seq, op2.Seq)
	}
	ops, err := st.ListOperations(ctx, batch.ID)
	if err != nil {
		t.Fatalf("查作业段失败: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("★★ 应 2 条作业段，实际 %d", len(ops))
	}
	var batchCount int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_production_batch WHERE created_by = ?`, m6StoreOpenID).
		Scan(&batchCount); err != nil {
		t.Fatalf("统计生产批失败: %v", err)
	}
	if batchCount != 1 {
		t.Fatalf("★★ 跨班组记多段，批仍应为 1 个，实际 %d", batchCount)
	}
	// output_weight 是本段产出（1.5 吨），不是整批产出
	if ops[0].OutputWeight == nil || *ops[0].OutputWeight != 1.5 {
		t.Fatalf("段 1 的 output_weight 应 1.5，实际 %v", ops[0].OutputWeight)
	}
}

// ===== TC-M6-07 ★ 正常：成品批码 =====

// TC-M6-07 ★ 正常：生产批 01 产出成品批 ⇒ 成品批码序1 = 来源生产批序、
// ★ 日期段 = 生产批 batch_date（不是生成当日）；成品袋 E 继承全部段位。
func TestTC_M6_07_Store_FgLotCodeSeq1AndDate(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	batch := m6Batch(t, st, cust, inMat, outMat)

	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{
		PackSpec: "吨袋", NetWeight: &tons{milli: 20000}, Remark: "M6产出",
	}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	p, err := codec.Parse(lot.Code)
	if err != nil {
		t.Fatalf("成品批码不可解析: %v", err)
	}
	bp, _ := codec.Parse(batch.Code)
	if p.Seg.T != "D" {
		t.Fatalf("T 应 D，实际 %s", p.Seg.T)
	}
	if p.Seg.SEQ1 != bp.Seg.SEQ1 {
		t.Fatalf("★★ 成品批序1 必须 = 来源生产批序 %s，实际 %s", bp.Seg.SEQ1, p.Seg.SEQ1)
	}
	if p.Seg.Date != m6DateSeg {
		t.Fatalf("★★ 日期段必须 = 生产批 batch_date（%s，链根日期），实际 %s", m6DateSeg, p.Seg.Date)
	}
	today := time.Now().Format("060102")
	if p.Seg.Date == today && m6DateSeg != today {
		t.Fatalf("★ 日期段不得取生成当日 %s", today)
	}
	if p.Seg.SEQ2 != "001" {
		t.Fatalf("批内第 1 个成品批的序2 应 001，实际 %s", p.Seg.SEQ2)
	}
	if p.Seg.Customer != m6CustCode || p.Seg.Material != m6OutMatCode {
		t.Fatalf("客户段/物料段应继承来源批，实际 %s/%s", p.Seg.Customer, p.Seg.Material)
	}
	// 第二个成品批 ⇒ 序2 递增
	lot2, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成第二个成品批失败: %v", err)
	}
	p2, _ := codec.Parse(lot2.Code)
	if p2.Seg.SEQ2 != "002" {
		t.Fatalf("第 2 个成品批序2 应 002，实际 %s", p2.Seg.SEQ2)
	}
	// 客户 / 物料显式传入但不一致 ⇒ 拒绝（§6-7）
	if _, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{OutputMaterialID: inMat}, m6Actor()); !errors.Is(err, ErrFgMismatch) {
		t.Fatalf("★ 物料不一致应 ErrFgMismatch，实际 %v", err)
	}
	_ = lot2

	// ★ 成品袋：段位全部继承（序1 = 批序、序2 = 成品批序、序3 = 袋序、日期 = 链根日期）
	bags, err := st.GenerateFgBags(ctx, lot.ID, 3, m6Actor())
	if err != nil {
		t.Fatalf("生成成品袋失败: %v", err)
	}
	if len(bags) != 3 {
		t.Fatalf("应生成 3 个成品袋，实际 %d", len(bags))
	}
	for i, b := range bags {
		ep, err := codec.Parse(b.Code)
		if err != nil {
			t.Fatalf("成品袋码不可解析: %v", err)
		}
		if ep.Seg.T != "E" || ep.Seg.SEQ1 != bp.Seg.SEQ1 || ep.Seg.SEQ2 != "001" ||
			ep.Seg.SEQ3 != fmt.Sprintf("%03d", i+1) || ep.Seg.Date != m6DateSeg {
			t.Fatalf("★ 成品袋码段位错乱（第 %d 袋）: %+v", i+1, ep.Seg)
		}
		if b.BagSeq != i+1 {
			t.Fatalf("bag_seq 应 %d，实际 %d", i+1, b.BagSeq)
		}
		if b.WeightAllocated == nil || *b.WeightAllocated != 6.667 {
			t.Fatalf("20 吨 ÷ 3 袋摊算应 6.667，实际 %v", b.WeightAllocated)
		}
	}
	// qty_bag = 实际生成袋数（D23：产出只在 b_fg_lot）
	after, err := st.GetFgLot(ctx, lot.ID)
	if err != nil {
		t.Fatalf("读成品批失败: %v", err)
	}
	if after.QtyBag != 3 {
		t.Fatalf("qty_bag 应为实际生成数 3，实际 %d", after.QtyBag)
	}
	if after.NetWeight == nil || *after.NetWeight != 20 {
		t.Fatalf("净重应 20，实际 %v", after.NetWeight)
	}
	// 打印留痕（A9）
	items, err := st.PrintFgBags(ctx, lot.ID, FgPrintInput{}, m6Actor())
	if err != nil {
		t.Fatalf("打印成品袋标签失败: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("应打印 3 张，实际 %d", len(items))
	}
	// 再打一次没走补打 ⇒ 拒；走补打缺原因 ⇒ 拒；带原因 ⇒ is_reprint=1
	if _, err := st.PrintFgBags(ctx, lot.ID, FgPrintInput{}, m6Actor()); !errors.Is(err, ErrReprintReason) {
		t.Fatalf("重复打印应 ErrReprintReason，实际 %v", err)
	}
	if _, err := st.PrintFgBags(ctx, lot.ID, FgPrintInput{BagSeqs: []int{1}}, m6Actor()); !errors.Is(err, ErrReprintReason) {
		t.Fatalf("补打缺原因应 ErrReprintReason，实际 %v", err)
	}
	re, err := st.PrintFgBags(ctx, lot.ID, FgPrintInput{BagSeqs: []int{1}, Reprint: true, Reason: "标签破损"}, m6Actor())
	if err != nil {
		t.Fatalf("补打失败: %v", err)
	}
	if len(re) != 1 || re[0].IsReprint != 1 || re[0].Reason != "标签破损" {
		t.Fatalf("补打应 is_reprint=1 且带原因，实际 %+v", re)
	}
}

// ===== TC-M6-08 正常：返工 =====

// TC-M6-08 正常：返工 ⇒ 新批 id ≠ 原批、code **独立取号**（非原批加后缀）、
// ★ 新批码日期段 = 当日；b_rework 1 行且 new/src 正确。
func TestTC_M6_08_Store_ReworkNewBatch(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	src := m6Batch(t, st, cust, inMat, outMat)

	if _, err := st.CreateRework(ctx, ReworkInput{SrcBatchID: src.ID}, m6Actor()); !errors.Is(err, ErrReworkReason) {
		t.Fatalf("★ 返工缺 reason 应 ErrReworkReason，实际 %v", err)
	}

	nu, err := st.CreateRework(ctx, ReworkInput{
		SrcBatchID: src.ID, Reason: "客户要求返工",
	}, m6Actor())
	if err != nil {
		t.Fatalf("返工失败: %v", err)
	}
	if nu.ID == src.ID {
		t.Fatalf("★★ 返工必须是**新批**，id 不得等于原批 %d", src.ID)
	}
	if nu.Code == src.Code || strings.HasPrefix(nu.Code, src.Code) {
		t.Fatalf("★★ 不得用原批加后缀：src=%s new=%s", src.Code, nu.Code)
	}
	np, err := codec.Parse(nu.Code)
	if err != nil {
		t.Fatalf("新批码不可解析: %v", err)
	}
	today := time.Now().Format("060102")
	if np.Seg.Date != today {
		t.Fatalf("★★ 新批链根日期 = 新建当日，日期段应 %s，实际 %s", today, np.Seg.Date)
	}
	if np.Seg.SEQ1 != "01" {
		t.Fatalf("新批应在当日 scope 内独立取号（01），实际 %s", np.Seg.SEQ1)
	}
	if nu.BatchDate != time.Now().Format("2006-01-02") {
		t.Fatalf("新批 batch_date 应为当日，实际 %s", nu.BatchDate)
	}
	if nu.CustomerID != src.CustomerID || nu.PlannedOutputMaterialID != src.PlannedOutputMaterialID {
		t.Fatalf("缺省应继承原批客户/物料，实际 %+v", nu)
	}

	var rwCount int
	var reason string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(reason), '') FROM b_rework
		  WHERE new_batch_id = ? AND src_batch_id = ?`, nu.ID, src.ID).
		Scan(&rwCount, &reason); err != nil {
		t.Fatalf("查返工关联失败: %v", err)
	}
	if rwCount != 1 || reason != "客户要求返工" {
		t.Fatalf("b_rework 应 1 行且 reason 正确，实际 count=%d reason=%q", rwCount, reason)
	}
	rows, err := st.ListReworks(ctx, src.ID)
	if err != nil {
		t.Fatalf("查返工列表失败: %v", err)
	}
	if len(rows) != 1 || rows[0].NewBatchID != nu.ID {
		t.Fatalf("返工列表错乱: %+v", rows)
	}
	// 原批已作废 ⇒ 拒
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE b_production_batch SET status = '已作废' WHERE id = ?`, src.ID); err != nil {
		t.Fatalf("置原批作废失败: %v", err)
	}
	if _, err := st.CreateRework(ctx, ReworkInput{SrcBatchID: src.ID, Reason: "再返"}, m6Actor()); !errors.Is(err, ErrReworkSrcState) {
		t.Fatalf("★ 原批已作废应 ErrReworkSrcState，实际 %v", err)
	}
}

// ===== TC-M6-09 ★ 变异向：谱系不得是「批只挂一个袋」 =====

// TC-M6-09 ★ 变异向（docs/04：把投料记录写成「批只挂一个袋」⇒ 用例报红）：
//
//	① 行为面：同一生产批必须能挂**多个袋**（且来自不同车）；
//	② 结构面：b_feed_record 的 batch_id **不得有唯一索引**（schema 冻结；
//	  若有人把谱系压成一对一，这里当场红）。
func TestTC_M6_09_Store_GenealogyNotSingleBag(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()

	truckA, bagsA := m6Pass(t, st, cust, inMat, 2)
	truckB, bagsB := m6Pass(t, st, cust, inMat, 1)
	b1 := m6Batch(t, st, cust, inMat, outMat)
	_ = truckA
	_ = truckB

	m6Feed(t, st, b1.ID, bagsA[0].Code)
	m6Feed(t, st, b1.ID, bagsA[1].Code) // 同批第 2 个袋
	m6Feed(t, st, b1.ID, bagsB[0].Code) // 同批来自**另一台车**的袋

	if n := m6CountFeeds(t, st, b1.ID); n != 3 {
		t.Fatalf("★★★ 「批只挂一个袋」是错的：同一批必须能挂 3 个袋，实际 %d 行", n)
	}
	var distinctBags, distinctTrucks int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(DISTINCT f.bag_id),
       COUNT(DISTINCT b.truck_lot_id)
  FROM b_feed_record f JOIN b_bag b ON b.id = f.bag_id
 WHERE f.batch_id = ?`, b1.ID).Scan(&distinctBags, &distinctTrucks); err != nil {
		t.Fatalf("统计谱系维度失败: %v", err)
	}
	if distinctBags != 3 || distinctTrucks != 2 {
		t.Fatalf("★★ 谱系应覆盖 3 个袋 / 2 台车，实际 %d / %d", distinctBags, distinctTrucks)
	}
	// 结构面：batch_id 上不得存在唯一索引（uk ⇒ 一对一 ⇒ 谱系被压扁）
	var uniq int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.STATISTICS
 WHERE table_schema = DATABASE() AND table_name = 'b_feed_record'
   AND column_name = 'batch_id' AND non_unique = 0`).Scan(&uniq); err != nil {
		t.Fatalf("查索引失败: %v", err)
	}
	if uniq != 0 {
		t.Fatalf("★★ b_feed_record.batch_id 不得有唯一索引（多对多），实际 %d 个", uniq)
	}
	_ = truckA
}

// ===== A13 · 投料记录更正 / 删除留痕 =====

// A13：更正 / 删除**必填 reason**、均写 s_audit_log（correct / delete，带 old_value 快照）；
// 更正只动 feed_weight / remark；删除后袋状态回置「在库」。
func TestFeedCorrectDeleteAudit(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	_, bags := m6Pass(t, st, cust, inMat, 1)
	batch := m6Batch(t, st, cust, inMat, outMat)
	f := m6Feed(t, st, batch.ID, bags[0].Code)

	// 缺 reason ⇒ 拒
	if _, err := st.CorrectFeed(ctx, f.ID, FeedCorrectInput{FeedWeight: &tons{milli: 2000}}, m6Actor()); !errors.Is(err, ErrFeedReason) {
		t.Fatalf("★ 更正缺 reason 应 ErrFeedReason，实际 %v", err)
	}
	if err := st.DeleteFeed(ctx, f.ID, "", m6Actor()); !errors.Is(err, ErrFeedReason) {
		t.Fatalf("★ 删除缺 reason 应 ErrFeedReason，实际 %v", err)
	}
	// 有 reason ⇒ 成功 + 审计（old_value 带 batch_id/bag_id 快照）
	nu := "2.5"
	fixed, err := st.CorrectFeed(ctx, f.ID, FeedCorrectInput{
		FeedWeight: &tons{milli: 2500}, Remark: &nu, Reason: "录错量",
	}, m6Actor())
	if err != nil {
		t.Fatalf("更正失败: %v", err)
	}
	if fixed.FeedWeight == nil || *fixed.FeedWeight != 2.5 || fixed.Remark != "2.5" {
		t.Fatalf("更正结果错乱: %+v", fixed)
	}
	if fixed.BatchID != f.BatchID || fixed.BagID != f.BagID {
		t.Fatalf("★★ 谱系关系（batch_id/bag_id）不得被改动：%d/%d → %d/%d",
			f.BatchID, f.BagID, fixed.BatchID, fixed.BagID)
	}
	var oldVal string
	if err := st.DB().QueryRowContext(ctx, `
SELECT COALESCE(old_value, '') FROM s_audit_log
 WHERE entity = 'b_feed_record' AND action = 'correct' AND entity_id = ?
 ORDER BY id DESC LIMIT 1`, f.ID).Scan(&oldVal); err != nil {
		t.Fatalf("查更正审计失败: %v", err)
	}
	if !strings.Contains(oldVal, fmt.Sprintf("batch_id=%d", f.BatchID)) ||
		!strings.Contains(oldVal, fmt.Sprintf("bag_id=%d", f.BagID)) {
		t.Fatalf("★ 更正审计 old_value 应含谱系快照，实际 %q", oldVal)
	}

	// 删除：物理删 + 袋回置在库 + 审计 delete（整行快照）
	if err := st.DeleteFeed(ctx, f.ID, "投错批", m6Actor()); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n := m6CountFeeds(t, st, batch.ID); n != 0 {
		t.Fatalf("删除后投料行应 0，实际 %d", n)
	}
	var status string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM b_bag WHERE id = ?`, bags[0].ID).Scan(&status); err != nil {
		t.Fatalf("读袋状态失败: %v", err)
	}
	if status != BagStatusInStock {
		t.Fatalf("★★ 删除后袋状态应回置「在库」，实际 %q", status)
	}
	var delOld string
	if err := st.DB().QueryRowContext(ctx, `
SELECT COALESCE(old_value, '') FROM s_audit_log
 WHERE entity = 'b_feed_record' AND action = 'delete' AND entity_id = ?
 ORDER BY id DESC LIMIT 1`, f.ID).Scan(&delOld); err != nil {
		t.Fatalf("查删除审计失败: %v", err)
	}
	if !strings.Contains(delOld, "batch_id=") || !strings.Contains(delOld, "bag_code=") {
		t.Fatalf("★ 删除审计 old_value 应含整行快照，实际 %q", delOld)
	}
	// 删除后袋可重新投料（回置生效）
	m6Feed(t, st, batch.ID, bags[0].Code)
	if n := m6CountFeeds(t, st, batch.ID); n != 1 {
		t.Fatalf("重投后应 1 行，实际 %d", n)
	}
}

// ===== A14 · 取号超限明确报错（不自动进位）=====

// A14：批序 > 99 / 成品批序 > 999 / 成品袋序 > 999 ⇒ 各自**明确报错**。
func TestSeqOverflowReported(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()

	// 批序：直接落一行 SEQ1=99 的同 scope 批码 ⇒ 下一次取号必须报错
	overflowCode, err := codec.Generate(codec.Segments{
		T: "C", BT: BizTypeCG, Customer: m6CustCode, Material: m6OutMatCode,
		Date: m6DateSeg, SEQ1: "99", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成超限批码失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`,
		overflowCode, cust, inMat, outMat, m6Date, m6StoreOpenID); err != nil {
		t.Fatalf("造超限批失败: %v", err)
	}
	if _, err := st.CreateProductionBatch(ctx, CreateBatchInput{
		CustomerID: cust, InputMaterialID: inMat, PlannedOutputMaterialID: outMat, BatchDate: m6Date,
	}, m6Actor()); !errors.Is(err, ErrBatchSeqOverflow) {
		t.Fatalf("★★ 批序 >99 应 ErrBatchSeqOverflow，实际 %v", err)
	}
	// 清掉超限行，继续测后面两条
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM b_production_batch WHERE code = ?`, overflowCode); err != nil {
		t.Fatalf("清超限批失败: %v", err)
	}

	batch := m6Batch(t, st, cust, inMat, outMat)
	fgOverflow, err := codec.Generate(codec.Segments{
		T: "D", BT: BizTypeCG, Customer: m6CustCode, Material: m6OutMatCode,
		Date: m6DateSeg, SEQ1: "01", SEQ2: "999", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成超限成品批码失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_fg_lot (code, batch_id, customer_id, output_material_id, status, created_by)
VALUES (?,?,?,?, '在库', ?)`, fgOverflow, batch.ID, cust, outMat, m6StoreOpenID); err != nil {
		t.Fatalf("造超限成品批失败: %v", err)
	}
	if _, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor()); !errors.Is(err, ErrFgSeqOverflow) {
		t.Fatalf("★★ 成品批序 >999 应 ErrFgSeqOverflow，实际 %v", err)
	}
	// 清掉超限行，继续测袋序
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM b_fg_lot WHERE code = ?`, fgOverflow); err != nil {
		t.Fatalf("清超限成品批失败: %v", err)
	}

	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	// 袋序：直接落 bag_seq=999 ⇒ 再生成 1 个必须报错
	bagOverflow, err := codec.Generate(codec.Segments{
		T: "E", BT: BizTypeCG, Customer: m6CustCode, Material: m6OutMatCode,
		Date: m6DateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "999",
	})
	if err != nil {
		t.Fatalf("生成超限成品袋码失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_fg_bag (code, fg_lot_id, bag_seq, status, created_by)
VALUES (?,?,999, '在库', ?)`, bagOverflow, lot.ID, m6StoreOpenID); err != nil {
		t.Fatalf("造超限成品袋失败: %v", err)
	}
	if _, err := st.GenerateFgBags(ctx, lot.ID, 1, m6Actor()); !errors.Is(err, ErrFgBagSeqOverflow) {
		t.Fatalf("★★ 成品袋序 >999 应 ErrFgBagSeqOverflow，实际 %v", err)
	}
}

// ===== A15 · 联动修正：退车排除已投料袋 =====

// A15（§6-24）：车次下存在**已投料袋** ⇒ 退车必须被拒（整批 UPDATE 不得绕过
// VoidBag 的「已投料不得作废」守卫）；M3 既有 TC 不涉及投料，保持绿。
func TestReturnTruckRejectsFedBag(t *testing.T) {
	st, cust, inMat, outMat := m6Fixture(t)
	ctx := context.Background()
	truck, bags := m6TruckWithBags(t, st, cust, inMat, 2)

	// 造检测单（无结论）⇒ 紧急放行生效 ⇒ 可投料
	inspID := m6Inspect(t, st, truck.ID, bags[0].Code)
	initiator := MDActor{OpenID: "ou_test_m6_store_init", Name: "发起", Role: "production", IP: "127.0.0.1"}
	approver := MDActor{OpenID: "ou_test_m6_store_approve", Name: "审批", Role: "management", IP: "127.0.0.1"}
	if _, err := st.UrgentReleaseInit(ctx, "b_truck_lot", truck.ID, "先行投料", initiator); err != nil {
		t.Fatalf("发起放行失败: %v", err)
	}
	if _, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck.ID, "同意", approver); err != nil {
		t.Fatalf("审批放行失败: %v", err)
	}

	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags[0].Code)

	// 造「退货」处置判定（ReturnTruck 的既有前置）
	if _, err := st.SetConclusion(ctx, ConclusionInput{
		InspectionID: inspID, Conclusion: InspConclusionFail,
	}, m6Actor()); err != nil {
		t.Fatalf("出不合格结论失败: %v", err)
	}
	if _, err := st.SetDisposition(ctx, inspID, "退货", m6Actor()); err != nil {
		t.Fatalf("填退货处置失败: %v", err)
	}

	// ★ 已有投料袋 ⇒ 拒绝退车
	if _, err := st.ReturnTruck(ctx, truck.ID, "想退车", m6Actor()); !errors.Is(err, ErrTruckFed) {
		t.Fatalf("★★★ 已投料车次退车必须被拒（ErrTruckFed），实际 %v", err)
	}
	// 袋未被整批作废
	var voided int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_bag WHERE truck_lot_id = ? AND status = '作废'`, truck.ID).Scan(&voided); err != nil {
		t.Fatalf("统计作废袋失败: %v", err)
	}
	if voided != 0 {
		t.Fatalf("★★ 被拒后不得作废任何袋，实际 %d", voided)
	}
	// 反证：把投料删掉 ⇒ 退车可以通过（守卫只拦已投料）
	var feedID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM b_feed_record WHERE batch_id = ? LIMIT 1`, batch.ID).Scan(&feedID); err != nil {
		t.Fatalf("查投料记录失败: %v", err)
	}
	if err := st.DeleteFeed(ctx, feedID, "退车前清理", m6Actor()); err != nil {
		t.Fatalf("删投料失败: %v", err)
	}
	if _, err := st.ReturnTruck(ctx, truck.ID, "无投料后退车", m6Actor()); err != nil {
		t.Fatalf("★★ 无投料时退车应放行，实际 %v", err)
	}
}

// ===== A16 · D23 边界：实际产出只在 b_fg_lot =====

// A16：b_production_batch **没有**产出类字段（产出物料 / 净重 / 袋数 / 产出时间 /
// 包装规格）—— 「两份真相」在结构上就不可能发生。
func TestD23BatchHasNoOutputColumns(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	for _, col := range []string{
		"net_weight", "qty_bag", "produced_at", "pack_spec", "output_weight",
	} {
		var n int
		if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.COLUMNS
 WHERE table_schema = DATABASE() AND table_name = 'b_production_batch' AND column_name = ?`,
			col).Scan(&n); err != nil {
			t.Fatalf("查列失败: %v", err)
		}
		if n != 0 {
			t.Fatalf("★★ D23：b_production_batch 不得有产出字段 %q（产出只在 b_fg_lot）", col)
		}
	}
	// 计划产出物料列存在（批号编排用），实际产出列在 b_fg_lot
	var n int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM information_schema.COLUMNS
 WHERE table_schema = DATABASE() AND table_name = 'b_fg_lot'
   AND column_name = 'output_material_id'`).Scan(&n); err != nil {
		t.Fatalf("查列失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("b_fg_lot 应有 output_material_id（实际产出唯一记录点）")
	}
}
