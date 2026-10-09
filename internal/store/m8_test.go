package store

// ===== M8 追溯 · 库侧 TC（TC-M8-01~05 + A9/A10/A11/A12/A13 关键不变量）=====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 上游链路复用 M6 夹具（真实链：车 → 取样 → 检测 → 投料 → 成品），
//	出货环节用 M7 实现（m7Actor 记账 ⇒ 由 m7Wipe 清理，先于 m6Wipe）。
// ★ 反向「当时检测结果」= 现行单 + is_current 标注（§6-9）；让步判据 =
//	现行单 conclusion='CONCESSION'（§6-11）。接口级 TC 见 internal/httpapi/m8_test.go。

import (
	"context"
	"errors"
	"testing"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// m8Fixture = M6 链路夹具 + M7 出货清场（出货行由 m7Actor 记账）。
func m8Fixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	st, cust, inMat, outMat := m6Fixture(t)
	m7Wipe(t, st)
	t.Cleanup(func() { m7Wipe(t, st) })
	return st, cust, inMat, outMat
}

// m8Ship 建一张出货单并（可选）出场登记，返回单 id。
func m8Ship(t *testing.T, st *Store, bagCodes []string, depart bool) int64 {
	t.Helper()
	ctx := context.Background()
	sh, err := st.CreateShipment(ctx, CreateShipmentInput{BagCodes: bagCodes}, m7Actor())
	if err != nil {
		t.Fatalf("建出货单失败: %v", err)
	}
	if depart {
		if _, err := st.DepartShipment(ctx, sh.ID, DepartInput{
			PlateNo: "湘A·M8001", Driver: "追八",
		}, m7Actor()); err != nil {
			t.Fatalf("出场登记失败: %v", err)
		}
	}
	return sh.ID
}

// ===== TC-M8-01 正常：正向追溯 =====

// TC-M8-01 正常：A 车正向追溯 ⇒ 生产批 + 成品批 + 出货单齐（has_flow=true）；
// ★ 链路经 b_feed_record（吨袋码入口同样命中该链）。
func TestTC_M8_01_Store_ForwardTrace(t *testing.T) {
	st, cust, inMat, outMat := m8Fixture(t)
	ctx := context.Background()

	truck, bags := m6Pass(t, st, cust, inMat, 2)
	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags[0].Code)
	m6Feed(t, st, batch.ID, bags[1].Code)
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
	shipID := m8Ship(t, st, []string{fgBags[0].Code, fgBags[1].Code}, true)

	// 车次入口
	fwd, err := st.TraceForward(ctx, truck.ID, "")
	if err != nil {
		t.Fatalf("正向追溯失败: %v", err)
	}
	if !fwd.HasFlow {
		t.Fatalf("★★★ A 车有投料，has_flow 应 true")
	}
	if len(fwd.Batches) != 1 || fwd.Batches[0].BatchID != batch.ID {
		t.Fatalf("★★ 正向生产批应 [%d]，实际 %+v", batch.ID, fwd.Batches)
	}
	lotOK := false
	for _, f := range fwd.FgLots {
		if f.FgLotID == lot.ID {
			lotOK = true
		}
	}
	if !lotOK {
		t.Fatalf("★★ 正向应含成品批 %d，实际 %+v", lot.ID, fwd.FgLots)
	}
	shipOK := false
	for _, s := range fwd.Shipments {
		if s.ShipmentID == shipID {
			shipOK = true
			if s.Status != ShipStatusOut {
				t.Fatalf("出货单状态应「已出厂」，实际 %q", s.Status)
			}
		}
	}
	if !shipOK {
		t.Fatalf("★★ 正向应含出货单 %d，实际 %+v", shipID, fwd.Shipments)
	}
	if fwd.Source.TruckLotID != truck.ID {
		t.Fatalf("source 应为 A 车 %d，实际 %+v", truck.ID, fwd.Source)
	}

	// 吨袋码入口（同一条 b_feed_record 链）
	fwd2, err := st.TraceForward(ctx, 0, bags[0].Code)
	if err != nil {
		t.Fatalf("按袋码正向追溯失败: %v", err)
	}
	if !fwd2.HasFlow || len(fwd2.Batches) != 1 || fwd2.Batches[0].BatchID != batch.ID {
		t.Fatalf("★★ 袋码入口应命中同一生产批，实际 %+v", fwd2)
	}
}

// ===== TC-M8-02 ★★ 一致性：正反互为逆 =====

// TC-M8-02 ★★：对 TC-M8-01 结果中的成品批**反向追溯** ⇒ 能追回 A 车（A→B→A 回环）；
// 反向给出的 batch_id / truck_lot_id 与正向集合互为逆（两向同走 b_feed_record）。
func TestTC_M8_02_Store_BackwardInverse(t *testing.T) {
	st, cust, inMat, outMat := m8Fixture(t)
	ctx := context.Background()

	truckA, bagsA := m6Pass(t, st, cust, inMat, 2)
	truckB, bagsB := m6Pass(t, st, cust, inMat, 1)
	batch1 := m6Batch(t, st, cust, inMat, outMat)
	batch2 := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch1.ID, bagsA[0].Code) // A → 批 1
	m6Feed(t, st, batch2.ID, bagsA[1].Code) // A → 批 2（一车拆多批）
	m6Feed(t, st, batch2.ID, bagsB[0].Code) // B → 批 2
	lot1, err := st.CreateFgLot(ctx, batch1.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	if _, err := st.CreateFgLot(ctx, batch2.ID, CreateFgLotInput{}, m6Actor()); err != nil {
		t.Fatalf("生成成品批 2 失败: %v", err)
	}

	// A → 正向
	fwd, err := st.TraceForward(ctx, truckA.ID, "")
	if err != nil {
		t.Fatalf("正向追溯失败: %v", err)
	}
	if !fwd.HasFlow || len(fwd.Batches) != 2 {
		t.Fatalf("★★ A 车正向应 2 个生产批，实际 %+v", fwd.Batches)
	}
	for _, s := range []int64{batch1.ID, batch2.ID} {
		found := false
		for _, b := range fwd.Batches {
			if b.BatchID == s {
				found = true
			}
		}
		if !found {
			t.Fatalf("★★ 正向缺生产批 %d", s)
		}
	}

	// → B：对正向结果中的成品批反向
	for _, f := range fwd.FgLots {
		back, err := st.TraceBackward(ctx, f.FgLotID, "")
		if err != nil {
			t.Fatalf("反向追溯失败: %v", err)
		}
		if !back.HasFlow {
			t.Fatalf("★★★ 反向应有流向（has_flow）")
		}
		// A → B → A：反向的投料车次必须能追回 A 车
		trucks := map[int64]bool{}
		for _, fd := range back.Feeds {
			trucks[fd.TruckLotID] = true
		}
		if !trucks[truckA.ID] {
			t.Fatalf("★★★ 正反互为逆被破坏：成品批 %d 反向追不回 A 车 %d（feeds=%+v）",
				f.FgLotID, truckA.ID, back.Feeds)
		}
		// 反向批集合 ⊆ 正向批集合（同一张 b_feed_record）
		if len(back.Batches) != 1 {
			t.Fatalf("反向应 1 个来源批，实际 %+v", back.Batches)
		}
		inFwd := false
		for _, b := range fwd.Batches {
			if b.BatchID == back.Batches[0].BatchID {
				inFwd = true
			}
		}
		if !inFwd {
			t.Fatalf("★★ 反向批 %d 不在正向集合内", back.Batches[0].BatchID)
		}
	}
	if len(fwd.FgLots) == 0 {
		t.Fatalf("正向应有成品批")
	}
	// 反向按 fg_code 入口同样命中
	back2, err := st.TraceBackward(ctx, 0, fwd.FgLots[0].Code)
	if err != nil {
		t.Fatalf("按 fg_code 反向失败: %v", err)
	}
	if !back2.HasFlow {
		t.Fatalf("fg_code 入口应有流向")
	}
	_ = lot1
	_ = truckB
}

// ===== TC-M8-03 边界：未被投料的车 =====

// TC-M8-03 ★：未投料的车 / 不存在的车 / 未投料的袋 ⇒ **不报错**，
// 返回 has_flow:false + 三个空数组（§6-12：200，不是 404/500）。
func TestTC_M8_03_Store_ForwardNoFlow(t *testing.T) {
	st, cust, inMat, _ := m8Fixture(t)
	ctx := context.Background()

	truck, bags := m6TruckWithBags(t, st, cust, inMat, 1) // 未投料
	cases := []struct {
		name string
		id   int64
		code string
	}{
		{"未投料的车", truck.ID, ""},
		{"未投料的袋", 0, bags[0].Code},
		{"不存在的车", 99999999, ""},
	}
	for _, tc := range cases {
		fwd, err := st.TraceForward(ctx, tc.id, tc.code)
		if err != nil {
			t.Fatalf("★★★ %s：无流向不得报错（应 200），实际 %v", tc.name, err)
		}
		if fwd.HasFlow {
			t.Fatalf("★★★ %s：has_flow 应 false", tc.name)
		}
		if len(fwd.Batches) != 0 || len(fwd.FgLots) != 0 || len(fwd.Shipments) != 0 {
			t.Fatalf("★★★ %s：三个数组应全空，实际 %+v", tc.name, fwd)
		}
	}
}

// ===== TC-M8-04 正常：反向追溯含检测结果 =====

// TC-M8-04 正常：反向每条投料带车次**现行检测单** —— 单号 + 结论 + 数值 +
// ★ is_current=true 标注（§6-9 如实声明非历史快照）；
// 无现行单但**生效紧急放行**投料的车次 ⇒ urgent_release=true + 两笔留痕（§6-10）。
func TestTC_M8_04_Store_BackwardInspection(t *testing.T) {
	st, cust, inMat, outMat := m8Fixture(t)
	ctx := context.Background()

	// 车 1：合格结论 + 一条检测数值（直插夹具结果行）
	truck1, bags1 := m6Pass(t, st, cust, inMat, 1)
	var inspID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM b_inspection WHERE target_type = '车次' AND target_id = ?
		 ORDER BY id DESC LIMIT 1`, truck1.ID).Scan(&inspID); err != nil {
		t.Fatalf("查现行检测单失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_inspection_result (inspection_id, item_id, state, value_num, unit, judge, created_by)
VALUES (?,?, '已测', 12.345, '%', '合格', ?)`, inspID, 999999, m6StoreOpenID); err != nil {
		t.Fatalf("造检测数值失败: %v", err)
	}

	// 车 2：无结论，凭生效紧急放行投料
	truck2, bags2 := m6TruckWithBags(t, st, cust, inMat, 1)
	initiator := MDActor{OpenID: m7StoreInitID, Name: "m8-发起", Role: "qc", IP: "127.0.0.1"}
	approver := MDActor{OpenID: m7StoreApprID, Name: "m8-审批", Role: "management", IP: "127.0.0.1"}
	if _, err := st.UrgentReleaseInit(ctx, "b_truck_lot", truck2.ID, "料急", initiator); err != nil {
		t.Fatalf("发起紧急放行失败: %v", err)
	}
	if _, err := st.UrgentReleaseApprove(ctx, "b_truck_lot", truck2.ID, "同意", approver); err != nil {
		t.Fatalf("审批紧急放行失败: %v", err)
	}

	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags1[0].Code)
	m6Feed(t, st, batch.ID, bags2[0].Code)
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}

	back, err := st.TraceBackward(ctx, lot.ID, "")
	if err != nil {
		t.Fatalf("反向追溯失败: %v", err)
	}
	if len(back.Feeds) != 2 {
		t.Fatalf("反向投料应 2 条，实际 %d", len(back.Feeds))
	}

	// 车 1：现行单 + 数值 + is_current
	f0 := back.Feeds[0]
	if f0.TruckLotID != truck1.ID {
		t.Fatalf("投料顺序错乱: %+v", back.Feeds)
	}
	if f0.Inspection == nil {
		t.Fatalf("★★★ 投料必须带车次检测结果")
	}
	if !f0.Inspection.IsCurrent {
		t.Fatalf("★★★ 必须显式标注 is_current=true（现行单，非历史快照）")
	}
	if f0.Inspection.Conclusion != InspConclusionPass {
		t.Fatalf("结论应「合格」，实际 %q", f0.Inspection.Conclusion)
	}
	if f0.Inspection.InspectionNo == "" {
		t.Fatalf("★★ 检测单号应非空")
	}
	var hasVal bool
	for _, r := range f0.Inspection.Results {
		if r.ValueNum != nil && *r.ValueNum > 12.34 && *r.ValueNum < 12.36 {
			hasVal = true
		}
	}
	if !hasVal {
		t.Fatalf("★★★ 检测数值应一并列出，实际 %+v", f0.Inspection.Results)
	}
	if f0.Inspection.UrgentRelease {
		t.Fatalf("★ 合格车次不应标紧急放行")
	}

	// 车 2：无现行单，凭生效紧急放行 ⇒ urgent_release + 两笔留痕
	f1 := back.Feeds[1]
	if f1.TruckLotID != truck2.ID {
		t.Fatalf("投料顺序错乱: %+v", back.Feeds)
	}
	if f1.Inspection == nil {
		t.Fatalf("★★ 紧急放行车次也应带 inspection 对象")
	}
	if !f1.Inspection.UrgentRelease {
		t.Fatalf("★★★ 生效的紧急放行应标 urgent_release=true")
	}
	if len(f1.Inspection.UrgentRecords) != 2 {
		t.Fatalf("★★ 紧急放行应附 init/approve 两笔留痕，实际 %d", len(f1.Inspection.UrgentRecords))
	}
	acts := map[string]bool{}
	for _, r := range f1.Inspection.UrgentRecords {
		acts[r.Action] = true
	}
	if !acts["urgent_release_init"] || !acts["urgent_release_approve"] {
		t.Fatalf("★ 留痕动作应含 init+approve，实际 %+v", f1.Inspection.UrgentRecords)
	}
}

// ===== TC-M8-05 边界：让步接收料产出的成品查档案 =====

// TC-M8-05 + A13 边界：投料链上有 CONCESSION 现行单的车次 ⇒ 档案
// concession_used=true 且列出来源车次；无让步 ⇒ false。
// 同时验档案六块齐（批 / 投料 / 作业段 / 成品批+袋 / 出货单 / 标注）。
func TestTC_M8_05_Store_BatchArchiveConcession(t *testing.T) {
	st, cust, inMat, outMat := m8Fixture(t)
	ctx := context.Background()

	// 让步车次：取样 + 建单 + CONCESSION 结论（四字段必填）
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
	team := int64(88)
	if _, err := st.AddOperation(ctx, batch.ID, OperationInput{
		TeamID: &team, Operator: "甲班",
		StartAt: "2026-10-02 08:00:00", EndAt: "2026-10-02 12:00:00",
		OutputWeight: &tons{milli: 5000},
	}, m6Actor()); err != nil {
		t.Fatalf("记作业段失败: %v", err)
	}
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{
		PackSpec: "吨袋", NetWeight: &tons{milli: 5000},
	}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}
	fgBags, err := st.GenerateFgBags(ctx, lot.ID, 2, m6Actor())
	if err != nil {
		t.Fatalf("生成成品袋失败: %v", err)
	}
	shipID := m8Ship(t, st, []string{fgBags[0].Code}, false)

	arch, err := st.GetBatchArchive(ctx, batch.ID)
	if err != nil {
		t.Fatalf("读批次档案失败: %v", err)
	}

	// ★ 让步标注（A12）
	if !arch.ConcessionUsed {
		t.Fatalf("★★★ 让步接收料产出的档案必须 concession_used=true")
	}
	found := false
	for _, c := range arch.ConcessionSources {
		if c.TruckLotID == truck.ID && c.InspectionNo != "" && c.Conclusion == InspConclusionCons {
			found = true
		}
	}
	if !found {
		t.Fatalf("★★ 让步来源应列出车次与现行单，实际 %+v", arch.ConcessionSources)
	}

	// 六块齐（A13）
	if arch.Batch.BatchID != batch.ID || arch.Batch.BatchDate != m6Date {
		t.Fatalf("批基本信息错乱: %+v", arch.Batch)
	}
	if arch.CustomerName == "" {
		t.Fatalf("档案应带客户名")
	}
	if len(arch.Feeds) != 1 || arch.Feeds[0].BagCode != bags[0].Code {
		t.Fatalf("投料明细错乱: %+v", arch.Feeds)
	}
	if len(arch.Operations) != 1 || arch.Operations[0].Seq != 1 {
		t.Fatalf("作业段错乱: %+v", arch.Operations)
	}
	if len(arch.FgLots) != 1 || arch.FgLots[0].FgLot.ID != lot.ID || len(arch.FgLots[0].Bags) != 2 {
		t.Fatalf("成品批 / 袋错乱: %+v", arch.FgLots)
	}
	shipOK := false
	for _, s := range arch.Shipments {
		if s.ShipmentID == shipID {
			shipOK = true
		}
	}
	if !shipOK {
		t.Fatalf("★ 档案应含出货单 %d，实际 %+v", shipID, arch.Shipments)
	}

	// 反例：纯合格料的批 ⇒ 无让步标注
	truck2, bags2 := m6Pass(t, st, cust, inMat, 1)
	batch2 := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch2.ID, bags2[0].Code)
	arch2, err := st.GetBatchArchive(ctx, batch2.ID)
	if err != nil {
		t.Fatalf("读批次档案 2 失败: %v", err)
	}
	if arch2.ConcessionUsed || len(arch2.ConcessionSources) != 0 {
		t.Fatalf("★★ 合格料的批不应标让步: %+v", arch2.ConcessionSources)
	}
	_ = truck2
}

// ===== A17（库侧补充）· 追溯零写：调用三个只读入口前后，业务表计数不变 =====

// A17：TraceForward / TraceBackward / GetBatchArchive 全程不写任何业务表
// （s_audit_log 也不写 —— 一期访问日志属 M9）。
func TestM8TraceZeroWrite(t *testing.T) {
	st, cust, inMat, outMat := m8Fixture(t)
	ctx := context.Background()

	truck, bags := m6Pass(t, st, cust, inMat, 1)
	batch := m6Batch(t, st, cust, inMat, outMat)
	m6Feed(t, st, batch.ID, bags[0].Code)
	lot, err := st.CreateFgLot(ctx, batch.ID, CreateFgLotInput{}, m6Actor())
	if err != nil {
		t.Fatalf("生成成品批失败: %v", err)
	}

	counts := func() (shipN, itemN, auditN int) {
		t.Helper()
		for _, q := range []struct {
			sql string
			dst *int
		}{
			{`SELECT COUNT(*) FROM b_shipment`, &shipN},
			{`SELECT COUNT(*) FROM b_shipment_item`, &itemN},
			{`SELECT COUNT(*) FROM s_audit_log`, &auditN},
		} {
			if err := st.DB().QueryRowContext(ctx, q.sql).Scan(q.dst); err != nil {
				t.Fatalf("统计失败: %v", err)
			}
		}
		return
	}
	s0, i0, a0 := counts()

	if _, err := st.TraceForward(ctx, truck.ID, ""); err != nil {
		t.Fatalf("正向失败: %v", err)
	}
	if _, err := st.TraceBackward(ctx, lot.ID, ""); err != nil {
		t.Fatalf("反向失败: %v", err)
	}
	if _, err := st.GetBatchArchive(ctx, batch.ID); err != nil {
		t.Fatalf("档案失败: %v", err)
	}

	s1, i1, a1 := counts()
	if s1 != s0 || i1 != i0 || a1 != a0 {
		t.Fatalf("★★★ M8 零写被破坏：ship %d→%d item %d→%d audit %d→%d",
			s0, s1, i0, i1, a0, a1)
	}
}

// ===== 补充：反向 fg_code / 成品批码类型校验 =====

func TestM8BackwardInputGuards(t *testing.T) {
	st, _, _, _ := m8Fixture(t)
	ctx := context.Background()

	// 两个入参都缺 ⇒ 400
	if _, err := st.TraceBackward(ctx, 0, ""); !errors.Is(err, ErrProdBadInput) {
		t.Fatalf("★ 缺入参应 ErrProdBadInput，实际 %v", err)
	}
	// 非 D 类码 ⇒ 400（B 类深度 2 ⇒ SEQ3 恒 000）
	tonBag, err := codec.Generate(codec.Segments{
		T: "B", BT: BizTypeCG, Customer: m6CustCode, Material: m6InMatCode,
		Date: m6DateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成吨袋码失败: %v", err)
	}
	if _, err := st.TraceBackward(ctx, 0, tonBag); !errors.Is(err, ErrProdBadInput) {
		t.Fatalf("★ 非 D 类码应 ErrProdBadInput，实际 %v", err)
	}
	// 不存在的成品批 ⇒ 404
	fg, err := codec.Generate(codec.Segments{
		T: "D", BT: BizTypeCG, Customer: m6CustCode, Material: m6OutMatCode,
		Date: m6DateSeg, SEQ1: "01", SEQ2: "999", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	if _, err := st.TraceBackward(ctx, 0, fg); !errors.Is(err, ErrProdNotFound) {
		t.Fatalf("★ 不存在的成品批应 ErrProdNotFound，实际 %v", err)
	}
}
