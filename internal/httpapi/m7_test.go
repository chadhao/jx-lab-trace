package httpapi

// ===== M7 出货 · 接口级 TC（TC-M7-01~06 双层形态的接口侧；库侧见 internal/store/m7_test.go）=====
//
// ★ 账号：receiver（ship.load.scan / ship.out.register = ALL、ship.void.init = INIT、
//	ship.void.approve = NONE、trace.* = READ）＋ management（void.approve = APPROVE、
//	load.scan / out.register = READ）＋ ★ receiver+management 并集账号（自审用：
//	同账号既有 INIT 又有 APPROVE，才进得到业务层「发起人不得自审」的 403）＋
//	sales（ship.void.init = READ —— 撤销留痕读入口挂该点 LevelRead，只有拿到 READ 的角色读得到）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 夹具纪律（§6-19）：按账号精确匹配、先子表后父表（先 item 后 shipment）；
//	断言限定本单作用域。生产批 / 成品批 / 成品袋是上游对象 ⇒ 直写库造最小对象（§5），
//	出货动作本身全部走真实接口。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m7HTTPOpenID   = "ou_test_m7_http" // receiver
	m7HMgmtOpenID  = "ou_test_m7_mgmt" // management（审批）
	m7HBothOpenID  = "ou_test_m7_both" // receiver+management 并集（★ 自审）
	m7HSalesOpenID = "ou_test_m7_sales"
	m7HCustCode    = "9411"
	m7HCust2Code   = "9413"
	m7HInMatCode   = "9411"
	m7HOutMatCode  = "9412"
	m7HDate        = "2026-10-04"
	m7HDateSeg     = "261004"
)

// m7HTTPWipe 清掉 M7 接口测试数据（★ 先子后父、按账号精确匹配，§6-19）。
func m7HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	actors := []interface{}{m7HTTPOpenID, m7HMgmtOpenID, m7HBothOpenID, m7HSalesOpenID}
	q := func(sqlStr string, args ...interface{}) {
		if _, err := st.DB().ExecContext(ctx, sqlStr, args...); err != nil {
			t.Logf("清理失败（忽略，继续）: %v", err)
		}
	}
	q(`DELETE FROM b_shipment_item WHERE shipment_id IN
	       (SELECT id FROM b_shipment WHERE created_by IN (?,?,?,?))`,
		actors...)
	q(`DELETE FROM b_shipment WHERE created_by IN (?,?,?,?)`, actors...)
	q(`DELETE FROM b_fg_bag WHERE fg_lot_id IN (SELECT id FROM b_fg_lot WHERE created_by = ?)`,
		m7HTTPOpenID)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, m7HTTPOpenID)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, m7HTTPOpenID)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?,?,?,?)`, actors...)
	q(`DELETE FROM m_customer WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m7HCustCode, m7HCust2Code)
	q(`DELETE FROM m_material WHERE code IN (?, ?) AND created_by LIKE 'ou\_test\_%'`,
		m7HInMatCode, m7HOutMatCode)
}

// m7HTTPEnv 建测试主数据 + 起服务 + 绑角色；返回 env 与 receiver 的 cookie。
func m7HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m7HTTPWipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M7接口测试客户一', '启用', 1, 1, ?)`, m7HCustCode, m7HTTPOpenID); err != nil {
		t.Fatalf("建测试客户一失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M7接口测试客户二', '启用', 1, 1, ?)`, m7HCust2Code, m7HTTPOpenID); err != nil {
		t.Fatalf("建测试客户二失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M7接口测试原料', '原料', '启用', 1, 1, ?)`, m7HInMatCode, m7HTTPOpenID); err != nil {
		t.Fatalf("建测试原料失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M7接口测试成品', '成品', '启用', 1, 1, ?)`, m7HOutMatCode, m7HTTPOpenID); err != nil {
		t.Fatalf("建测试成品失败: %v", err)
	}
	t.Cleanup(func() { m7HTTPWipe(t, st) })

	bindRole(t, st, m7HTTPOpenID, "receiver")
	bindRole(t, st, m7HMgmtOpenID, "management")
	bindRole(t, st, m7HBothOpenID, "receiver")
	bindRole(t, st, m7HBothOpenID, "management")
	bindRole(t, st, m7HSalesOpenID, "sales")

	e := newEnv(t, time.Hour)
	return e, e.login(m7HTTPOpenID)
}

// m7HMasterIDs 取两个测试客户与原料 / 成品的当前行 id。
func m7HMasterIDs(t *testing.T, st *store.Store) (cust1, cust2, inMat, outMat int64) {
	t.Helper()
	ctx := context.Background()
	byCode := func(table, code string) int64 {
		var id int64
		if err := st.DB().QueryRowContext(ctx,
			`SELECT id FROM `+table+` WHERE code = ? AND is_current = 1`, code).Scan(&id); err != nil {
			t.Fatalf("查 %s %s 失败: %v", table, code, err)
		}
		return id
	}
	return byCode("m_customer", m7HCustCode), byCode("m_customer", m7HCust2Code),
		byCode("m_material", m7HInMatCode), byCode("m_material", m7HOutMatCode)
}

// m7HBatchFixture 直写库造生产批（上游对象，§5 允许夹具）。
func m7HBatchFixture(t *testing.T, st *store.Store, custID int64, custCode string, outMatID int64) int64 {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "C", BT: "CG", Customer: custCode, Material: m7HOutMatCode,
		Date: m7HDateSeg, SEQ1: "01", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成生产批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`, code, custID, 1 /*占位：M7 不读该列*/, outMatID, m7HDate, m7HTTPOpenID)
	if err != nil {
		t.Fatalf("造生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// m7HLotFixture 直写库造成品批（SEQ2 = 批内第 n 个），返回 id 与码。
func m7HLotFixture(t *testing.T, st *store.Store, batchID, custID int64,
	custCode string, outMatID int64, seq2 int) (int64, string) {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "D", BT: "CG", Customer: custCode, Material: m7HOutMatCode,
		Date: m7HDateSeg, SEQ1: "01", SEQ2: fmt.Sprintf("%03d", seq2), SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot (code, batch_id, customer_id, output_material_id, qty_bag, status, created_by)
VALUES (?,?,?,?, 0, '在库', ?)`, code, batchID, custID, outMatID, m7HTTPOpenID)
	if err != nil {
		t.Fatalf("造成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id, code
}

// m7HFgBagsFixture 直写库造 n 个成品袋（段位继承成品批码，SEQ3 依次编号）。
func m7HFgBagsFixture(t *testing.T, st *store.Store, lotID int64, lotCode string, n int) []string {
	t.Helper()
	ctx := context.Background()
	p, err := codec.Parse(lotCode)
	if err != nil {
		t.Fatalf("成品批码不可解析: %v", err)
	}
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		code, err := codec.Generate(codec.Segments{
			T: "E", BT: p.Seg.BT, Customer: p.Seg.Customer, Material: p.Seg.Material,
			Date: p.Seg.Date, SEQ1: p.Seg.SEQ1, SEQ2: p.Seg.SEQ2, SEQ3: fmt.Sprintf("%03d", i),
		})
		if err != nil {
			t.Fatalf("生成成品袋码失败: %v", err)
		}
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO b_fg_bag (code, fg_lot_id, bag_seq, status, created_by)
VALUES (?,?,?, '在库', ?)`, code, lotID, i, m7HTTPOpenID); err != nil {
			t.Fatalf("造成品袋失败: %v", err)
		}
		out = append(out, code)
	}
	return out
}

// m7HShip 建一张出货单（失败即 Fatal），返回 row。
func m7HShip(t *testing.T, e *env, ck string, codes []string) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": codes, "remark": "M7接口夹具"})
	resp, body := e.do("POST", "/api/ship/shipments", ck, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建出货单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("建出货单未返回 row: %v", body)
	}
	return row
}

// m7HItems 查某单明细行数（★ 本单作用域）。
func m7HItems(t *testing.T, e *env, ck string, shipID int64) []map[string]interface{} {
	t.Helper()
	resp, body := e.do("GET", fmt.Sprintf("/api/ship/shipments/%d", shipID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查出货单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	raw, _ := body["items"].([]interface{})
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		out = append(out, m)
	}
	return out
}

// m7HBagStatus 直读成品袋状态（库侧断言）。
func m7HBagStatus(t *testing.T, st *store.Store, code string) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_fg_bag WHERE code = ?`, code).Scan(&s); err != nil {
		t.Fatalf("读成品袋状态失败: %v", err)
	}
	return s
}

// m7HShipStatus 直读出货单状态（库侧断言）。
func m7HShipStatus(t *testing.T, st *store.Store, shipID int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_shipment WHERE id = ?`, shipID).Scan(&s); err != nil {
		t.Fatalf("读出货单状态失败: %v", err)
	}
	return s
}

// ===== TC-M7-01 正常：扫 15 袋归集成一张出货单 =====

func TestTC_M7_01_HTTP_CreateShipment15Bags(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	cust1, _, _, outMat := m7HMasterIDs(t, e.st)
	batch := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot, lotCode := m7HLotFixture(t, e.st, batch, cust1, m7HCustCode, outMat, 1)
	bags := m7HFgBagsFixture(t, e.st, lot, lotCode, 16) // 15 归集 + 1 留给第二张单

	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": bags[:15], "remark": "TC-M7-01"})
	resp, body := e.do("POST", "/api/ship/shipments", ck, string(raw))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫 15 袋建单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	if row == nil {
		t.Fatalf("未返回 row: %v", body)
	}
	no := fmt.Sprint(row["shipment_no"])
	today := time.Now().Format("060102")
	if !regexp.MustCompile(`^CH\d{6}-\d{3}$`).MatchString(no) {
		t.Fatalf("★★ 出货单号应 CH+YYMMDD-NNN，实际 %q", no)
	}
	if !regexp.MustCompile(`^CH` + today + `-\d{3}$`).MatchString(no) {
		t.Fatalf("★★ 单号日期段应为当日 %s，实际 %q", today, no)
	}
	if fmt.Sprint(row["status"]) != "已出厂" {
		t.Fatalf("★★ 建单即「已出厂」，实际 %v", row["status"])
	}
	if row["ship_at"] != nil {
		t.Fatalf("★ 未出场登记时 ship_at 应 null，实际 %v", row["ship_at"])
	}
	if numOf(row["customer_id"]) != cust1 {
		t.Fatalf("客户应由首个袋推定为 %d，实际 %v", cust1, row["customer_id"])
	}

	shipID := numOf(row["id"])
	items := m7HItems(t, e, ck, shipID)
	if len(items) != 15 {
		t.Fatalf("★★★ 本单明细应 15 行，实际 %d", len(items))
	}
	got := map[string]bool{}
	for _, it := range items {
		got[fmt.Sprint(it["bag_code"])] = true
	}
	for _, c := range bags[:15] {
		if !got[c] {
			t.Fatalf("★★ 明细缺袋码 %s", c)
		}
	}
	// ★ 归集不动袋状态（§6-3）
	for _, c := range bags[:15] {
		if s := m7HBagStatus(t, e.st, c); s != "在库" {
			t.Fatalf("★★★ 归集后袋 %s 应仍「在库」，实际 %q", c, s)
		}
	}

	// 串行取号：第二张单序号 = 第一张 + 1
	row2 := m7HShip(t, e, ck, []string{bags[15]})
	no2 := fmt.Sprint(row2["shipment_no"])
	n1, _ := strconv.Atoi(no[len(no)-3:])
	n2, _ := strconv.Atoi(no2[len(no2)-3:])
	if n2 != n1+1 {
		t.Fatalf("★★ 当日序号应串行递增：%s → %s", no, no2)
	}
}

// ===== TC-M7-02 ★ 异常：同一个成品袋扫进第二张出货单 =====

func TestTC_M7_02_HTTP_CrossShipmentRejected(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	cust1, _, _, outMat := m7HMasterIDs(t, e.st)
	batch := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot, lotCode := m7HLotFixture(t, e.st, batch, cust1, m7HCustCode, outMat, 1)
	bags := m7HFgBagsFixture(t, e.st, lot, lotCode, 2)

	shipA := m7HShip(t, e, ck, []string{bags[0]})

	// ① 建单 B 时再归集同袋 ⇒ 409
	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": []string{bags[1], bags[0]}})
	resp, body := e.do("POST", "/api/ship/shipments", ck, string(raw))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 跨单重复归集应 409，实际 %d body=%v", resp.StatusCode, body)
	}
	if msg := fmt.Sprint(body["error"]); !strings.Contains(msg, "已归集") {
		t.Fatalf("★★ 拒绝原因应含「已归集」，实际 %q", msg)
	}

	// ② 向已存在的单 B 追加同袋 ⇒ 409
	shipB := m7HShip(t, e, ck, []string{bags[1]})
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/items", numOf(shipB["id"])), ck,
		fmt.Sprintf(`{"bag_code":%q}`, bags[0]))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 向第二张单追加已归集袋应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// 被拒后两单明细均不变
	if n := len(m7HItems(t, e, ck, numOf(shipA["id"]))); n != 1 {
		t.Fatalf("单 A 明细应 1 行，实际 %d", n)
	}
	if n := len(m7HItems(t, e, ck, numOf(shipB["id"]))); n != 1 {
		t.Fatalf("★★ 被拒后单 B 明细应仍 1 行，实际 %d", n)
	}
}

// ===== TC-M7-03 边界：跨成品批允许 / 跨客户拆单 =====

func TestTC_M7_03_HTTP_CrossLotAllowedCrossCustomerRejected(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	cust1, cust2, _, outMat := m7HMasterIDs(t, e.st)
	batch1 := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot1, code1 := m7HLotFixture(t, e.st, batch1, cust1, m7HCustCode, outMat, 1)
	lot2, code2 := m7HLotFixture(t, e.st, batch1, cust1, m7HCustCode, outMat, 2)
	b1 := m7HFgBagsFixture(t, e.st, lot1, code1, 2)
	b2 := m7HFgBagsFixture(t, e.st, lot2, code2, 2)

	// 跨成品批、同客户 ⇒ 允许
	ship := m7HShip(t, e, ck, []string{b1[0], b2[0]})
	items := m7HItems(t, e, ck, numOf(ship["id"]))
	lots := map[int64]bool{}
	for _, it := range items {
		lots[numOf(it["fg_lot_id"])] = true
	}
	if len(lots) != 2 {
		t.Fatalf("★★ 明细应来自 2 个成品批，实际 %d", len(lots))
	}

	// 跨客户 ⇒ 拒（400）
	batch2 := m7HBatchFixture(t, e.st, cust2, m7HCust2Code, outMat)
	lot3, code3 := m7HLotFixture(t, e.st, batch2, cust2, m7HCust2Code, outMat, 1)
	b3 := m7HFgBagsFixture(t, e.st, lot3, code3, 2)

	raw, _ := json.Marshal(map[string]interface{}{"bag_codes": []string{b1[1], b3[0]}})
	resp, body := e.do("POST", "/api/ship/shipments", ck, string(raw))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★★ 跨客户建单应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	if msg := fmt.Sprint(body["error"]); !strings.Contains(msg, "拆单") {
		t.Fatalf("★★ 拒绝原因应提示拆单，实际 %q", msg)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/items", numOf(ship["id"])), ck,
		fmt.Sprintf(`{"bag_code":%q}`, b3[1]))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★★ 跨客户追加应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	// 被拒后本单明细不增
	if n := len(m7HItems(t, e, ck, numOf(ship["id"]))); n != 2 {
		t.Fatalf("被拒后本单明细应仍 2 行，实际 %d", n)
	}
}

// ===== TC-M7-04 正常：出场登记 =====

func TestTC_M7_04_HTTP_DepartShipment(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	cust1, _, _, outMat := m7HMasterIDs(t, e.st)
	batch := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot, lotCode := m7HLotFixture(t, e.st, batch, cust1, m7HCustCode, outMat, 1)
	bags := m7HFgBagsFixture(t, e.st, lot, lotCode, 3)
	ship := m7HShip(t, e, ck, bags)
	shipID := numOf(ship["id"])

	// 必填校验
	resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", shipID), ck,
		`{"driver":"张三"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 缺 plate_no 应 400，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", shipID), ck,
		`{"plate_no":"湘A·1"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 缺 driver 应 400，实际 %d body=%v", resp.StatusCode, body)
	}

	// 正常登记
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", shipID), ck,
		`{"plate_no":"湘A·76543","driver":"李四","ship_at":"2026-10-04 08:00:00","operator":"王五"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("出场登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	// 库侧断言：ship_at / 车牌 / 司机落库；袋全部转「已出厂」
	var shipAt interface{}
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT ship_at FROM b_shipment WHERE id = ?`, shipID).Scan(&shipAt); err != nil || shipAt == nil {
		t.Fatalf("★★ ship_at 应落库: err=%v v=%v", err, shipAt)
	}
	var plate, driver string
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT plate_no, driver FROM b_shipment WHERE id = ?`, shipID).Scan(&plate, &driver); err != nil {
		t.Fatalf("读出场信息失败: %v", err)
	}
	if plate != "湘A·76543" || driver != "李四" {
		t.Fatalf("车牌 / 司机应落库，实际 %q / %q", plate, driver)
	}
	for _, c := range bags {
		if s := m7HBagStatus(t, e.st, c); s != "已出厂" {
			t.Fatalf("★★★ 出场登记后袋 %s 应「已出厂」，实际 %q", c, s)
		}
	}

	// 重复登记 ⇒ 409
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", shipID), ck,
		`{"plate_no":"湘A·76543","driver":"李四"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 重复出场登记应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// 空单 ⇒ 409（直插一张无明细的单）
	no := fmt.Sprintf("CH%s-980", time.Now().Format("060102"))
	res, err := e.st.DB().ExecContext(context.Background(), `
INSERT INTO b_shipment (shipment_no, customer_id, status, created_by)
VALUES (?,?, '已出厂', ?)`, no, cust1, m7HTTPOpenID)
	if err != nil {
		t.Fatalf("造空单失败: %v", err)
	}
	emptyID, _ := res.LastInsertId()
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", emptyID), ck,
		`{"plate_no":"湘A·00000","driver":"赵六"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 空单出场登记应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== TC-M7-05 ★ 异常：撤销发起人自己审批 =====

func TestTC_M7_05_HTTP_VoidInitApproveSelfRejected(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	both := e.login(m7HBothOpenID)
	cust1, _, _, outMat := m7HMasterIDs(t, e.st)
	batch := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot, lotCode := m7HLotFixture(t, e.st, batch, cust1, m7HCustCode, outMat, 1)
	bags := m7HFgBagsFixture(t, e.st, lot, lotCode, 4)

	ship := m7HShip(t, e, ck, bags[:2])
	shipID := numOf(ship["id"])
	if resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/depart", shipID), ck,
		`{"plate_no":"湘A·11111","driver":"钱七"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("出场登记应 200，实际 %d body=%v", resp.StatusCode, body)
	}

	// reason 必填
	resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void", shipID), ck,
		`{"reason":"  "}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 缺 reason 应 400，实际 %d body=%v", resp.StatusCode, body)
	}

	// 发起（receiver 的 INIT 级）⇒ 不生效（A6）
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void", shipID), ck,
		`{"reason":"错发重开"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("撤销发起应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if s := m7HShipStatus(t, e.st, shipID); s != "已出厂" {
		t.Fatalf("★★★ 发起不生效：状态应仍「已出厂」，实际 %q", s)
	}
	for _, c := range bags[:2] {
		if s := m7HBagStatus(t, e.st, c); s != "已出厂" {
			t.Fatalf("★★★ 发起不生效：袋 %s 应仍「已出厂」，实际 %q", c, s)
		}
	}
	// 重复发起 ⇒ 409
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void", shipID), ck,
		`{"reason":"再发一次"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 重复发起应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// ★ receiver 在 ship.void.approve = NONE ⇒ 走审批入口被权限守卫 403（状态不变）
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void/approve", shipID), ck, `{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ receiver 走审批入口应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	if s := m7HShipStatus(t, e.st, shipID); s != "已出厂" {
		t.Fatalf("被 403 拒绝后状态应仍「已出厂」，实际 %q", s)
	}

	// ★★ TC-M7-05 落点：并集账号（INIT+APPROVE）自己发起再自己审批 ⇒ 403（业务层自审拒绝）
	ship2 := m7HShip(t, e, both, bags[2:])
	ship2ID := numOf(ship2["id"])
	if resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void", ship2ID), both,
		`{"reason":"自审用例"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("并集账号发起应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void/approve", ship2ID), both, `{}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★★ 发起人自审应 403，实际 %d body=%v", resp.StatusCode, body)
	}
	if msg := fmt.Sprint(body["error"]); !strings.Contains(msg, "不得自行审批") {
		t.Fatalf("★★ 自审拒绝原因应含「不得自行审批」，实际 %q", msg)
	}
	if s := m7HShipStatus(t, e.st, ship2ID); s != "已出厂" {
		t.Fatalf("★★ 自审被拒后状态应仍「已出厂」，实际 %q", s)
	}
}

// ===== TC-M7-06 正常：撤销后查成品袋，状态回退可再出货 =====

func TestTC_M7_06_HTTP_RecollectAfterVoid(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	mgmt := e.login(m7HMgmtOpenID)
	sales := e.login(m7HSalesOpenID)
	cust1, _, _, outMat := m7HMasterIDs(t, e.st)
	batch := m7HBatchFixture(t, e.st, cust1, m7HCustCode, outMat)
	lot, lotCode := m7HLotFixture(t, e.st, batch, cust1, m7HCustCode, outMat, 1)
	bags := m7HFgBagsFixture(t, e.st, lot, lotCode, 2)

	shipA := m7HShip(t, e, ck, bags)
	shipAID := numOf(shipA["id"])

	// 未发起即审批 ⇒ 409（management 有 APPROVE，能进业务层）
	resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void/approve", shipAID), mgmt, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 未发起即审批应 409，实际 %d body=%v", resp.StatusCode, body)
	}

	// 发起（receiver）→ 审批（management，两人不同）⇒ 生效
	if resp, body := e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void", shipAID), ck,
		`{"reason":"装错车"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("发起撤销应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/void/approve", shipAID), mgmt, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("他人审批应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if s := m7HShipStatus(t, e.st, shipAID); s != "已撤销" {
		t.Fatalf("★★ 审批后应「已撤销」，实际 %q", s)
	}
	for _, c := range bags {
		if s := m7HBagStatus(t, e.st, c); s != "在库" {
			t.Fatalf("★★ 审批后袋 %s 应回退「在库」，实际 %q", c, s)
		}
	}
	// 明细保留（不物理删除，§6-21）
	if n := len(m7HItems(t, e, ck, shipAID)); n != 2 {
		t.Fatalf("★★★ 撤销后明细应保留 2 行，实际 %d", n)
	}
	// 撤销留痕：sales（ship.void.init=READ）读得到 init+approve 两笔
	resp, body = e.do("GET", fmt.Sprintf("/api/ship/shipments/%d/void-records", shipAID), sales, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("读撤销留痕应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if numOf(body["count"]) != 2 {
		t.Fatalf("★★ 撤销留痕应 2 笔，实际 %v", body["count"])
	}

	// ★ TC-M7-06：撤销后可再归集进新单
	shipB := m7HShip(t, e, ck, bags)
	if n := len(m7HItems(t, e, ck, numOf(shipB["id"]))); n != 2 {
		t.Fatalf("★★★ 撤销后再归集应允许，新单明细应 2 行，实际 %d", n)
	}
	// 原单明细仍保留（两单并存，靠状态区分有效性）
	if n := len(m7HItems(t, e, ck, shipAID)); n != 2 {
		t.Fatalf("原单明细应仍 2 行，实际 %d", n)
	}
	// 已撤销的单不得再扫码 ⇒ 409
	resp, body = e.do("POST", fmt.Sprintf("/api/ship/shipments/%d/items", shipAID), ck,
		fmt.Sprintf(`{"bag_code":%q}`, bags[0]))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★★ 已撤销的单再扫码应 409，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ===== A14（接口侧）· 7 个点在路由层被消费且级别正确 =====

func TestM7HTTPPermLevels(t *testing.T) {
	e, ck := m7HTTPEnv(t)
	mgmt := e.login(m7HMgmtOpenID)

	// perm-summary：receiver 应拿到 7 个点（4 ship + 3 trace）
	resp, body := e.do("GET", "/api/ship/perm-summary", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("perm-summary 应 200，实际 %d", resp.StatusCode)
	}
	points, _ := body["points"].(map[string]interface{})
	for _, code := range []string{
		"ship.load.scan", "ship.out.register", "ship.void.init", "ship.void.approve",
		"trace.forward", "trace.backward", "trace.batch.view",
	} {
		v, ok := points[code]
		if !ok {
			t.Fatalf("★★ perm-summary 缺少 %s：%v", code, points)
		}
		if fmt.Sprint(v) == "NONE" {
			t.Fatalf("★ receiver 在 %s 上不应是 NONE，实际 %v", code, v)
		}
	}

	// 未登录 ⇒ 401
	if resp, _ := e.do("POST", "/api/ship/shipments", "", `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录建单应 401，实际 %d", resp.StatusCode)
	}

	// ★ management 对 ship.load.scan = READ ⇒ 读 200 / 写 403（LevelAll 守卫）
	if resp, body := e.do("GET", "/api/ship/shipments", mgmt, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("management 读出货单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	if resp, body := e.do("POST", "/api/ship/shipments", mgmt, `{"bag_codes":[]}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ management（READ）建单应 403，实际 %d body=%v", resp.StatusCode, body)
	}

	// ★ management 对 ship.void.init = NONE ⇒ 发起入口 403（LevelInit 守卫）
	resp, body = e.do("POST", "/api/ship/shipments/1/void", mgmt, `{"reason":"管理层发起"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ management（NONE）发起撤销应 403，实际 %d body=%v", resp.StatusCode, body)
	}
}

// ---- 小工具 ----
