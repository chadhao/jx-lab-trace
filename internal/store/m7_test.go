package store

// ===== M7 出货 · 库侧 TC（TC-M7-01~06 + A2~A7 / A15 关键不变量）=====
//
// ★ 这些是 DB 集成测试：按 docs/05，本机未设 JX_TEST_DB=1 一律 Skip，
//	在测试服务器上以 JX_TEST_DB=1 执行（scripts/run_tc_server.sh）。
// ★ 夹具纪律（§6-19）：清理按账号精确匹配、**先子表后父表**（先 item 后 shipment）；
//	断言「某表有几行」一律限定**本单作用域**（shipment_id），不全库 COUNT(*)。
// ★ 上游对象（生产批 / 成品批 / 成品袋）允许夹具直写库造最小对象（任务包 §5）；
//	出货动作本身全部走真实实现。接口级 TC 见 internal/httpapi/m7_test.go。

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
)

const (
	m7StoreOpenID  = "ou_test_m7_store"
	m7StoreInitID  = "ou_test_m7_store_init"
	m7StoreApprID  = "ou_test_m7_store_approve"
	m7CustCode     = "9421"
	m7Cust2Code    = "9423"
	m7InMatCode    = "9421"
	m7OutMatCode   = "9422"
	m7BatchDate    = "2026-10-02"
	m7BatchDateSeg = "261002"
)

// m7Wipe 清掉 M7 库侧测试数据。
//
// ★ 顺序硬约束：**先子后父** —— b_shipment_item 先于 b_shipment；
//
//	成品袋 / 成品批 / 生产批 / 检测族 / 袋 / 车 逐层先子后父。
func m7Wipe(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	actors := []interface{}{m7StoreOpenID, m7StoreInitID, m7StoreApprID}
	q(`DELETE FROM b_shipment_item WHERE created_by IN (?,?,?)
	       OR shipment_id IN (SELECT id FROM b_shipment WHERE created_by IN (?,?,?))`,
		append(actors, actors...)...)
	q(`DELETE FROM b_shipment WHERE created_by IN (?,?,?)`, actors...)

	q(`DELETE FROM b_fg_bag WHERE fg_lot_id IN (SELECT id FROM b_fg_lot WHERE created_by = ?)`, m7StoreOpenID)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, m7StoreOpenID)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, m7StoreOpenID)

	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?,?,?)`, actors...)

	q(`DELETE FROM m_customer WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m7CustCode, m7Cust2Code)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m7InMatCode, m7OutMatCode)
}

// m7Fixture 清场 + 建测试主数据（客户 9421/9423、原料 9421、成品 9422）。
func m7Fixture(t *testing.T) (*Store, int64, int64, int64) {
	t.Helper()
	st := migrateForTest(t)
	m7Wipe(t, st)
	t.Cleanup(func() { m7Wipe(t, st) })
	ctx := context.Background()

	res, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M7测试客户一', '启用', 1, 1, ?)`, m7CustCode, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户一失败: %v", err)
	}
	cust1, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M7测试客户二', '启用', 1, 1, ?)`, m7Cust2Code, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建测试客户二失败: %v", err)
	}
	cust2, _ := res.LastInsertId()

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M7测试原料', '原料', '启用', 1, 1, ?)`, m7InMatCode, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	inMat, _ := res.LastInsertId()
	_ = inMat // 原料 id 不入返回值：夹具批的 input_material_id 走占位（M7 不读该列）

	res, err = st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M7测试成品', '成品', '启用', 1, 1, ?)`, m7OutMatCode, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	outMat, _ := res.LastInsertId()

	return st, cust1, cust2, outMat
}

func m7Actor() MDActor {
	return MDActor{OpenID: m7StoreOpenID, Name: "m7-store", Role: "receiver", IP: "127.0.0.1"}
}

func m7InitActor() MDActor {
	return MDActor{OpenID: m7StoreInitID, Name: "m7-发起人", Role: "receiver", IP: "127.0.0.1"}
}

func m7ApproveActor() MDActor {
	return MDActor{OpenID: m7StoreApprID, Name: "m7-审批人", Role: "management", IP: "127.0.0.1"}
}

// m7Batch 直写一个生产批（夹具最小对象；返回 id）。
func m7Batch(t *testing.T, st *Store, custID int64, custCode string, outMatID int64) int64 {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "C", BT: BizTypeCG, Customer: custCode, Material: m7OutMatCode,
		Date: m7BatchDateSeg, SEQ1: "01", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`,
		code, custID, 1 /*inMat 占位：出货不读该列*/, outMatID, m7BatchDate, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建夹具生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// m7FgLot 直写一个成品批（seq2 保证同批内码唯一）。
func m7FgLot(t *testing.T, st *Store, batchID, custID int64, custCode string, outMatID int64, seq2 int) FgLot {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "D", BT: BizTypeCG, Customer: custCode, Material: m7OutMatCode,
		Date: m7BatchDateSeg, SEQ1: "01", SEQ2: pad(seq2, 3), SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot (code, batch_id, customer_id, output_material_id, qty_bag, status, created_by)
VALUES (?,?,?,?, 0, '在库', ?)`, code, batchID, custID, outMatID, m7StoreOpenID)
	if err != nil {
		t.Fatalf("建夹具成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	lot := FgLot{ID: id, Code: code, BatchID: batchID, CustomerID: custID,
		OutputMaterialID: outMatID, Status: FgLotStatusInStock}
	lot.Human, _ = codec.ToHuman(code)
	return lot
}

// m7FgBags 直写 n 个成品袋（段位继承成品批码，SEQ3 依次编号）。
func m7FgBags(t *testing.T, st *Store, lot FgLot, n int) []FgBag {
	t.Helper()
	ctx := context.Background()
	p, err := codec.Parse(lot.Code)
	if err != nil {
		t.Fatalf("成品批码不可解析: %v", err)
	}
	out := make([]FgBag, 0, n)
	for i := 1; i <= n; i++ {
		code, err := codec.Generate(codec.Segments{
			T: "E", BT: p.Seg.BT, Customer: p.Seg.Customer, Material: p.Seg.Material,
			Date: p.Seg.Date, SEQ1: p.Seg.SEQ1, SEQ2: p.Seg.SEQ2, SEQ3: pad(i, 3),
		})
		if err != nil {
			t.Fatalf("生成成品袋码失败: %v", err)
		}
		res, err := st.DB().ExecContext(ctx, `
INSERT INTO b_fg_bag (code, fg_lot_id, bag_seq, status, created_by)
VALUES (?,?,?, '在库', ?)`, code, lot.ID, i, m7StoreOpenID)
		if err != nil {
			t.Fatalf("建夹具成品袋失败: %v", err)
		}
		id, _ := res.LastInsertId()
		b := FgBag{ID: id, Code: code, FgLotID: lot.ID, BagSeq: i, Status: FgBagStatusInStock}
		b.Human, _ = codec.ToHuman(code)
		out = append(out, b)
	}
	return out
}

// m7ShipItemN 统计**本单作用域**的明细行数（★ 不全库 COUNT）。
func m7ShipItemN(t *testing.T, st *Store, shipID int64) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_shipment_item WHERE shipment_id = ?`, shipID).Scan(&n); err != nil {
		t.Fatalf("统计出货明细失败: %v", err)
	}
	return n
}

// m7BagStatus 读单个成品袋状态。
func m7BagStatus(t *testing.T, st *Store, bagID int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_fg_bag WHERE id = ?`, bagID).Scan(&s); err != nil {
		t.Fatalf("读成品袋状态失败: %v", err)
	}
	return s
}

// m7ShipStatus 直读出货单状态（绕过读模型，作不变量断言）。
func m7ShipStatus(t *testing.T, st *Store, shipID int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_shipment WHERE id = ?`, shipID).Scan(&s); err != nil {
		t.Fatalf("读出货单状态失败: %v", err)
	}
	return s
}

// ===== TC-M7-01 正常：扫 15 袋归集 =====

// TC-M7-01 正常：扫 15 个成品袋码 ⇒ 一张出货单、明细 **15 行**（按本单作用域计数）；
// 单号形态 CH + YYMMDD + '-' + 3 位序号；建单即「已出厂」、ship_at IS NULL；
// ★ 归集本身不动袋状态（§6-3）；第二张单序号 = 第一张 + 1（串行取号）。
func TestTC_M7_01_Store_CreateShipment15Bags(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 16) // 15 归集 + 1 留给第二张单

	codes := make([]string, 0, 15)
	for i := 0; i < 15; i++ {
		codes = append(codes, bags[i].Code)
	}
	sh, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: codes, Remark: "TC-M7-01",
	}, m7Actor())
	if err != nil {
		t.Fatalf("建出货单失败: %v", err)
	}

	// 单号形态：CH + 6 位日期 + '-' + 3 位序号，日期 = 当日
	today := time.Now().Format("060102")
	if !regexp.MustCompile(`^CH\d{6}-\d{3}$`).MatchString(sh.ShipmentNo) {
		t.Fatalf("★★ 出货单号形态应 CH+YYMMDD-NNN，实际 %q", sh.ShipmentNo)
	}
	if !regexp.MustCompile(`^CH` + today + `-\d{3}$`).MatchString(sh.ShipmentNo) {
		t.Fatalf("★★ 出货单号日期段应为当日 %s，实际 %q", today, sh.ShipmentNo)
	}
	if sh.Status != ShipStatusOut {
		t.Fatalf("★★ 建单即「已出厂」，实际 %q", sh.Status)
	}
	if sh.ShipAt != nil {
		t.Fatalf("★ 未出场登记时 ship_at 应 NULL，实际 %v", sh.ShipAt)
	}
	if sh.CustomerID != cust1 {
		t.Fatalf("客户应由首个袋推定为 %d，实际 %d", cust1, sh.CustomerID)
	}

	// 明细 15 行（★ 本单作用域计数）
	if n := m7ShipItemN(t, st, sh.ID); n != 15 {
		t.Fatalf("★★★ 本单明细应 15 行，实际 %d", n)
	}
	items, err := st.ListShipmentItems(ctx, sh.ID)
	if err != nil {
		t.Fatalf("查明细失败: %v", err)
	}
	got := map[string]bool{}
	for _, it := range items {
		got[it.BagCode] = true
	}
	for _, c := range codes {
		if !got[c] {
			t.Fatalf("★★ 明细缺袋码 %s", c)
		}
	}

	// ★ 归集不动袋状态（§6-3：占用由跨单存在性检查表达）
	for i := 0; i < 15; i++ {
		if s := m7BagStatus(t, st, bags[i].ID); s != FgBagStatusInStock {
			t.Fatalf("★★ 归集后袋 #%d 状态应仍「在库」，实际 %q", bags[i].ID, s)
		}
	}

	// 串行取号：第二张单序号 = 第一张 + 1
	sh2, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[15].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建第二张出货单失败: %v", err)
	}
	n1, _ := parseShipSeq(sh.ShipmentNo[len(sh.ShipmentNo)-3:])
	n2, _ := parseShipSeq(sh2.ShipmentNo[len(sh2.ShipmentNo)-3:])
	if n2 != n1+1 {
		t.Fatalf("★★ 当日序号应串行递增：%s → %s", sh.ShipmentNo, sh2.ShipmentNo)
	}
}

// ===== TC-M7-02 ★ 异常：同一袋进第二张单 =====

// TC-M7-02 ★ 异常：袋已归集进单 A ⇒ 单 B 建单 / 追加扫码均拒（409），
// 且被拒后两张单的明细行数均不变（跨单唯一由应用层三段合取落实，A3）。
func TestTC_M7_02_Store_CrossShipmentRejected(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 2)

	shipA, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建单 A 失败: %v", err)
	}

	// ① 建单 B 时再归集同袋 ⇒ 拒
	if _, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[1].Code, bags[0].Code},
	}, m7Actor()); !errors.Is(err, ErrShipBagCollected) {
		t.Fatalf("★★ 跨单重复归集应 ErrShipBagCollected，实际 %v", err)
	}

	// ② 向已存在的单 B 追加同袋 ⇒ 拒
	shipB, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[1].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建单 B 失败: %v", err)
	}
	if _, err := st.AddShipmentItem(ctx, shipB.ID, AddShipmentItemInput{
		BagCode: bags[0].Code,
	}, m7Actor()); !errors.Is(err, ErrShipBagCollected) {
		t.Fatalf("★★ 向第二张单追加已归集袋应 ErrShipBagCollected，实际 %v", err)
	}

	// 被拒后明细行数不变
	if n := m7ShipItemN(t, st, shipA.ID); n != 1 {
		t.Fatalf("单 A 明细应 1 行，实际 %d", n)
	}
	if n := m7ShipItemN(t, st, shipB.ID); n != 1 {
		t.Fatalf("★★ 被拒后单 B 明细应仍 1 行，实际 %d", n)
	}
}

// ===== TC-M7-03 边界：跨成品批允许 / 跨客户拆单 =====

// TC-M7-03 边界：明细来自两个成品批（同客户）⇒ 允许；
// ★ 跨客户混入同一单 ⇒ 拒（ErrShipCrossCustomer / 400，§6-5）。
func TestTC_M7_03_Store_CrossLotAllowedCrossCustomerRejected(t *testing.T) {
	st, cust1, cust2, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot1 := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	lot2 := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 2)
	b1 := m7FgBags(t, st, lot1, 2)
	b2 := m7FgBags(t, st, lot2, 2)

	// 跨成品批、同客户 ⇒ 允许
	ship, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{b1[0].Code, b2[0].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("★★ 跨成品批装一车应允许，实际拒: %v", err)
	}
	lots := map[int64]bool{}
	items, _ := st.ListShipmentItems(ctx, ship.ID)
	for _, it := range items {
		lots[it.FgLotID] = true
	}
	if len(lots) != 2 {
		t.Fatalf("★★ 明细应来自 2 个成品批，实际 %d", len(lots))
	}

	// 跨客户 ⇒ 拒：追加
	batch2 := m7Batch(t, st, cust2, m7Cust2Code, outMat)
	lot3 := m7FgLot(t, st, batch2, cust2, m7Cust2Code, outMat, 1)
	b3 := m7FgBags(t, st, lot3, 2)
	if _, err := st.AddShipmentItem(ctx, ship.ID, AddShipmentItemInput{
		BagCode: b3[0].Code,
	}, m7Actor()); !errors.Is(err, ErrShipCrossCustomer) {
		t.Fatalf("★★ 跨客户追加应 ErrShipCrossCustomer，实际 %v", err)
	}
	// 跨客户 ⇒ 拒：建单
	if _, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{b1[1].Code, b3[1].Code},
	}, m7Actor()); !errors.Is(err, ErrShipCrossCustomer) {
		t.Fatalf("★★ 跨客户建单应 ErrShipCrossCustomer，实际 %v", err)
	}
	// 被拒后本单明细不增
	if n := m7ShipItemN(t, st, ship.ID); n != 2 {
		t.Fatalf("被拒后本单明细应仍 2 行，实际 %d", n)
	}
}

// ===== TC-M7-04 正常：出场登记 =====

// TC-M7-04 正常：出场登记 ⇒ 成品袋全部转「已出厂」、ship_at / plate_no / driver 落库；
// plate_no / driver 缺任一 ⇒ 400；空单 ⇒ 409；重复登记 ⇒ 409（不覆盖）。
func TestTC_M7_04_Store_DepartShipment(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 3)
	ship, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code, bags[1].Code, bags[2].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建出货单失败: %v", err)
	}

	// 必填校验
	if _, err := st.DepartShipment(ctx, ship.ID, DepartInput{Driver: "张三"}, m7Actor()); !errors.Is(err, ErrShipBadInput) {
		t.Fatalf("★ 缺 plate_no 应 ErrShipBadInput，实际 %v", err)
	}
	if _, err := st.DepartShipment(ctx, ship.ID, DepartInput{PlateNo: "湘A·1"}, m7Actor()); !errors.Is(err, ErrShipBadInput) {
		t.Fatalf("★ 缺 driver 应 ErrShipBadInput，实际 %v", err)
	}

	// 正常登记
	out, err := st.DepartShipment(ctx, ship.ID, DepartInput{
		PlateNo: "湘A·76543", Driver: "李四", ShipAt: "2026-10-05 08:00:00", Operator: "王五",
	}, m7Actor())
	if err != nil {
		t.Fatalf("出场登记失败: %v", err)
	}
	if out.ShipAt == nil {
		t.Fatalf("★★ ship_at 应落库")
	}
	if out.PlateNo != "湘A·76543" || out.Driver != "李四" {
		t.Fatalf("车牌 / 司机应落库，实际 %q / %q", out.PlateNo, out.Driver)
	}
	// 袋全部转已出厂
	for _, b := range bags {
		if s := m7BagStatus(t, st, b.ID); s != FgBagStatusOut {
			t.Fatalf("★★★ 出场登记后袋 #%d 应「已出厂」，实际 %q", b.ID, s)
		}
	}
	// 重复登记 ⇒ 409
	if _, err := st.DepartShipment(ctx, ship.ID, DepartInput{
		PlateNo: "湘A·76543", Driver: "李四",
	}, m7Actor()); !errors.Is(err, ErrShipDeparted) {
		t.Fatalf("★★ 重复出场登记应 ErrShipDeparted，实际 %v", err)
	}

	// 空单 ⇒ 409（直插一张无明细的单）
	no := fmt.Sprintf("CH%s-990", time.Now().Format("060102"))
	res, err := st.DB().ExecContext(ctx, `
INSERT INTO b_shipment (shipment_no, customer_id, status, created_by)
VALUES (?,?, '已出厂', ?)`, no, cust1, m7StoreOpenID)
	if err != nil {
		t.Fatalf("造空单失败: %v", err)
	}
	emptyID, _ := res.LastInsertId()
	if _, err := st.DepartShipment(ctx, emptyID, DepartInput{
		PlateNo: "湘A·00000", Driver: "赵六",
	}, m7Actor()); !errors.Is(err, ErrShipNoItems) {
		t.Fatalf("★★ 空单出场登记应 ErrShipNoItems，实际 %v", err)
	}
}

// ===== TC-M7-05 ★ 异常：撤销发起人自审 =====

// TC-M7-05 ★ 异常 + A6：发起**不生效**（状态 / 袋均不动）；发起人自审 ⇒ 拒
// （ErrShipVoidSelf / 403）且状态仍未变；未发起审批 ⇒ 409；他人审批 ⇒ 生效。
func TestTC_M7_05_Store_VoidInitApproveSplit(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 3)
	ship, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code, bags[1].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建出货单失败: %v", err)
	}
	// 另一张单（未发起撤销，用于验 409）
	ship2, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[2].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建单 2 失败: %v", err)
	}
	if _, err := st.DepartShipment(ctx, ship.ID, DepartInput{
		PlateNo: "湘A·11111", Driver: "钱七",
	}, m7Actor()); err != nil {
		t.Fatalf("出场登记失败: %v", err)
	}

	// reason 必填
	if _, err := st.InitShipVoid(ctx, ship.ID, "  ", m7InitActor()); !errors.Is(err, ErrShipBadInput) {
		t.Fatalf("★ 缺 reason 应 ErrShipBadInput，实际 %v", err)
	}

	// 发起 ⇒ 不生效（A6）
	if _, err := st.InitShipVoid(ctx, ship.ID, "错发重开", m7InitActor()); err != nil {
		t.Fatalf("发起撤销失败: %v", err)
	}
	if s := m7ShipStatus(t, st, ship.ID); s != ShipStatusOut {
		t.Fatalf("★★★ 发起不生效：状态应仍「已出厂」，实际 %q", s)
	}
	for _, b := range bags[:2] {
		if s := m7BagStatus(t, st, b.ID); s != FgBagStatusOut {
			t.Fatalf("★★★ 发起不生效：袋 #%d 应仍「已出厂」，实际 %q", b.ID, s)
		}
	}
	// 重复发起 ⇒ 409
	if _, err := st.InitShipVoid(ctx, ship.ID, "再发一次", m7InitActor()); !errors.Is(err, ErrShipVoidPending) {
		t.Fatalf("★★ 重复发起应 ErrShipVoidPending，实际 %v", err)
	}

	// ★ 发起人自审 ⇒ 403（TC-M7-05），且状态仍未变
	if _, err := st.ApproveShipVoid(ctx, ship.ID, m7InitActor()); !errors.Is(err, ErrShipVoidSelf) {
		t.Fatalf("★★★ 发起人自审应 ErrShipVoidSelf，实际 %v", err)
	}
	if s := m7ShipStatus(t, st, ship.ID); s != ShipStatusOut {
		t.Fatalf("★★ 自审被拒后状态应仍「已出厂」，实际 %q", s)
	}

	// 未发起的单审批 ⇒ 409
	if _, err := st.ApproveShipVoid(ctx, ship2.ID, m7ApproveActor()); !errors.Is(err, ErrShipVoidNotInit) {
		t.Fatalf("★★ 未发起即审批应 ErrShipVoidNotInit，实际 %v", err)
	}

	// 他人审批 ⇒ 生效
	approved, err := st.ApproveShipVoid(ctx, ship.ID, m7ApproveActor())
	if err != nil {
		t.Fatalf("他人审批失败: %v", err)
	}
	if approved.Status != ShipStatusVoid {
		t.Fatalf("★★ 审批后应「已撤销」，实际 %q", approved.Status)
	}
	for _, b := range bags[:2] {
		if s := m7BagStatus(t, st, b.ID); s != FgBagStatusInStock {
			t.Fatalf("★★ 审批后袋 #%d 应回退「在库」，实际 %q", b.ID, s)
		}
	}
	// 留痕：init + approve 各 1 笔（s_audit_log）
	for _, action := range []string{"ship_void_init", "ship_void_approve"} {
		var n int
		if err := st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM s_audit_log WHERE entity = 'b_shipment'
			   AND entity_id = ? AND action = ?`, ship.ID, action).Scan(&n); err != nil {
			t.Fatalf("查撤销留痕失败: %v", err)
		}
		if n != 1 {
			t.Fatalf("★★ 撤销留痕 %s 应 1 笔，实际 %d", action, n)
		}
	}
}

// ===== TC-M7-06 正常：撤销后可再出货 =====

// TC-M7-06 正常：撤销审批后 —— 袋回退「在库」、明细行**保留**（不物理删除），
// 同袋可再归集进新单（D1 第 3 项只查未撤销的单）。
func TestTC_M7_06_Store_RecollectAfterVoid(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 2)

	shipA, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code, bags[1].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("建单 A 失败: %v", err)
	}
	if _, err := st.InitShipVoid(ctx, shipA.ID, "装错车", m7InitActor()); err != nil {
		t.Fatalf("发起撤销失败: %v", err)
	}
	if _, err := st.ApproveShipVoid(ctx, shipA.ID, m7ApproveActor()); err != nil {
		t.Fatalf("审批撤销失败: %v", err)
	}

	// 明细保留（历史留痕，不物理删除）
	if n := m7ShipItemN(t, st, shipA.ID); n != 2 {
		t.Fatalf("★★★ 撤销后明细应保留 2 行，实际 %d", n)
	}
	// 袋回退在库
	for _, b := range bags {
		if s := m7BagStatus(t, st, b.ID); s != FgBagStatusInStock {
			t.Fatalf("撤销后袋 #%d 应回退「在库」，实际 %q", b.ID, s)
		}
	}
	// 可再归集进新单（TC-M7-06 / A3 反向）
	shipB, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code, bags[1].Code},
	}, m7Actor())
	if err != nil {
		t.Fatalf("★★★ 撤销后再归集应允许，实际拒: %v", err)
	}
	if n := m7ShipItemN(t, st, shipB.ID); n != 2 {
		t.Fatalf("新单明细应 2 行，实际 %d", n)
	}
	// 原单明细仍保留（两单并存，靠状态区分有效性）
	if n := m7ShipItemN(t, st, shipA.ID); n != 2 {
		t.Fatalf("★★ 原单明细应仍 2 行，实际 %d", n)
	}
	// 已撤销的单不得再扫码
	if _, err := st.AddShipmentItem(ctx, shipA.ID, AddShipmentItemInput{
		BagCode: bags[0].Code,
	}, m7Actor()); !errors.Is(err, ErrShipState) {
		t.Fatalf("★★ 已撤销的单再扫码应 ErrShipState，实际 %v", err)
	}
}

// ===== A15 · 取号超限明确报错 =====

// A15：当日序号 > 999 ⇒ 明确报错 ErrShipSeqOverflow，不自动进位。
func TestM7ShipSeqOverflowReported(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()

	// 直插 CH+当日+-999 的满额行
	no := fmt.Sprintf("CH%s-999", time.Now().Format("060102"))
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_shipment (shipment_no, customer_id, status, created_by)
VALUES (?,?, '已出厂', ?)`, no, cust1, m7StoreOpenID); err != nil {
		t.Fatalf("造满额出货单失败: %v", err)
	}

	// 再建单 ⇒ 超限明确报错（不自动进位）
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 1)
	if _, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code},
	}, m7Actor()); !errors.Is(err, ErrShipSeqOverflow) {
		t.Fatalf("★★★ 当日序号超 999 应 ErrShipSeqOverflow，实际 %v", err)
	}
}

// ===== 码类型校验（D1 第 1 项）=====

// 装车只扫成品袋码（T=E）：吨袋码 / 未知码分别拒（400 / 404）。
func TestM7ShipmentBagCodeGuards(t *testing.T) {
	st, cust1, _, outMat := m7Fixture(t)
	ctx := context.Background()

	// 吨袋码（T=B）⇒ 400（B 类深度 2 ⇒ SEQ3 恒 000）
	bagB, err := codec.Generate(codec.Segments{
		T: "B", BT: BizTypeCG, Customer: m7CustCode, Material: m7InMatCode,
		Date: m7BatchDateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成吨袋码失败: %v", err)
	}
	if _, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bagB},
	}, m7Actor()); !errors.Is(err, ErrShipBadInput) {
		t.Fatalf("★★ 非 E 类码应 ErrShipBadInput（400），实际 %v", err)
	}

	// 格式合法但不存在的 E 码 ⇒ ErrCodeUnknown（404）
	batch := m7Batch(t, st, cust1, m7CustCode, outMat)
	lot := m7FgLot(t, st, batch, cust1, m7CustCode, outMat, 1)
	bags := m7FgBags(t, st, lot, 1)
	ghost, err := codec.Generate(codec.Segments{
		T: "E", BT: BizTypeCG, Customer: m7CustCode, Material: m7OutMatCode,
		Date: m7BatchDateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "999",
	})
	if err != nil {
		t.Fatalf("生成幽灵袋码失败: %v", err)
	}
	if _, err := st.CreateShipment(ctx, CreateShipmentInput{
		BagCodes: []string{bags[0].Code, ghost},
	}, m7Actor()); !errors.Is(err, ErrCodeUnknown) {
		t.Fatalf("★★ 未知袋码应 ErrCodeUnknown（404），实际 %v", err)
	}
	// 整单回滚：本用例作用域内不得建成任何出货单
	var ships int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_shipment WHERE created_by = ?`, m7StoreOpenID).
		Scan(&ships); err != nil {
		t.Fatalf("统计出货单失败: %v", err)
	}
	if ships != 0 {
		t.Fatalf("★★★ 被拒后应整单回滚，实际残留 %d 张单", ships)
	}
}
