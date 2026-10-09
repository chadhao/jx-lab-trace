package store

// ===== M5 检测 · 库侧 TC（关键不变量：A18/A19/A20 与删项/修正守卫）=====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip，
//	在测试服务器上以 JX_TEST_DB=1 执行（scripts/run_tc_server.sh）。
// ★ 夹具纪律（§6-23）：清理按账号精确匹配、先子表后父表；断言限定本用例作用域。
// ★ 接口级 14 条 TC 见 internal/httpapi/m5_test.go；本文件补库侧不变量。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const (
	m5StoreOpenID = "ou_test_m5_store"
	m5SCustCode   = "9404"
	m5SMatCode    = "9404"
	m5SDate       = "2026-10-09"
	m5SDateSeg    = "261009"
)

// m5SWipe 清掉 M5 库侧测试数据（先子后父、按账号精确匹配）。
func m5SWipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := m5StoreOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
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
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)
	q(`DELETE FROM s_audit_log WHERE actor_open_id = ?`, me)
	q(`DELETE FROM m_test_item_limit WHERE created_by = ?`, me)
	q(`DELETE FROM m_test_item WHERE created_by = ?`, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m5SCustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m5SMatCode)
}

// m5SFixture 清场 + 建测试主数据（客户 9404 / 物料 9404）+ 检测项 + 判定限。
func m5SFixture(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	m5SWipe(t, st)
	t.Cleanup(func() { m5SWipe(t, st) })
	ctx := context.Background()

	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M5库侧测试客户', '启用', 1, 1, ?)`, m5SCustCode, m5StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M5库侧测试原料', '原料', '启用', 1, 1, ?)`, m5SMatCode, m5StoreOpenID)
	if err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	matID, _ := res.LastInsertId()

	for i := 1; i <= 3; i++ {
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_test_item (code, name, unit, method, value_type, status, version, is_current, created_by)
VALUES (?,?, '%','GB/T 2007','数值', '启用', 1, 1, ?)`,
			fmt.Sprintf("m5sti%02d", i), fmt.Sprintf("M5库侧项%d", i), m5StoreOpenID); err != nil {
			t.Fatalf("建检测项失败: %v", err)
		}
	}
	var item1 int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_test_item WHERE code = 'm5sti01' AND is_current = 1`).Scan(&item1); err != nil {
		t.Fatalf("查检测项失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_test_item_limit
  (test_item_id, customer_id, material_id, lower_limit, upper_limit, status, version, is_current, created_by)
VALUES (?,?,?,0,100,'启用',1,1,?)`, item1, custID, matID, m5StoreOpenID); err != nil {
		t.Fatalf("建判定限失败: %v", err)
	}
	return st, custID, matID
}

func m5SActor() MDActor {
	return MDActor{OpenID: m5StoreOpenID, Name: "m5-store", Role: "qc", IP: "127.0.0.1"}
}

// m5STruckWithBags 走批 3 真实链路造车 + 袋（同 m4 手法，用本批账号）。
func m5STruckWithBags(t *testing.T, st *Store, custID, matID int64, bags int) (TruckLot, []Bag) {
	t.Helper()
	ctx := context.Background()
	n, err := st.CreateNotice(ctx, NoticeInput{
		CustomerID: custID, MaterialID: matID, BizType: BizTypeCG,
		ArriveDate: m5SDate, EstBagCount: bags, PlateNo: "湘F·M5",
	}, m5SActor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID}, m5SActor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	if _, err := st.WeighTruck(ctx, truck.ID, WeighInput{
		Gross: tons{milli: 30000 + int64(bags)}, Tare: tons{milli: 5000},
	}, m5SActor()); err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	bs, err := st.GenerateBags(ctx, truck.ID, bags, m5SActor())
	if err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	return truck, bs
}

// m5SGroup 扫袋取份样 → 建取样组（大样），返回 group_id。
func m5SGroup(t *testing.T, st *Store, truckID int64, bagCode string) int64 {
	t.Helper()
	ctx := context.Background()
	take, err := st.TakeSample(ctx, TakeInput{Code: bagCode}, m5SActor())
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
	}, m5SActor())
	if err != nil {
		t.Fatalf("建取样组失败: %v", err)
	}
	return g.ID
}

// m5SInsp 造「车次 + 大样 + 检测单」，返回单 id。
func m5SInsp(t *testing.T, st *Store, custID, matID int64) (TruckLot, int64) {
	t.Helper()
	truck, bags := m5STruckWithBags(t, st, custID, matID, 2)
	m5SGroup(t, st, truck.ID, bags[0].Code)
	insp, err := st.CreateInspection(ctxBG(), CreateInspInput{
		TargetType: TargetTruck, TargetID: truck.ID, Inspector: "检验员五",
	}, m5SActor())
	if err != nil {
		t.Fatalf("建检测单失败: %v", err)
	}
	return truck, insp.ID
}

func ctxBG() context.Context { return context.Background() }

func m5SItemID(t *testing.T, st *Store, code string) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM m_test_item WHERE code = ? AND is_current = 1`, code).Scan(&id); err != nil {
		t.Fatalf("查检测项 %s 失败: %v", code, err)
	}
	return id
}

// m5SResults 归一化快照某单全部 result 行（供逐字比对）。
func m5SResults(t *testing.T, st *Store, inspID int64) string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `
SELECT id, item_id, state, IFNULL(CAST(value_num AS CHAR), ''), IFNULL(value_text, ''),
       IFNULL(judge, ''), IFNULL(CAST(lower_limit AS CHAR), ''), IFNULL(CAST(upper_limit AS CHAR), '')
  FROM b_inspection_result WHERE inspection_id = ? ORDER BY id`, inspID)
	if err != nil {
		t.Fatalf("读取清单快照失败: %v", err)
	}
	defer rows.Close()
	out := ""
	for rows.Next() {
		var id, itemID int64
		var state, vnum, vtext, judge, lo, hi string
		if err := rows.Scan(&id, &itemID, &state, &vnum, &vtext, &judge, &lo, &hi); err != nil {
			t.Fatalf("扫描清单快照失败: %v", err)
		}
		out += fmt.Sprintf("%d|%d|%s|%s|%s|%s|%s|%s\n", id, itemID, state, vnum, vtext, judge, lo, hi)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取清单快照失败: %v", err)
	}
	return out
}

// TC-M5-02 库侧：已录值项不可删；未测项可删且审计留痕（old_value 记快照）。
func TestTC_M5_02_Store_DeleteItemGuardAndAudit(t *testing.T) {
	st, cust, mat := m5SFixture(t)
	ctx := context.Background()
	_, inspID := m5SInsp(t, st, cust, mat)
	item1 := m5SItemID(t, st, "m5sti01")
	item2 := m5SItemID(t, st, "m5sti02")

	if _, err := st.AddInspectionItems(ctx, inspID, []int64{item1, item2}, m5SActor()); err != nil {
		t.Fatalf("加项失败: %v", err)
	}
	if _, err := st.RecordInspectionResult(ctx, RecordInspInput{
		InspectionID: inspID, ItemID: item1, State: InspStateDone, ValueNum: floatPtr(42),
	}, m5SActor()); err != nil {
		t.Fatalf("录入失败: %v", err)
	}

	// 已录值 ⇒ 拒绝
	if err := st.DeleteInspectionItem(ctx, inspID, item1, m5SActor()); !errors.Is(err, ErrInspResultRecorded) {
		t.Fatalf("★ 删已录值项应 ErrInspResultRecorded，实际 %v", err)
	}
	// 未测 ⇒ 允许 + 审计
	if err := st.DeleteInspectionItem(ctx, inspID, item2, m5SActor()); err != nil {
		t.Fatalf("删未测项应成功: %v", err)
	}
	rows, err := st.ListInspectionItems(ctx, inspID)
	if err != nil {
		t.Fatalf("查清单失败: %v", err)
	}
	if len(rows) != 1 || rows[0].ItemID != item1 {
		t.Fatalf("删后清单应只剩已录值项，实际 %+v", rows)
	}
	var oldVal string
	if err := st.DB().QueryRowContext(ctx, `
SELECT IFNULL(old_value, '') FROM s_audit_log
 WHERE entity = 'b_inspection_result' AND action = 'delete' AND actor_open_id = ?
 ORDER BY id DESC LIMIT 1`, m5StoreOpenID).Scan(&oldVal); err != nil {
		t.Fatalf("查删项审计失败: %v", err)
	}
	if !strings.Contains(oldVal, fmt.Sprintf("item=%d", item2)) || !strings.Contains(oldVal, "state=未测") {
		t.Fatalf("★ 删项审计 old_value 应含快照，实际 %q", oldVal)
	}
}

func floatPtr(v float64) *float64 { return &v }

// TC-M5-12 库侧：结果录入是唯一允许的原地写（守卫 + 受影响行数）；
// 修正走「作废 + 新开单」，原单 result 逐字不变。
func TestTC_M5_12_Store_GuardedUpdateAndVoidNew(t *testing.T) {
	st, cust, mat := m5SFixture(t)
	ctx := context.Background()
	_, inspID := m5SInsp(t, st, cust, mat)
	item1 := m5SItemID(t, st, "m5sti01")

	if _, err := st.AddInspectionItems(ctx, inspID, []int64{item1}, m5SActor()); err != nil {
		t.Fatalf("加项失败: %v", err)
	}
	if _, err := st.RecordInspectionResult(ctx, RecordInspInput{
		InspectionID: inspID, ItemID: item1, State: InspStateDone, ValueNum: floatPtr(42),
	}, m5SActor()); err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	before := m5SResults(t, st, inspID)

	// 二次录入 ⇒ 拒绝，原值不动
	if _, err := st.RecordInspectionResult(ctx, RecordInspInput{
		InspectionID: inspID, ItemID: item1, State: InspStateDone, ValueNum: floatPtr(99),
	}, m5SActor()); !errors.Is(err, ErrInspResultRecorded) {
		t.Fatalf("★ 二次录入应 ErrInspResultRecorded，实际 %v", err)
	}
	if after := m5SResults(t, st, inspID); after != before {
		t.Fatalf("★ 被拒后原值必须不变：\n%s\n---\n%s", before, after)
	}

	// 修正（缺原因 ⇒ 拒绝）
	if _, err := st.CorrectInspection(ctx, inspID, "", m5SActor()); !errors.Is(err, ErrInspVoidReason) {
		t.Fatalf("修正缺原因应 ErrInspVoidReason，实际 %v", err)
	}
	// 修正 ⇒ 作废 + 新开单
	nu, err := st.CorrectInspection(ctx, inspID, "录错值", m5SActor())
	if err != nil {
		t.Fatalf("修正失败: %v", err)
	}
	if nu.ID == inspID || nu.IsRecheck || nu.RecheckOf == nil || *nu.RecheckOf != inspID {
		t.Fatalf("修正新单应 is_recheck=false + recheck_of=原单，实际 %+v", nu)
	}
	var voidCount int
	var approvedBy string
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*), IFNULL(MAX(approved_by), '') FROM b_obj_void
 WHERE entity = 'b_inspection' AND entity_id = ?`, inspID).Scan(&voidCount, &approvedBy); err != nil {
		t.Fatalf("查作废行失败: %v", err)
	}
	if voidCount != 1 || approvedBy != "" {
		t.Fatalf("★ 修正应作废原单且 approved_by 留空，实际 count=%d approved=%q", voidCount, approvedBy)
	}
	if after := m5SResults(t, st, inspID); after != before {
		t.Fatalf("★★ 修正不得原地 UPDATE：\n%s\n---\n%s", before, after)
	}
	// 现行单 = 新单
	cur, err := currentInspectionIDTx(ctx, st.db, TargetTruck, nu.TargetID)
	if err != nil {
		t.Fatalf("查现行单失败: %v", err)
	}
	if !cur.Valid || cur.Int64 != nu.ID {
		t.Fatalf("现行单应为修正新单 #%d，实际 %+v", nu.ID, cur)
	}
}

// TC-M5-11 库侧：复检新开单（is_recheck=1），原单不作废、结果逐字不变。
func TestTC_M5_11_Store_RecheckKeepsOriginal(t *testing.T) {
	st, cust, mat := m5SFixture(t)
	ctx := context.Background()
	_, inspID := m5SInsp(t, st, cust, mat)
	item1 := m5SItemID(t, st, "m5sti01")

	if _, err := st.AddInspectionItems(ctx, inspID, []int64{item1}, m5SActor()); err != nil {
		t.Fatalf("加项失败: %v", err)
	}
	if _, err := st.RecordInspectionResult(ctx, RecordInspInput{
		InspectionID: inspID, ItemID: item1, State: InspStateDone, ValueNum: floatPtr(42),
	}, m5SActor()); err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	before := m5SResults(t, st, inspID)

	nu, err := st.RecheckInspection(ctx, inspID, m5SActor())
	if err != nil {
		t.Fatalf("复检失败: %v", err)
	}
	if !nu.IsRecheck || nu.RecheckOf == nil || *nu.RecheckOf != inspID {
		t.Fatalf("复检新单应 is_recheck=true + recheck_of=原单，实际 %+v", nu)
	}
	orig, err := st.GetInspection(ctx, inspID)
	if err != nil {
		t.Fatalf("读原单失败: %v", err)
	}
	if orig.Voided {
		t.Fatal("★ 复检不得作废原单")
	}
	if after := m5SResults(t, st, inspID); after != before {
		t.Fatalf("★★ 复检后原单结果必须逐字不变：\n%s\n---\n%s", before, after)
	}
	// 现行单 = 复检单
	cur, err := currentInspectionIDTx(ctx, st.db, TargetTruck, nu.TargetID)
	if err != nil {
		t.Fatalf("查现行单失败: %v", err)
	}
	if !cur.Valid || cur.Int64 != nu.ID {
		t.Fatalf("现行单应为复检单 #%d，实际 %+v", nu.ID, cur)
	}
}

// TC-M5-10 库侧 + A20：服务层同人校验（发起人自批 ⇒ 拒绝）；init/approve 各一笔审计且不同人。
func TestTC_M5_10_Store_UrgentServiceGuardAndAudit(t *testing.T) {
	st, cust, mat := m5SFixture(t)
	ctx := context.Background()
	truck, _ := m5STruckWithBags(t, st, cust, mat, 1)

	initiator := MDActor{OpenID: "ou_test_m5_store_qc", Name: "质检发起", Role: "qc", IP: "127.0.0.1"}
	approver := MDActor{OpenID: "ou_test_m5_store_mgmt", Name: "管理层审批", Role: "management", IP: "127.0.0.1"}
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx,
			`DELETE FROM s_audit_log WHERE actor_open_id IN (?, ?)`, initiator.OpenID, approver.OpenID)
	})

	if _, err := st.UrgentReleaseInit(ctx, "b_truck_lot", truck.ID, "料急先行放行", initiator); err != nil {
		t.Fatalf("发起应成功: %v", err)
	}
	// 重复发起 ⇒ 拒绝
	if _, err := st.UrgentReleaseInit(ctx, "b_truck_lot", truck.ID, "再来一次", initiator); !errors.Is(err, ErrUrgentAlready) {
		t.Fatalf("重复发起应 ErrUrgentAlready，实际 %v", err)
	}
	// ★ 未发起先审批（另一对象）⇒ ErrUrgentNotInit
	truck2, _ := m5STruckWithBags(t, st, cust, mat, 1)
	if _, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck2.ID, "", approver); !errors.Is(err, ErrUrgentNotInit) {
		t.Fatalf("未发起应 ErrUrgentNotInit，实际 %v", err)
	}
	// ★★ 发起人自批 ⇒ ErrUrgentSameActor（服务层 —— 不依赖权限层）
	if _, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck.ID, "", initiator); !errors.Is(err, ErrUrgentSameActor) {
		t.Fatalf("★★ 发起人自批应 ErrUrgentSameActor，实际 %v", err)
	}
	// 他人审批 ⇒ 成功
	stt, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck.ID, "同意", approver)
	if err != nil {
		t.Fatalf("他人审批应成功: %v", err)
	}
	if !stt.Initialized || !stt.Approved {
		t.Fatalf("放行态应已发起+已审批，实际 %+v", stt)
	}
	// A20：两笔审计各一笔、actor_open_id 不同
	rows, err := st.FindAuditByEntity(ctx, "b_truck_lot", truck.ID)
	if err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	var initBy, approveBy string
	for _, r := range rows {
		switch r.Action {
		case "urgent_release_init":
			initBy = r.ActorOpenID
		case "urgent_release_approve":
			approveBy = r.ActorOpenID
		}
	}
	if initBy != initiator.OpenID || approveBy != approver.OpenID || initBy == approveBy {
		t.Fatalf("★ 两笔审计应分属两人：init=%q approve=%q", initBy, approveBy)
	}
}

// TC-M5-18（A18）★ 联动修正：已作废单的旧「退货」判定不得复活 ——
//
//	造「已作废单 disposition=退货 + 现行单 conclusion=合格」⇒ 退车必须被拒。
func TestTC_M5_18_Store_ReturnTruckExcludesVoidedInspection(t *testing.T) {
	st, cust, mat := m5SFixture(t)
	ctx := context.Background()
	truck, origID := m5SInsp(t, st, cust, mat)

	// 原单：不合格 + 处置退货（同步车次状态「已退货」）
	orig, err := st.GetInspection(ctx, origID)
	if err != nil {
		t.Fatalf("读原单失败: %v", err)
	}
	if _, err := st.SetConclusion(ctx, ConclusionInput{
		InspectionID: orig.ID, Conclusion: InspConclusionFail,
	}, m5SActor()); err != nil {
		t.Fatalf("出不合格结论失败: %v", err)
	}
	if _, err := st.SetDisposition(ctx, orig.ID, "退货", m5SActor()); err != nil {
		t.Fatalf("填退货处置失败: %v", err)
	}

	// 修正：原单作废，新开单出「合格」结论（现行单不再有退货判定）
	nu, err := st.CorrectInspection(ctx, orig.ID, "误判，改判合格", m5SActor())
	if err != nil {
		t.Fatalf("修正失败: %v", err)
	}
	if _, err := st.SetConclusion(ctx, ConclusionInput{
		InspectionID: nu.ID, Conclusion: InspConclusionPass,
	}, m5SActor()); err != nil {
		t.Fatalf("新单出结论失败: %v", err)
	}

	// ★ 退车必须被拒（只认现行单 —— 已作废单的 退货 不算）
	if _, err := st.ReturnTruck(ctx, truck.ID, "试图借已作废单退车", m5SActor()); !errors.Is(err, ErrNoReturnDecision) {
		t.Fatalf("★★ 已作废单的退货判定不得复活，应 ErrNoReturnDecision，实际 %v", err)
	}
}
