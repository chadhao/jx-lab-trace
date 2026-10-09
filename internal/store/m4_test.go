package store

// ===== M4 取样与留样 · 库侧 TC =====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip，
//	在测试服务器上以 JX_TEST_DB=1 执行（scripts/run_tc_server.sh）。
// ★ 夹具纪律（批 3 教训）：清理**按账号精确匹配**（禁前缀通配）、**先子表后父表**；
//	断言「某表为空/有几个」一律**限定本用例作用域**，不全库 COUNT(*)。
// ★ 生产批 / 成品批属批 6 —— 本批按任务包允许**直写库造最小对象**（不建批入口）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

const (
	m4StoreOpenID = "ou_test_m4_store"
	m4CustCode    = "9401"
	m4MatCode     = "9401"
	m4Date        = "2026-10-09"
	m4DateSeg     = "261009"
)

// m4Wipe 清掉 M4 库侧测试数据。
//
// ★ 顺序硬约束：销毁 → 借还 → 留样 → 样品 → 取样组 → 作废记录 → 袋 → 车 → 预报
//
//	→ 标签 → 生产批 → 成品批 → 主数据（先子后父）。
//
// ★ 范围硬约束：只删本账号（ou_test_m4_store）的行；主数据按测试编号 + 测试账号前缀。
func m4Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	me := m4StoreOpenID
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
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
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m4CustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m4MatCode)
}

// m4Fixture 清场 + 建测试主数据（客户 9401 / 原料物料 9401），返回其 id。
func m4Fixture(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	m4Wipe(t, st)
	t.Cleanup(func() { m4Wipe(t, st) })

	ctx := context.Background()
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M4测试客户', '启用', 1, 1, ?)`, m4CustCode, m4StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	custID, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M4测试原料', '原料', '启用', 1, 1, ?)`, m4MatCode, m4StoreOpenID)
	if err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}
	matID, _ := res.LastInsertId()
	return st, custID, matID
}

func m4Actor() MDActor {
	return MDActor{OpenID: m4StoreOpenID, Name: "m4-store", Role: "qc", IP: "127.0.0.1"}
}

// m4TruckWithBags 造一个车次并按实际袋数生成袋码（走批 3 的真实链路），返回车次与袋。
func m4TruckWithBags(t *testing.T, st *Store, custID, matID int64, bags int) (TruckLot, []Bag) {
	t.Helper()
	ctx := context.Background()
	n, err := st.CreateNotice(ctx, NoticeInput{
		CustomerID: custID, MaterialID: matID, BizType: BizTypeCG,
		ArriveDate: m4Date, EstBagCount: bags, PlateNo: "湘F·M4",
	}, m4Actor())
	if err != nil {
		t.Fatalf("登记预报失败: %v", err)
	}
	truck, err := st.ConfirmArrival(ctx, ArriveInput{NoticeID: n.ID}, m4Actor())
	if err != nil {
		t.Fatalf("到货确认失败: %v", err)
	}
	if _, err := st.WeighTruck(ctx, truck.ID, WeighInput{
		Gross: tons{milli: 30000 + int64(bags)}, Tare: tons{milli: 5000},
	}, m4Actor()); err != nil {
		t.Fatalf("过磅失败: %v", err)
	}
	bs, err := st.GenerateBags(ctx, truck.ID, bags, m4Actor())
	if err != nil {
		t.Fatalf("生成袋码失败: %v", err)
	}
	return truck, bs
}

// m4BatchFixture 直写库造一个生产批（T=C）—— 属批 6 的对象，本批只读（任务包 §5 允许夹具）。
func m4BatchFixture(t *testing.T, st *Store, custID, matID int64) (int64, string) {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "C", BT: "CG", Customer: m4CustCode, Material: m4MatCode,
		Date: m4DateSeg, SEQ1: "01", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成生产批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`, code, custID, matID, matID, m4Date, m4StoreOpenID)
	if err != nil {
		t.Fatalf("造生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// m4FgLotFixture 直写库造一个成品批（T=D），挂在指定生产批下。
func m4FgLotFixture(t *testing.T, st *Store, custID, matID, batchID int64) (int64, string) {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "D", BT: "CG", Customer: m4CustCode, Material: m4MatCode,
		Date: m4DateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot
  (code, batch_id, customer_id, output_material_id, status, created_by)
VALUES (?,?,?,?, '在库', ?)`, code, batchID, custID, matID, m4StoreOpenID)
	if err != nil {
		t.Fatalf("造成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// parentFromCode 从 27 位全码取派生串父码（人读行前 7 组，§6-4）—— 用例侧独立复算。
func parentFromCode(t *testing.T, code string) string {
	t.Helper()
	human, err := codec.ToHuman(code)
	if err != nil {
		t.Fatalf("转人读行失败: %v", err)
	}
	parts := strings.Split(human, "-")
	if len(parts) != 9 {
		t.Fatalf("人读行应 9 组，实际 %d 组 %q", len(parts), human)
	}
	return strings.Join(parts[:7], "-")
}

// TC-M4-01 正常：扫吨袋码取样一次 ⇒ 生成 2 条样品（份样 + 保留样），编号可推导。
func TestTC_M4_01_ScanBagCreatesTwoSamples(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	_, bags := m4TruckWithBags(t, st, cust, mat, 3)
	wantParent := parentFromCode(t, bags[0].Code)

	res, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if err != nil {
		t.Fatalf("扫码取样失败: %v", err)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("应生成 2 条样品（份样+保留样），实际 %d", len(res.Samples))
	}
	byRole := map[string]Sample{}
	for _, s := range res.Samples {
		byRole[s.Role] = s
	}
	inc, ok := byRole[SampleRoleIncremental]
	if !ok {
		t.Fatalf("缺份样，实际角色 %v", roleKeys(byRole))
	}
	ret, ok := byRole[SampleRoleRetention]
	if !ok {
		t.Fatalf("缺保留样，实际角色 %v", roleKeys(byRole))
	}
	if want := wantParent + "-I01"; inc.SampleNo != want {
		t.Fatalf("份样编号应 %q，实际 %q", want, inc.SampleNo)
	}
	if want := wantParent + "-R01"; ret.SampleNo != want {
		t.Fatalf("保留样编号应 %q，实际 %q", want, ret.SampleNo)
	}
	if res.Parent != wantParent {
		t.Fatalf("父码应 %q，实际 %q", wantParent, res.Parent)
	}
	if inc.BagID == nil || ret.BagID == nil {
		t.Fatal("原料样必须绑吨袋（bag_id 非空）")
	}
	if inc.BatchID != nil || inc.FgLotID != nil {
		t.Fatal("原料样不得绑生产批/成品批")
	}
}

func roleKeys(m map[string]Sample) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TC-M4-02 边界：同袋重复扫 ⇒ 生成第 2 组（序号递增 I02/R02），**不覆盖**第一组。
func TestTC_M4_02_RescanSameBagNewGroupSeq(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	_, bags := m4TruckWithBags(t, st, cust, mat, 1)
	parent := parentFromCode(t, bags[0].Code)

	first, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if err != nil {
		t.Fatalf("第 1 次取样失败: %v", err)
	}
	second, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if err != nil {
		t.Fatalf("第 2 次取样失败: %v", err)
	}
	if len(first.Samples) != 2 || len(second.Samples) != 2 {
		t.Fatalf("每扫一次应生成 2 条样品，实际 %d / %d", len(first.Samples), len(second.Samples))
	}
	want := map[string]string{
		SampleRoleIncremental: parent + "-I02",
		SampleRoleRetention:   parent + "-R02",
	}
	for _, s := range second.Samples {
		if s.SampleNo != want[s.Role] {
			t.Fatalf("第 2 组 %s 编号应 %q，实际 %q", s.Role, want[s.Role], s.SampleNo)
		}
	}
	// 第 1 组仍在（不被覆盖）
	var alive int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_sample WHERE sample_no IN (?,?)`,
		parent+"-I01", parent+"-R01").Scan(&alive); err != nil {
		t.Fatalf("统计第 1 组失败: %v", err)
	}
	if alive != 2 {
		t.Fatalf("第 1 组必须保留（2 条），实际 %d 条", alive)
	}
	// 该袋共 4 条样品
	var total int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_sample WHERE bag_id = ?`, first.Samples[0].BagID).Scan(&total); err != nil {
		t.Fatalf("统计样品失败: %v", err)
	}
	if total != 4 {
		t.Fatalf("同袋两次取样应共 4 条样品，实际 %d", total)
	}
}

// TC-M4-03 ★★ 一车 30 袋只扫 3 袋 ⇒ **只有 3 袋有样品记录**，其余无（不是"未测"）。
//
// ★ 断言限定本用例作用域（按本车袋计数），不全库 COUNT(*)（批 3 夹具教训）。
func TestTC_M4_03_NoPreGeneration(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	truck, bags := m4TruckWithBags(t, st, cust, mat, 30)
	if len(bags) != 30 {
		t.Fatalf("应造 30 袋，实际 %d", len(bags))
	}

	countScoped := func() (rows, bagsWithSamples int) {
		t.Helper()
		if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*), COUNT(DISTINCT s.bag_id)
  FROM b_sample s JOIN b_bag b ON b.id = s.bag_id
 WHERE b.truck_lot_id = ?`, truck.ID).Scan(&rows, &bagsWithSamples); err != nil {
			t.Fatalf("统计本车样品失败: %v", err)
		}
		return
	}
	if r, b := countScoped(); r != 0 || b != 0 {
		t.Fatalf("取样前本车应无样品记录，实际 rows=%d bags=%d", r, b)
	}

	for i := 0; i < 3; i++ {
		if _, err := st.TakeSample(ctx, TakeInput{Code: bags[i].Code}, m4Actor()); err != nil {
			t.Fatalf("扫第 %d 袋失败: %v", i+1, err)
		}
	}
	rows, bagsWith := countScoped()
	if rows != 6 { // 3 袋 × (1 份样 + 1 保留样)
		t.Fatalf("只有扫过的 3 袋应有样品（6 条），实际 %d 条", rows)
	}
	if bagsWith != 3 {
		t.Fatalf("应恰好 3 袋有样品记录，实际 %d 袋", bagsWith)
	}
	// 其余 27 袋**无记录**（不是「未测」占位）
	var untouched int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM b_bag b
 WHERE b.truck_lot_id = ?
   AND NOT EXISTS (SELECT 1 FROM b_sample s WHERE s.bag_id = b.id)`,
		truck.ID).Scan(&untouched); err != nil {
		t.Fatalf("统计未扫袋失败: %v", err)
	}
	if untouched != 27 {
		t.Fatalf("未扫的 27 袋应无任何样品记录（无「未测」占位），实际无记录袋数=%d", untouched)
	}
	// 作用域内不得出现「未测」状态行（三态是 M5 的事）
	var placeholder int
	if err := st.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM b_sample s JOIN b_bag b ON b.id = s.bag_id
 WHERE b.truck_lot_id = ? AND s.status = '未测'`, truck.ID).Scan(&placeholder); err != nil {
		t.Fatalf("统计占位失败: %v", err)
	}
	if placeholder != 0 {
		t.Fatalf("不得写「未测」占位，实际 %d 条", placeholder)
	}
}

// TC-M4-04 正常：3 个份样并入新建的大样 ⇒ sample_count=3，成员可查。
func TestTC_M4_04_GroupMergeSampleCount(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	truck, bags := m4TruckWithBags(t, st, cust, mat, 3)

	ids := make([]int64, 0, 3)
	for _, b := range bags {
		res, err := st.TakeSample(ctx, TakeInput{Code: b.Code}, m4Actor())
		if err != nil {
			t.Fatalf("取样失败: %v", err)
		}
		for _, s := range res.Samples {
			if s.Role == SampleRoleIncremental {
				ids = append(ids, s.ID)
			}
		}
	}
	if len(ids) != 3 {
		t.Fatalf("应有 3 个份样，实际 %d", len(ids))
	}

	grp, err := st.CreateSampleGroup(ctx, GroupInput{
		TargetType: TargetTruck, TargetID: truck.ID, SampleIDs: ids,
	}, m4Actor())
	if err != nil {
		t.Fatalf("建取样组失败: %v", err)
	}
	if grp.SampleCount != 3 {
		t.Fatalf("sample_count 应 3，实际 %d", grp.SampleCount)
	}
	if grp.Composite == nil || grp.Composite.Role != SampleRoleComposite {
		t.Fatalf("取样组须带 role=大样 的样品行，实际 %+v", grp.Composite)
	}
	if grp.GroupNo != grp.Composite.SampleNo {
		t.Fatalf("group_no 必须等于大样 sample_no（§6-5）：%q vs %q",
			grp.GroupNo, grp.Composite.SampleNo)
	}
	wantParent := parentFromCode(t, truck.Code)
	if want := wantParent + "-C01"; grp.GroupNo != want {
		t.Fatalf("大样编号应 %q，实际 %q", want, grp.GroupNo)
	}

	members, err := st.ListGroupMembers(ctx, grp.ID)
	if err != nil {
		t.Fatalf("查成员失败: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("成员应 3 个份样，实际 %d", len(members))
	}
	for _, m := range members {
		if m.Role != SampleRoleIncremental {
			t.Fatalf("成员应为份样，实际 %s（%s）", m.Role, m.SampleNo)
		}
		if m.GroupID == nil || *m.GroupID != grp.ID {
			t.Fatalf("成员 group_id 应回填为 %d，实际 %v", grp.ID, m.GroupID)
		}
	}
}

// TC-M4-05 ★ 保留样入库 ⇒ 期限按样品类型取默认（原料6月/中间3月/成品12月/仲裁24月）；
// 到期可检索（due 查询）。
func TestTC_M4_05_RetentionDefaultAndDueQuery(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()

	// —— 四类样品各一条 ——
	_, bags := m4TruckWithBags(t, st, cust, mat, 1)
	batchID, batchCode := m4BatchFixture(t, st, cust, mat)
	fgID, fgCode := m4FgLotFixture(t, st, cust, mat, batchID)

	retentionOf := func(code string) Sample {
		t.Helper()
		res, err := st.TakeSample(ctx, TakeInput{Code: code}, m4Actor())
		if err != nil {
			t.Fatalf("取样失败(%s): %v", code, err)
		}
		for _, s := range res.Samples {
			if s.Role == SampleRoleRetention {
				return s
			}
		}
		t.Fatalf("取样结果缺保留样: %+v", res.Samples)
		return Sample{}
	}
	rawRet := retentionOf(bags[0].Code)
	midRet := retentionOf(batchCode)
	fgRet := retentionOf(fgCode)

	// 仲裁样不在取样链路（批 5 范围）——夹具直插一条（任务包 §5 允许）
	arbitrationParent := parentFromCode(t, fgCode)
	arbitration, err := st.GetSample(ctx, mustInsertArbitrationSample(t, st, arbitrationParent, fgID))
	if err != nil {
		t.Fatalf("读仲裁样失败: %v", err)
	}

	now := time.Now()
	cases := []struct {
		name   string
		sample Sample
		months int
	}{
		{"原料", rawRet, 6},
		{"中间", midRet, 3},
		{"成品", fgRet, 12},
		{"仲裁", arbitration, 24},
	}
	for _, tc := range cases {
		row, err := st.RetainSample(ctx, RetainInput{
			SampleID: tc.sample.ID, Location: "化验室-留样柜A-第3层",
		}, m4Actor())
		if err != nil {
			t.Fatalf("%s样入库失败: %v", tc.name, err)
		}
		want := now.AddDate(0, tc.months, 0).Format("2006-01-02")
		if row.RetentionUntil != want {
			t.Fatalf("%s样保留期限应默认 %d 个月 → %s，实际 %s",
				tc.name, tc.months, want, row.RetentionUntil)
		}
		if row.Status != SampleStatusInStock {
			t.Fatalf("%s样入库状态应「在库」，实际 %q", tc.name, row.Status)
		}
	}

	// 到期可检索：再入一条已过期的（显式日期），due 查询应命中它、且不命中未到期的
	expiredRet := retentionOf(bags[0].Code) // 同袋第 2 次取样的保留样
	if _, err := st.RetainSample(ctx, RetainInput{
		SampleID: expiredRet.ID, Location: "化验室-留样柜B-第1层",
		RetentionUntil: "2020-01-01",
	}, m4Actor()); err != nil {
		t.Fatalf("过期留样入库失败: %v", err)
	}
	due, err := st.ListRetention(ctx, now.Format("2006-01-02"), false)
	if err != nil {
		t.Fatalf("到期检索失败: %v", err)
	}
	// 断言限定本用例作用域：过期那条必须命中；四条未来到期的必须不命中
	// （不拿全表行数硬断言 —— 库里可能有其他包的夹具数据）。
	hitExpired, hitFuture := false, false
	for _, r := range due {
		switch r.SampleID {
		case expiredRet.ID:
			hitExpired = true
		case rawRet.ID, midRet.ID, fgRet.ID, arbitration.ID:
			hitFuture = true
		}
	}
	if !hitExpired {
		t.Fatalf("到期检索应命中本用例的过期留样（2020-01-01），实际 %d 条中无它", len(due))
	}
	if hitFuture {
		t.Fatalf("未到期的留样不得出现在到期检索结果里（%d 条）", len(due))
	}
}

// mustInsertArbitrationSample 夹具：直插一条仲裁样（role=仲裁样，绑指定成品批）。
func mustInsertArbitrationSample(t *testing.T, st *Store, parent string, fgLotID int64) int64 {
	t.Helper()
	sampleNo := parent + "-A01"
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_sample (sample_no, role, fg_lot_id, sampled_at, sampled_by, status, created_by)
VALUES (?, '仲裁样', ?, NOW(3), 'm4-store', '在库', ?)`, sampleNo, fgLotID, m4StoreOpenID)
	if err != nil {
		t.Fatalf("造仲裁样失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TC-M4-06 正常：借出 ⇒ 状态「已借出」；归还 ⇒ 回「在库」（b_sample 与 retention 同步）。
func TestTC_M4_06_LendReturnStatus(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	_, bags := m4TruckWithBags(t, st, cust, mat, 1)
	res, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if err != nil {
		t.Fatalf("取样失败: %v", err)
	}
	var ret Sample
	for _, s := range res.Samples {
		if s.Role == SampleRoleRetention {
			ret = s
		}
	}
	if _, err := st.RetainSample(ctx, RetainInput{
		SampleID: ret.ID, Location: "化验室-留样柜A-第2层",
	}, m4Actor()); err != nil {
		t.Fatalf("入库失败: %v", err)
	}

	assertBoth := func(want, when string) {
		t.Helper()
		var sStatus, rStatus string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT status FROM b_sample WHERE id = ?`, ret.ID).Scan(&sStatus); err != nil {
			t.Fatalf("读样品状态失败: %v", err)
		}
		if err := st.DB().QueryRowContext(ctx,
			`SELECT status FROM b_sample_retention WHERE sample_id = ?`, ret.ID).Scan(&rStatus); err != nil {
			t.Fatalf("读留样状态失败: %v", err)
		}
		if sStatus != want || rStatus != want {
			t.Fatalf("%s：两表状态应均为 %q，实际 sample=%q retention=%q",
				when, want, sStatus, rStatus)
		}
	}

	lend, err := st.LendSample(ctx, LendInput{
		SampleID: ret.ID, LentTo: "张工", Purpose: "复核",
	}, m4Actor())
	if err != nil {
		t.Fatalf("借出失败: %v", err)
	}
	if lend.Status != SampleStatusLent {
		t.Fatalf("借出后状态应「已借出」，实际 %q", lend.Status)
	}
	assertBoth(SampleStatusLent, "借出后")

	// 未归还前不可再借
	if _, err := st.LendSample(ctx, LendInput{
		SampleID: ret.ID, LentTo: "李工",
	}, m4Actor()); err == nil {
		t.Fatal("已借出的留样再次借出应被拒绝")
	}

	if _, err := st.ReturnLend(ctx, lend.ID, m4Actor()); err != nil {
		t.Fatalf("归还失败: %v", err)
	}
	assertBoth(SampleStatusInStock, "归还后")
}

// TC-M4-07 异常：销毁不填审批人 ⇒ **拒绝**（approved_by 空 ⇒ ErrDestroyNeedApprover）。
// 填了审批人 ⇒ 才落「已销毁」，且记录含发起人 + 审批人。
func TestTC_M4_07_DestroyWithoutApproverRejected(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	_, bags := m4TruckWithBags(t, st, cust, mat, 1)
	res, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if err != nil {
		t.Fatalf("取样失败: %v", err)
	}
	var ret Sample
	for _, s := range res.Samples {
		if s.Role == SampleRoleRetention {
			ret = s
		}
	}
	if _, err := st.RetainSample(ctx, RetainInput{
		SampleID: ret.ID, Location: "化验室-留样柜C-第1层",
	}, m4Actor()); err != nil {
		t.Fatalf("入库失败: %v", err)
	}

	// 未发起 ⇒ 审批被拒（409）
	if _, err := st.ApproveDestroy(ctx, ret.ID, "王主任", m4Actor()); !errors.Is(err, ErrDestroyNotInit) {
		t.Fatalf("未发起就审批应报 ErrDestroyNotInit，实际 %v", err)
	}

	if _, err := st.InitDestroy(ctx, DestroyInitInput{
		SampleID: ret.ID, DestroyedBy: "张三", Reason: "超过保留期",
	}, m4Actor()); err != nil {
		t.Fatalf("销毁发起失败: %v", err)
	}
	// ★ 发起不改状态
	var status string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM b_sample WHERE id = ?`, ret.ID).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != SampleStatusInStock {
		t.Fatalf("发起后状态应仍「在库」，实际 %q", status)
	}

	// ★★ 不填审批人 ⇒ 拒绝
	if _, err := st.ApproveDestroy(ctx, ret.ID, "", m4Actor()); !errors.Is(err, ErrDestroyNeedApprover) {
		t.Fatalf("审批人为空应报 ErrDestroyNeedApprover，实际 %v", err)
	}
	if _, err := st.ApproveDestroy(ctx, ret.ID, "   ", m4Actor()); !errors.Is(err, ErrDestroyNeedApprover) {
		t.Fatalf("审批人全空白应同样拒绝，实际 %v", err)
	}
	// 状态仍未销毁
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM b_sample WHERE id = ?`, ret.ID).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != SampleStatusInStock {
		t.Fatalf("未获审批不得置为已销毁，实际 %q", status)
	}

	// 填了审批人 ⇒ 通过
	row, err := st.ApproveDestroy(ctx, ret.ID, "李四", m4Actor())
	if err != nil {
		t.Fatalf("销毁审批失败: %v", err)
	}
	if row.ApprovedBy != "李四" || row.DestroyedBy != "张三" {
		t.Fatalf("销毁记录须留发起人+审批人，实际 发起=%q 审批=%q", row.DestroyedBy, row.ApprovedBy)
	}
	// 两表同步为已销毁
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM b_sample WHERE id = ?`, ret.ID).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != SampleStatusDestroyed {
		t.Fatalf("审批后样品状态应「已销毁」，实际 %q", status)
	}
	var rStatus string
	if err := st.DB().QueryRowContext(ctx,
		`SELECT status FROM b_sample_retention WHERE sample_id = ?`, ret.ID).Scan(&rStatus); err != nil {
		t.Fatalf("读留样状态失败: %v", err)
	}
	if rStatus != SampleStatusDestroyed {
		t.Fatalf("审批后留样状态应「已销毁」，实际 %q", rStatus)
	}
	// 重复审批 ⇒ 拒绝（一对象只销毁一次）
	if _, err := st.ApproveDestroy(ctx, ret.ID, "赵四", m4Actor()); !errors.Is(err, ErrDestroyAlready) {
		t.Fatalf("重复审批应报 ErrDestroyAlready，实际 %v", err)
	}
}

// TC-M4-08 ★ 扫生产批码 ⇒ 中间样绑**生产批**（batch_id 非空、bag_id 空）。
func TestTC_M4_08_IntermediateBindsBatch(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	batchID, batchCode := m4BatchFixture(t, st, cust, mat)

	res, err := st.TakeSample(ctx, TakeInput{Code: batchCode}, m4Actor())
	if err != nil {
		t.Fatalf("扫生产批码取样失败: %v", err)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("应生成 2 条样品，实际 %d", len(res.Samples))
	}
	for _, s := range res.Samples {
		if s.BatchID == nil || *s.BatchID != batchID {
			t.Fatalf("%s 应绑生产批 #%d，实际 %v", s.SampleNo, batchID, s.BatchID)
		}
		if s.BagID != nil {
			t.Fatalf("%s 不得绑吨袋（中间样不是原料样），实际 bag_id=%d", s.SampleNo, *s.BagID)
		}
		if s.FgLotID != nil {
			t.Fatalf("%s 不得绑成品批", s.SampleNo)
		}
	}
	wantParent := parentFromCode(t, batchCode)
	if res.Parent != wantParent {
		t.Fatalf("父码应 %q，实际 %q", wantParent, res.Parent)
	}
	if res.Samples[0].SampleNo != wantParent+"-I01" {
		t.Fatalf("中间份样编号应 %q，实际 %q", wantParent+"-I01", res.Samples[0].SampleNo)
	}
}

// TC-M4-09 ★ 扫成品批码 ⇒ 成品样绑**成品批**（fg_lot_id 非空）。
func TestTC_M4_09_FinishedGoodsBindsFgLot(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	batchID, _ := m4BatchFixture(t, st, cust, mat)
	fgID, fgCode := m4FgLotFixture(t, st, cust, mat, batchID)

	res, err := st.TakeSample(ctx, TakeInput{Code: fgCode}, m4Actor())
	if err != nil {
		t.Fatalf("扫成品批码取样失败: %v", err)
	}
	if len(res.Samples) != 2 {
		t.Fatalf("应生成 2 条样品，实际 %d", len(res.Samples))
	}
	for _, s := range res.Samples {
		if s.FgLotID == nil || *s.FgLotID != fgID {
			t.Fatalf("%s 应绑成品批 #%d，实际 %v", s.SampleNo, fgID, s.FgLotID)
		}
		if s.BagID != nil || s.BatchID != nil {
			t.Fatalf("%s 只绑成品批，实际 bag=%v batch=%v", s.SampleNo, s.BagID, s.BatchID)
		}
	}
	wantParent := parentFromCode(t, fgCode)
	if res.Parent != wantParent {
		t.Fatalf("父码应 %q，实际 %q", wantParent, res.Parent)
	}
}

// —— 补充（非 TC）：作废袋不得取样 + 配置可覆盖保留期限（纯函数）——

// 作废袋拒绝取样（D1 口径：只允许对有效袋取样）。
func TestM4_VoidedBagCannotTake(t *testing.T) {
	st, cust, mat := m4Fixture(t)
	ctx := context.Background()
	_, bags := m4TruckWithBags(t, st, cust, mat, 1)
	if _, err := st.VoidBag(ctx, bags[0].ID, "录错作废", m4Actor()); err != nil {
		t.Fatalf("作废袋失败: %v", err)
	}
	_, err := st.TakeSample(ctx, TakeInput{Code: bags[0].Code}, m4Actor())
	if !errors.Is(err, ErrTargetVoided) {
		t.Fatalf("作废袋取样应报 ErrTargetVoided，实际 %v", err)
	}
}

// 保留期限默认值可配置（SetRetentionDefaults 覆盖，不硬编码）+ 类型判定次序（§6-9）。
func TestM4_RetentionDefaultsConfigurable(t *testing.T) {
	st := &Store{}
	if got := st.RetentionDefaults(); got != DefaultRetentionDefaults() {
		t.Fatalf("未配置时应取 D4 缺省 6/3/12/24，实际 %+v", got)
	}
	st.SetRetentionDefaults(RetentionDefaults{
		RawMonths: 3, IntermediateMonths: 1, FGMonths: 6, ArbitrationMonths: 12,
	})
	got := st.RetentionDefaults()
	if got.RawMonths != 3 || got.IntermediateMonths != 1 ||
		got.FGMonths != 6 || got.ArbitrationMonths != 12 {
		t.Fatalf("配置未生效: %+v", got)
	}
	// 类型判定次序：仲裁优先，其次看绑定列（任务包 §6-9）
	if k, _ := RetentionKindOf(SampleRoleArbitration, ptr64(1), nil, nil); k != "仲裁" {
		t.Fatalf("role=仲裁样 应判「仲裁」，实际 %q", k)
	}
	if k, _ := RetentionKindOf(SampleRoleRetention, ptr64(1), nil, nil); k != "原料" {
		t.Fatalf("bag_id 非空应判「原料」，实际 %q", k)
	}
	if k, _ := RetentionKindOf(SampleRoleRetention, nil, ptr64(2), nil); k != "中间" {
		t.Fatalf("batch_id 非空应判「中间」，实际 %q", k)
	}
	if k, _ := RetentionKindOf(SampleRoleRetention, nil, nil, ptr64(3)); k != "成品" {
		t.Fatalf("fg_lot_id 非空应判「成品」，实际 %q", k)
	}
	if m, _ := got.monthsOf("原料"); m != 3 {
		t.Fatalf("原料月数应取配置值 3，实际 %d", m)
	}
}

func ptr64(v int64) *int64 { return &v }
