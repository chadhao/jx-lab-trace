package httpapi

// ===== M5 检测 · 接口级 TC（TC-M5-01~14）=====
//
// ★ 账号：qc（insp.* 写入链路 ALL + urgent init INIT + sample.take + receiver 收货链路）
//	＋ management（urgent approve APPROVE）＋ production（dept_sign ALL）。
// ★ 按 docs/05：DB 集成测试，本机未设 JX_TEST_DB=1 一律 Skip。
// ★ 夹具纪律（§6-23）：清理**按账号精确匹配**、**先子表后父表**；
//	断言「某表有几个」一律**限定本用例作用域**，不全库 COUNT(*)。
// ★ 生产批 / 成品批属批 6 —— 按 §5 允许**直写库造最小对象**（不建批入口）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/store"
)

const (
	m5HTTPOpenID = "ou_test_m5_http" // qc + receiver
	m5MgmtOpenID = "ou_test_m5_mgmt" // management
	m5ProdOpenID = "ou_test_m5_prod" // production
	m5CustCode   = "9403"
	m5MatCode    = "9403"
	m5Date       = "2026-10-09"
	m5DateSeg    = "261009"
)

// m5HTTPWipe 清掉 M5 接口测试数据。
//
// ★ 顺序硬约束（先子后父）：附件 → 清单 → 作废 → 检测单 → 销毁 → 借还 → 留样
//
//	→ 样品 → 取样组 → 袋作废 → 袋 → 车 → 预报 → 标签 → 生产批 → 成品批
//	→ 审计（本批账号） → 判定限 → 检测项字典 → 主数据。
func m5HTTPWipe(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	me := m5HTTPOpenID
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
	q(`DELETE FROM b_bag WHERE created_by = ?
	     OR truck_lot_id IN (SELECT id FROM b_truck_lot
	                           WHERE customer_id IN (SELECT id FROM m_customer WHERE code = ?))`,
		me, m5CustCode)
	q(`DELETE FROM b_truck_lot WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m5CustCode)
	q(`DELETE FROM b_arrival_notice WHERE created_by = ?
	     OR customer_id IN (SELECT id FROM m_customer WHERE code = ?)`, me, m5CustCode)
	q(`DELETE FROM b_label_print WHERE created_by = ?`, me)
	q(`DELETE FROM b_production_batch WHERE created_by = ?`, me)
	q(`DELETE FROM b_fg_lot WHERE created_by = ?`, me)
	q(`DELETE FROM s_audit_log WHERE actor_open_id IN (?,?,?)`, me, m5MgmtOpenID, m5ProdOpenID)
	q(`DELETE FROM m_test_item_limit WHERE created_by = ?`, me)
	q(`DELETE FROM m_test_item WHERE created_by = ?`, me)
	q(`DELETE FROM m_customer WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m5CustCode)
	q(`DELETE FROM m_material WHERE code = ? AND created_by LIKE 'ou\_test\_%'`, m5MatCode)
}

// m5HTTPEnv 建测试主数据 + 检测项字典 + 判定限 + 起服务 + 绑角色；
// 返回 env 与 qc 的 cookie。
func m5HTTPEnv(t *testing.T) (*env, string) {
	t.Helper()
	st := requireDB(t)
	m5HTTPWipe(t, st)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_customer (code, name, status, version, is_current, created_by)
VALUES (?, 'M5接口测试客户', '启用', 1, 1, ?)`, m5CustCode, m5HTTPOpenID); err != nil {
		t.Fatalf("建测试客户失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_material (code, name, kind, status, version, is_current, created_by)
VALUES (?, 'M5接口测试原料', '原料', '启用', 1, 1, ?)`, m5MatCode, m5HTTPOpenID); err != nil {
		t.Fatalf("建测试物料失败: %v", err)
	}

	// 检测项字典：6 个数值项（TC-M5-01 的 5 项 + 追加项）+ 1 个文本项
	for i := 1; i <= 7; i++ {
		vt := "数值"
		if i == 7 {
			vt = "文本"
		}
		if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_test_item (code, name, unit, method, value_type, status, version, is_current, created_by)
VALUES (?,?, '%','GB/T 2007',?, '启用', 1, 1, ?)`,
			fmt.Sprintf("m5ti%02d", i), fmt.Sprintf("M5检测项%d", i), vt, m5HTTPOpenID); err != nil {
			t.Fatalf("建检测项失败: %v", err)
		}
	}
	// 判定限：项1 × (测试客户, 测试物料) = [0, 100]
	var item1 int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_test_item WHERE code = 'm5ti01' AND is_current = 1`).Scan(&item1); err != nil {
		t.Fatalf("查检测项失败: %v", err)
	}
	var custID, matID int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m5CustCode).Scan(&custID); err != nil {
		t.Fatalf("查客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m5MatCode).Scan(&matID); err != nil {
		t.Fatalf("查物料失败: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `
INSERT INTO m_test_item_limit
  (test_item_id, customer_id, material_id, lower_limit, upper_limit, status, version, is_current, created_by)
VALUES (?,?,?,0,100,'启用',1,1,?)`, item1, custID, matID, m5HTTPOpenID); err != nil {
		t.Fatalf("建判定限失败: %v", err)
	}

	t.Cleanup(func() { m5HTTPWipe(t, st) })

	// qc 走完整检测链路 + 收货/取样（造车与大样）；management 批紧急放行；production 双签
	bindRole(t, st, m5HTTPOpenID, "qc")
	bindRole(t, st, m5HTTPOpenID, "receiver")
	bindRole(t, st, m5MgmtOpenID, "management")
	bindRole(t, st, m5ProdOpenID, "production")

	e := newEnv(t, time.Hour)
	e.cfg.AttachDir = t.TempDir() // 附件落盘隔离（断言「文件在附件目录」）
	return e, e.login(m5HTTPOpenID)
}

// m5ItemID 取测试检测项 id。
func m5ItemID(t *testing.T, st *store.Store, code string) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT id FROM m_test_item WHERE code = ? AND is_current = 1`, code).Scan(&id); err != nil {
		t.Fatalf("查检测项 %s 失败: %v", code, err)
	}
	return id
}

// m5Truck 造一个车次（预报 → 到货 → 过磅 → 袋码），返回车次 id 与袋行。
func m5Truck(t *testing.T, e *env, ck string, custID, matID int64) (int64, []map[string]interface{}) {
	t.Helper()
	truck := m3ArriveViaHTTP(t, e, ck, custID, matID, 2)
	id := int64(numOf(truck["id"]))
	bags := m3WeighAndBags(t, e, ck, id, "35000", "5000", 2)
	return id, bags
}

// m5Group 对车次建取样组（大样）：扫指定袋取份样 → 建组，返回 group_id。
func m5Group(t *testing.T, e *env, ck string, truckID int64, bagCode string) int64 {
	t.Helper()
	resp, body := e.do("POST", "/api/sample/take", ck, fmt.Sprintf(`{"code":%q}`, bagCode))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("扫码取样应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	samples, _ := body["samples"].([]interface{})
	var incID int64
	for _, it := range samples {
		m, _ := it.(map[string]interface{})
		if fmt.Sprint(m["role"]) == "份样" {
			incID = numOf(m["id"])
		}
	}
	if incID == 0 {
		t.Fatalf("未取到份样: %v", body)
	}
	return m5CreateGroup(t, e, ck, truckID, incID)
}

func m5CreateGroup(t *testing.T, e *env, ck string, truckID, sampleID int64) int64 {
	t.Helper()
	resp, body := e.do("POST", "/api/sample/groups", ck,
		fmt.Sprintf(`{"target_type":"车次","target_id":%d,"sample_ids":[%d],"remark":"M5夹具"}`,
			truckID, sampleID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建取样组应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	g, _ := body["row"].(map[string]interface{})
	return numOf(g["id"])
}

// m5BatchFixture 直写库造生产批（批 6 对象，§5 允许夹具）。
func m5BatchFixture(t *testing.T, st *store.Store, custID, matID int64) int64 {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "C", BT: "CG", Customer: m5CustCode, Material: m5MatCode,
		Date: m5DateSeg, SEQ1: "01", SEQ2: "000", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成生产批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, created_by)
VALUES (?,?,?,?,?, '进行中', ?)`, code, custID, matID, matID, m5Date, m5HTTPOpenID)
	if err != nil {
		t.Fatalf("造生产批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// m5FgFixture 直写库造成品批。
func m5FgFixture(t *testing.T, st *store.Store, custID, matID, batchID int64) int64 {
	t.Helper()
	code, err := codec.Generate(codec.Segments{
		T: "D", BT: "CG", Customer: m5CustCode, Material: m5MatCode,
		Date: m5DateSeg, SEQ1: "01", SEQ2: "001", SEQ3: "000",
	})
	if err != nil {
		t.Fatalf("生成成品批码失败: %v", err)
	}
	res, err := st.DB().ExecContext(context.Background(), `
INSERT INTO b_fg_lot
  (code, batch_id, customer_id, output_material_id, status, created_by)
VALUES (?,?,?,?, '在库', ?)`, code, batchID, custID, matID, m5HTTPOpenID)
	if err != nil {
		t.Fatalf("造成品批失败: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// m5CreateInsp 走接口建检测单，返回单 id。
func m5CreateInsp(t *testing.T, e *env, ck, targetType string, targetID int64) int64 {
	t.Helper()
	resp, body := e.do("POST", "/api/insp", ck,
		fmt.Sprintf(`{"target_type":%q,"target_id":%d}`, targetType, targetID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建检测单应 200，实际 %d body=%v", resp.StatusCode, body)
	}
	row, _ := body["row"].(map[string]interface{})
	return numOf(row["id"])
}

// m5MasterIDs 取测试客户/物料当前行 id。
func m5MasterIDs(t *testing.T, st *store.Store) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	var cust, mat int64
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_customer WHERE code = ? AND is_current = 1`, m5CustCode).Scan(&cust); err != nil {
		t.Fatalf("查测试客户失败: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT id FROM m_material WHERE code = ? AND is_current = 1`, m5MatCode).Scan(&mat); err != nil {
		t.Fatalf("查测试物料失败: %v", err)
	}
	return cust, mat
}

// m5Upload 以 multipart/form-data 上传附件（size 字节），返回状态码与 JSON 体。
func m5Upload(t *testing.T, e *env, ck string, inspID int64, fileName string, size int) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	chunk := make([]byte, 1<<20)
	for remain := size; remain > 0; {
		n := len(chunk)
		if remain < n {
			n = remain
		}
		if _, err := fw.Write(chunk[:n]); err != nil {
			t.Fatalf("写 multipart 失败: %v", err)
		}
		remain -= n
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭 multipart 失败: %v", err)
	}
	req, err := http.NewRequest("POST",
		e.srv.URL+fmt.Sprintf("/api/insp/%d/files", inspID), &buf)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Cookie", ck)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("上传请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// m5InspResults 归一化快照某单全部 result 行（id/项/态/值/判定/限），供逐字比对。
func m5InspResults(t *testing.T, st *store.Store, inspID int64) string {
	t.Helper()
	rows, err := st.DB().QueryContext(context.Background(), `
SELECT id, item_id, state, IFNULL(CAST(value_num AS CHAR), ''), IFNULL(value_text, ''),
       IFNULL(judge, ''), IFNULL(CAST(lower_limit AS CHAR), ''), IFNULL(CAST(upper_limit AS CHAR), '')
  FROM b_inspection_result WHERE inspection_id = ? ORDER BY id`, inspID)
	if err != nil {
		t.Fatalf("读取清单快照失败: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, itemID int64
		var state, vnum, vtext, judge, lo, hi string
		if err := rows.Scan(&id, &itemID, &state, &vnum, &vtext, &judge, &lo, &hi); err != nil {
			t.Fatalf("扫描清单快照失败: %v", err)
		}
		fmt.Fprintf(&sb, "%d|%d|%s|%s|%s|%s|%s|%s\n", id, itemID, state, vnum, vtext, judge, lo, hi)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取清单快照失败: %v", err)
	}
	return sb.String()
}

// m5TruckStatus 查车次状态。
func m5TruckStatus(t *testing.T, st *store.Store, truckID int64) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT status FROM b_truck_lot WHERE id = ?`, truckID).Scan(&s); err != nil {
		t.Fatalf("读车次状态失败: %v", err)
	}
	return s
}

// m5DBField 查检测单某列（通用字符串列）。
func m5DBField(t *testing.T, st *store.Store, inspID int64, col string) string {
	t.Helper()
	var s sql.NullString
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT `+col+` FROM b_inspection WHERE id = ?`, inspID).Scan(&s); err != nil {
		t.Fatalf("读检测单列 %s 失败: %v", col, err)
	}
	if !s.Valid {
		return ""
	}
	return s.String
}

// TC-M5-01 正常：为某车次生成含 5 项的清单 ⇒ 恰好 5 行 state='未测'（按本单作用域计数）。
func TestTC_M5_01_HTTP_AddFiveItemsAllTodo(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	ids := make([]int64, 0, 5)
	for i := 1; i <= 5; i++ {
		ids = append(ids, m5ItemID(t, e.st, fmt.Sprintf("m5ti%02d", i)))
	}
	body, _ := json.Marshal(map[string]interface{}{"item_ids": ids})
	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("加 5 项应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	resp, out = e.do("GET", fmt.Sprintf("/api/insp/%d/items", inspID), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查清单应 200，实际 %d", resp.StatusCode)
	}
	if numOf(out["count"]) != 5 {
		t.Fatalf("★ 清单应恰 5 行（本单作用域），实际 %v", out["count"])
	}
	if numOf(out["todo"]) != 5 {
		t.Fatalf("待办数应 5，实际 %v", out["todo"])
	}
	rows, _ := out["rows"].([]interface{})
	for _, it := range rows {
		m, _ := it.(map[string]interface{})
		if m["state"] != "未测" {
			t.Fatalf("新行 state 应「未测」，实际 %v（%v）", m["state"], m)
		}
	}
}

// TC-M5-02 异常 + 删项审计：已录值项删除 ⇒ 拒绝（409）；未测项删除 ⇒ 允许且审计留痕。
func TestTC_M5_02_HTTP_DeleteRecordedItemRejected(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	item1 := m5ItemID(t, e.st, "m5ti01")
	item2 := m5ItemID(t, e.st, "m5ti02")

	body, _ := json.Marshal(map[string]interface{}{"item_ids": []int64{item1, item2}})
	if resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("加项应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	// 录入 item1
	if resp, out := e.do("PATCH",
		fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":42.5}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("录入应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// ★ 已录值 ⇒ 拒绝
	resp, out := e.do("DELETE", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★ 删已录值项应 409，实际 %d body=%v", resp.StatusCode, out)
	}

	// ★ 未测项 ⇒ 允许（物理 DELETE）+ 审计 action='delete'
	resp, out = e.do("DELETE", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item2), ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删未测项应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, out = e.do("GET", fmt.Sprintf("/api/insp/%d/items", inspID), ck, "")
	if numOf(out["count"]) != 1 {
		t.Fatalf("删后清单应剩 1 行，实际 %v", out["count"])
	}

	// 审计：entity=b_inspection_result 有 action='delete' 且 old_value 记快照
	var delCount int
	var oldVal string
	rows, err := e.st.DB().QueryContext(context.Background(), `
SELECT IFNULL(old_value,'') FROM s_audit_log
 WHERE action = 'delete' AND actor_open_id = ? ORDER BY id DESC`, m5HTTPOpenID)
	if err != nil {
		t.Fatalf("查删项审计失败: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("扫描删项审计失败: %v", err)
		}
		if strings.Contains(v, fmt.Sprintf("item=%d", item2)) {
			delCount++
			oldVal = v
		}
	}
	if delCount == 0 {
		t.Fatalf("★ 删未测项必须写审计（action=delete，old_value 记快照）")
	}
	if !strings.Contains(oldVal, "state=未测") {
		t.Fatalf("删项审计 old_value 应含 item 快照，实际 %q", oldVal)
	}
}

// TC-M5-03 正常：某项已录值时追加新项 ⇒ 允许。
func TestTC_M5_03_HTTP_AddItemWhileRecorded(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	item1 := m5ItemID(t, e.st, "m5ti01")
	item2 := m5ItemID(t, e.st, "m5ti02")

	body, _ := json.Marshal(map[string]interface{}{"item_ids": []int64{item1}})
	if resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("加项应 200，实际 %d %v", resp.StatusCode, out)
	}
	if resp, out := e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":1}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("录入应 200，实际 %d %v", resp.StatusCode, out)
	}

	// ★ 已录值后追加 ⇒ 允许
	body, _ = json.Marshal(map[string]interface{}{"item_ids": []int64{item2}})
	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("★ 已录值时追加新项应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if numOf(out["count"]) != 2 {
		t.Fatalf("清单应 2 行，实际 %v", out["count"])
	}
}

// TC-M5-04 正常：三项分别 已测 / 不适用 / 未测 ⇒ 三态各自可区分，未测计入待办。
// ★ 顺带断言判定限快照（[0,100] 命中 ⇒ lower/upper 落行、judge=合格）。
func TestTC_M5_04_HTTP_ThreeStatesDistinguishable(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	item1 := m5ItemID(t, e.st, "m5ti01")
	item2 := m5ItemID(t, e.st, "m5ti02")
	item3 := m5ItemID(t, e.st, "m5ti03")

	body, _ := json.Marshal(map[string]interface{}{"item_ids": []int64{item1, item2, item3}})
	if resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("加项应 200，实际 %d %v", resp.StatusCode, out)
	}
	// item1 ⇒ 已测（限 [0,100]，50 在界内）
	resp, out := e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":50}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("录入已测应 200，实际 %d %v", resp.StatusCode, out)
	}
	// item2 ⇒ 不适用（值必须留空）
	if resp, out = e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item2), ck,
		`{"state":"不适用","value_num":99,"value_text":"x"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("录入不适用应 200，实际 %d %v", resp.StatusCode, out)
	}
	// item3 ⇒ 保持未测

	resp, out = e.do("GET", fmt.Sprintf("/api/insp/%d/items", inspID), ck, "")
	if numOf(out["todo"]) != 1 {
		t.Fatalf("★ 待办数应 = 未测行数 = 1，实际 %v", out["todo"])
	}
	byItem := map[int64]map[string]interface{}{}
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		byItem[numOf(m["item_id"])] = m
	}
	if s := fmt.Sprint(byItem[item1]["state"]); s != "已测" {
		t.Fatalf("item1 应 已测，实际 %s", s)
	}
	if s := fmt.Sprint(byItem[item2]["state"]); s != "不适用" {
		t.Fatalf("item2 应 不适用，实际 %s", s)
	}
	if s := fmt.Sprint(byItem[item3]["state"]); s != "未测" {
		t.Fatalf("item3 应 未测，实际 %s", s)
	}
	// 不适用 ⇒ 值/判定留空
	if _, has := byItem[item2]["value_num"]; has && byItem[item2]["value_num"] != nil {
		t.Fatalf("不适用行 value_num 必须留空，实际 %v", byItem[item2]["value_num"])
	}
	if j := fmt.Sprint(byItem[item2]["judge"]); j != "" && j != "<nil>" {
		t.Fatalf("不适用行 judge 必须留空，实际 %q", j)
	}
	// 判定限快照 + judge
	if byItem[item1]["lower_limit"] == nil || byItem[item1]["upper_limit"] == nil {
		t.Fatalf("已测数值行应有判定限快照，实际 %v", byItem[item1])
	}
	if j := fmt.Sprint(byItem[item1]["judge"]); j != "合格" {
		t.Fatalf("50 ∈ [0,100] ⇒ judge=合格，实际 %q", j)
	}
}

// TC-M5-05 边界：上传 20MB 附件 ⇒ 成功；库中只有路径、文件在附件目录。
func TestTC_M5_05_HTTP_Upload20MBAllowed(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	const mb = 1 << 20
	status, out := m5Upload(t, e, ck, inspID, "20mb.bin", 20*mb)
	if status != http.StatusOK {
		t.Fatalf("★ 20MB 附件应通过（200），实际 %d body=%v", status, out)
	}
	row, _ := out["row"].(map[string]interface{})
	path := fmt.Sprint(row["file_path"])
	if path == "" || path == "<nil>" {
		t.Fatalf("file_path 必须非空: %v", row)
	}
	if !strings.HasPrefix(path, e.cfg.AttachDir) {
		t.Fatalf("文件应落在 AttachDir（%s）内，实际 %s", e.cfg.AttachDir, path)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("附件应真实落盘: %v", err)
	}
	if st.Size() != 20*mb {
		t.Fatalf("落盘大小应 20MB，实际 %d", st.Size())
	}
	// 库中只有路径（b_inspection_file 无内容列；断言行存在且 size 正确）
	var rowCount int
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_inspection_file WHERE inspection_id = ? AND file_path = ?`,
		inspID, path).Scan(&rowCount); err != nil || rowCount != 1 {
		t.Fatalf("附件行应登记 1 条，实际 %d err=%v", rowCount, err)
	}
}

// TC-M5-06 异常：上传 21MB 附件 ⇒ 拒绝（413，先拒后写、不留半截文件）。
func TestTC_M5_06_HTTP_Upload21MBRejected(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	const mb = 1 << 20
	status, out := m5Upload(t, e, ck, inspID, "21mb.bin", 21*mb)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("★ 21MB 附件应 413，实际 %d body=%v", status, out)
	}
	var rowCount int
	if err := e.st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM b_inspection_file WHERE inspection_id = ?`, inspID).Scan(&rowCount); err != nil {
		t.Fatalf("查附件行失败: %v", err)
	}
	if rowCount != 0 {
		t.Fatalf("被拒后不得登记附件行，实际 %d", rowCount)
	}
	// 不留半截文件
	entries, err := os.ReadDir(e.cfg.AttachDir)
	if err != nil {
		t.Fatalf("读附件目录失败: %v", err)
	}
	for _, en := range entries {
		if strings.Contains(en.Name(), "21mb") {
			t.Fatalf("被拒后不得留半截文件: %s", en.Name())
		}
	}
}

// TC-M5-07 正常：结论设为让步 ⇒ 持久化为 CONCESSION（不是「让步接收」）。
func TestTC_M5_07_HTTP_ConclusionPersistsConcessionCode(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", inspID), ck, `{
		"conclusion": "CONCESSION",
		"authorized_by": "质量经理",
		"cust_notified_at": "2026-10-09 09:00:00",
		"cust_contact": "客户质检 王工",
		"cust_channel": "电话"
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("让步结论应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	if got := m5DBField(t, e.st, inspID, "conclusion"); got != "CONCESSION" {
		t.Fatalf("★ 持久化应恰为 CONCESSION，实际 %q", got)
	}
	// 车次状态回写为「让步接收」
	if s := m5TruckStatus(t, e.st, truckID); s != "让步接收" {
		t.Fatalf("让步后车次状态应「让步接收」，实际 %q", s)
	}
}

// TC-M5-08 ★★ 异常：让步四字段**逐个留空**各测一次 ⇒ 均拒绝（400）。
func TestTC_M5_08_HTTP_ConcessionFourFieldsEachRequired(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	full := map[string]string{
		"authorized_by":    "质量经理",
		"cust_notified_at": "2026-10-09 09:00:00",
		"cust_contact":     "客户质检 王工",
		"cust_channel":     "电话",
	}
	for _, skip := range []string{"authorized_by", "cust_notified_at", "cust_contact", "cust_channel"} {
		body := map[string]string{"conclusion": "CONCESSION"}
		for k, v := range full {
			if k == skip {
				continue
			}
			body[k] = v
		}
		raw, _ := json.Marshal(body)
		resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", inspID), ck, string(raw))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("★ 让步缺 %s 应 400，实际 %d body=%v", skip, resp.StatusCode, out)
		}
	}
	// 全部留空 ⇒ 同样拒绝
	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", inspID), ck,
		`{"conclusion":"CONCESSION"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 让步四字段全缺应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	if got := m5DBField(t, e.st, inspID, "conclusion"); got != "" {
		t.Fatalf("被拒后 conclusion 应仍为空，实际 %q", got)
	}
}

// TC-M5-09 正常：让步四字段 + 双签 ⇒ 成功；两签列均非空（对外披露载体属批 8，此处只验数据层）。
func TestTC_M5_09_HTTP_ConcessionFullAndTwoSigns(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)

	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", inspID), ck, `{
		"conclusion": "CONCESSION",
		"authorized_by": "质量经理",
		"cust_notified_at": "2026-10-09 09:00:00",
		"cust_contact": "客户质检 王工",
		"cust_channel": "书面"
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("让步结论应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// 质检方签署（qc 自己）
	resp, out = e.do("POST", fmt.Sprintf("/api/insp/%d/concession/qc-sign", inspID), ck, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("质检签署应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	// 使用部门签署（production 角色 —— 两个入口两个权限点）
	prodCK := e.login(m5ProdOpenID)
	resp, out = e.do("POST", fmt.Sprintf("/api/insp/%d/concession/dept-sign", inspID), prodCK, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("使用部门签署应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	if got := m5DBField(t, e.st, inspID, "qc_signed_by"); got == "" {
		t.Fatal("qc_signed_by 应非空")
	}
	if got := m5DBField(t, e.st, inspID, "dept_signed_by"); got == "" {
		t.Fatal("dept_signed_by 应非空")
	}
	// 重复签署 ⇒ 拒绝（单向一次）
	resp, out = e.do("POST", fmt.Sprintf("/api/insp/%d/concession/qc-sign", inspID), ck, `{}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("已签再签应 409，实际 %d body=%v", resp.StatusCode, out)
	}
}

// TC-M5-10 ★★ 异常：紧急放行发起人自批 ⇒ 拒绝（两条都要证）+ A20 两笔审计不同人。
func TestTC_M5_10_HTTP_UrgentReleaseInitApproveSplit(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)

	// ---- 路径 ①：权限层 ----
	truckA, bags := m5Truck(t, e, ck, cust, mat)
	_ = bags
	resp, out := e.do("POST", "/api/insp/urgent-release/init", ck,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"料急，先行放行"}`, truckA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("紧急放行发起应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	// 未发起先审批（另一对象）⇒ 409
	truckB, _ := m5Truck(t, e, ck, cust, mat)
	resp, out = e.do("POST", "/api/insp/urgent-release/approve", ck,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d}`, truckB))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★ 未发起先审批应 409，实际 %d body=%v", resp.StatusCode, out)
	}
	// qc 走审批入口 ⇒ 403（qc 对 approve 为 NONE，权限层拦在 handler 之前）
	resp, out = e.do("POST", "/api/insp/urgent-release/approve", ck,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d}`, truckA))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★ qc 走审批入口应 403（权限层），实际 %d body=%v", resp.StatusCode, out)
	}
	// management（APPROVE）审批他人发起的 ⇒ 200
	mgmtCK := e.login(m5MgmtOpenID)
	resp, out = e.do("POST", "/api/insp/urgent-release/approve", mgmtCK,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"同意"}`, truckA))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management 审批应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// ---- 路径 ②：服务层同人校验（构造「有 approve 权限者恰为发起人」）----
	// 给 management 临时加 INIT（其种子为 NONE），让「发起人」与「审批人」同为 management。
	if _, err := e.st.DB().ExecContext(context.Background(), `
UPDATE s_role_permission SET level = 'INIT'
 WHERE role_code = 'management' AND point_code = 'insp.urgent.release.init'`); err != nil {
		t.Fatalf("变异 management INIT 失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := e.st.DB().ExecContext(context.Background(), `
UPDATE s_role_permission SET level = 'NONE'
 WHERE role_code = 'management' AND point_code = 'insp.urgent.release.init'`); err != nil {
			t.Errorf("还原 management INIT 失败: %v", err)
		}
	})
	resp, out = e.do("POST", "/api/insp/urgent-release/init", mgmtCK,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d,"reason":"管理发起"}`, truckB))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management 发起应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, out = e.do("POST", "/api/insp/urgent-release/approve", mgmtCK,
		fmt.Sprintf(`{"entity":"b_truck_lot","entity_id":%d}`, truckB))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("★★ 发起人自批应 403（服务层同人校验），实际 %d body=%v", resp.StatusCode, out)
	}

	// ---- A20：两条审计各一笔、actor_open_id 不同 ----
	auditRows, err := e.st.FindAuditByEntity(context.Background(), "b_truck_lot", truckA)
	if err != nil {
		t.Fatalf("查紧急放行审计失败: %v", err)
	}
	var initBy, approveBy string
	for _, r := range auditRows {
		switch r.Action {
		case "urgent_release_init":
			initBy = r.ActorOpenID
		case "urgent_release_approve":
			approveBy = r.ActorOpenID
		}
	}
	if initBy == "" || approveBy == "" {
		t.Fatalf("★ 紧急放行须 init/approve 各一笔审计，实际 init=%q approve=%q", initBy, approveBy)
	}
	if initBy == approveBy {
		t.Fatalf("★ 两笔审计 actor_open_id 必须不同，实际都是 %q", initBy)
	}
	if initBy != m5HTTPOpenID || approveBy != m5MgmtOpenID {
		t.Fatalf("审计当事人错乱：init=%q approve=%q", initBy, approveBy)
	}
}

// TC-M5-11 ★ 正常：复检 ⇒ 新增检测单（is_recheck=1 + recheck_of），**原单结果逐字不变**。
func TestTC_M5_11_HTTP_RecheckKeepsOriginal(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	item1 := m5ItemID(t, e.st, "m5ti01")
	item2 := m5ItemID(t, e.st, "m5ti02")

	body, _ := json.Marshal(map[string]interface{}{"item_ids": []int64{item1, item2}})
	if resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("加项应 200，实际 %d %v", resp.StatusCode, out)
	}
	if resp, out := e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":42}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("录入应 200，实际 %d %v", resp.StatusCode, out)
	}
	before := m5InspResults(t, e.st, inspID)
	if before == "" {
		t.Fatal("前置失败：原单无结果行")
	}

	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/recheck", inspID), ck, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("复检应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	row, _ := out["row"].(map[string]interface{})
	newID := numOf(row["id"])
	if newID == inspID {
		t.Fatal("复检必须新开单")
	}
	if row["is_recheck"] != true {
		t.Fatalf("新单 is_recheck 应 true，实际 %v", row["is_recheck"])
	}
	if numOf(row["recheck_of"]) != inspID {
		t.Fatalf("新单 recheck_of 应指回原单 %d，实际 %v", inspID, row["recheck_of"])
	}
	// ★ 原单未被作废
	if got := m5DBField(t, e.st, inspID, "id"); got == "" {
		t.Fatal("原单应仍存在")
	}
	// ★ 原单结果逐字不变
	after := m5InspResults(t, e.st, inspID)
	if after != before {
		t.Fatalf("★★ 复检后原单结果必须逐字不变：\nbefore=\n%s\nafter=\n%s", before, after)
	}
	// 新单清单为空（同新单流程，不复制结果）
	if _, out := e.do("GET", fmt.Sprintf("/api/insp/%d/items", newID), ck, ""); numOf(out["count"]) != 0 {
		t.Fatalf("新单清单应为空，实际 %v", out["count"])
	}
}

// TC-M5-12 ★ 变异防护：结果修正**不得原地 UPDATE** ——
//
//	① 已测行二次录入 ⇒ 拒绝且原值不变；② 修正 ⇒ 原单被作废 + 新单 recheck_of 指回；
//	③ 原单 result 行逐字不变。
func TestTC_M5_12_HTTP_CorrectIsVoidAndNewNotInPlace(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, bags := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truckID, fmt.Sprint(bags[0]["code"]))
	inspID := m5CreateInsp(t, e, ck, "车次", truckID)
	item1 := m5ItemID(t, e.st, "m5ti01")

	body, _ := json.Marshal(map[string]interface{}{"item_ids": []int64{item1}})
	if resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/items", inspID), ck, string(body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("加项应 200，实际 %d %v", resp.StatusCode, out)
	}
	if resp, out := e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":42}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("录入应 200，实际 %d %v", resp.StatusCode, out)
	}
	before := m5InspResults(t, e.st, inspID)

	// ① 二次录入 ⇒ 409，原值不变
	resp, out := e.do("PATCH", fmt.Sprintf("/api/insp/%d/items/%d", inspID, item1), ck,
		`{"state":"已测","value_num":99}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("★ 已测行二次录入应 409，实际 %d body=%v", resp.StatusCode, out)
	}
	if after := m5InspResults(t, e.st, inspID); after != before {
		t.Fatalf("★ 被拒后原值必须不变：\nbefore=\n%s\nafter=\n%s", before, after)
	}

	// ② 修正（不填原因 ⇒ 400）
	resp, out = e.do("POST", fmt.Sprintf("/api/insp/%d/correct", inspID), ck, `{"reason":""}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("修正缺原因应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, out = e.do("POST", fmt.Sprintf("/api/insp/%d/correct", inspID), ck,
		`{"reason":"录错值，重录"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("修正应 200，实际 %d body=%v", resp.StatusCode, out)
	}
	row, _ := out["row"].(map[string]interface{})
	newID := numOf(row["id"])
	if newID == inspID || row["is_recheck"] != false || numOf(row["recheck_of"]) != inspID {
		t.Fatalf("修正新单应 is_recheck=false + recheck_of=原单，实际 %v", row)
	}

	// ★ 原单被作废（b_obj_void 有行、approved_by 留空 —— 更正类不审批）
	var voidCount int
	var approved sql.NullString
	var reason string
	if err := e.st.DB().QueryRowContext(context.Background(), `
SELECT COUNT(*), MAX(approved_by), MAX(reason) FROM b_obj_void
 WHERE entity = 'b_inspection' AND entity_id = ?`, inspID).
		Scan(&voidCount, &approved, &reason); err != nil {
		t.Fatalf("查作废行失败: %v", err)
	}
	if voidCount != 1 {
		t.Fatalf("★ 修正必须作废原单（b_obj_void 恰 1 行），实际 %d", voidCount)
	}
	if approved.Valid && approved.String != "" {
		t.Fatalf("更正类不审批，approved_by 应留空，实际 %q", approved.String)
	}
	if !strings.Contains(reason, "录错值") {
		t.Fatalf("作废原因应保留，实际 %q", reason)
	}

	// ③ 原单 result 行逐字不变
	if after := m5InspResults(t, e.st, inspID); after != before {
		t.Fatalf("★★ 修正不得原地 UPDATE，原单结果必须逐字不变：\nbefore=\n%s\nafter=\n%s", before, after)
	}

	// 现行单 = 新单（列表口径）
	resp, out = e.do("GET", "/api/insp/tasks?state=待检", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("任务列表应 200，实际 %d", resp.StatusCode)
	}
	found := false
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truckID {
			if numOf(m["inspection_id"]) != newID {
				t.Fatalf("★ 列表应只认现行单 #%d，实际 %v", newID, m["inspection_id"])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("任务列表应含该车次，实际 %v", out)
	}
}

// TC-M5-13 正常：任务列表三来源并集 + 可按状态过滤 + 已作废对象不列出。
func TestTC_M5_13_HTTP_TaskListUnionAndFilter(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)

	// 车次1：有大样 + 有单 + 有结论 ⇒ 已检
	truck1, bags1 := m5Truck(t, e, ck, cust, mat)
	m5Group(t, e, ck, truck1, fmt.Sprint(bags1[0]["code"]))
	insp1 := m5CreateInsp(t, e, ck, "车次", truck1)
	resp, out := e.do("POST", fmt.Sprintf("/api/insp/%d/conclusion", insp1), ck,
		`{"conclusion":"合格"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("出合格结论应 200，实际 %d body=%v", resp.StatusCode, out)
	}

	// 车次2：无大样 ⇒ 未取样
	truck2, _ := m5Truck(t, e, ck, cust, mat)
	// 生产批 / 成品批（§5 夹具）⇒ 未取样
	batchID := m5BatchFixture(t, e.st, cust, mat)
	fgID := m5FgFixture(t, e.st, cust, mat, batchID)
	// 已作废对象不列出
	truck3, _ := m5Truck(t, e, ck, cust, mat)
	if _, err := e.st.DB().ExecContext(context.Background(),
		`UPDATE b_truck_lot SET status = '已作废' WHERE id = ?`, truck3); err != nil {
		t.Fatalf("作废车次失败: %v", err)
	}

	// 全量：三来源并集
	resp, out = e.do("GET", "/api/insp/tasks", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("任务列表应 200，实际 %d", resp.StatusCode)
	}
	type key struct {
		t  string
		id int64
	}
	seen := map[key]map[string]interface{}{}
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		seen[key{fmt.Sprint(m["target_type"]), numOf(m["target_id"])}] = m
	}
	for _, want := range []key{{"车次", truck1}, {"车次", truck2}, {"生产批", batchID}, {"成品批", fgID}} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("★ 三来源并集缺行 %v（实际 %v）", want, seen)
		}
	}
	if _, ok := seen[key{"车次", truck3}]; ok {
		t.Fatal("★ 已作废对象不得列出")
	}
	if s := fmt.Sprint(seen[key{"车次", truck1}]["insp_state"]); s != "已检" {
		t.Fatalf("有结论 ⇒ 已检，实际 %s", s)
	}
	if s := fmt.Sprint(seen[key{"车次", truck2}]["insp_state"]); s != "未取样" {
		t.Fatalf("无大样 ⇒ 未取样，实际 %s", s)
	}

	// 过滤：待检（truck1 已检 ⇒ 不在）
	resp, out = e.do("GET", "/api/insp/tasks?state=待检", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("过滤应 200，实际 %d", resp.StatusCode)
	}
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truck1 {
			t.Fatal("★ state=待检 不得含已检对象")
		}
	}
	// 过滤：已检 ⇒ 只有 truck1（车次范围内）
	resp, out = e.do("GET", "/api/insp/tasks?state=已检", ck, "")
	found := false
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truck1 {
			found = true
		}
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truck2 {
			t.Fatal("★ state=已检 不得含未取样对象")
		}
	}
	if !found {
		t.Fatal("state=已检 应含 truck1")
	}
}

// TC-M5-14 边界：尚无大样的对象**仍列出**并提示「尚未取样」；建单被拒但对象不消失。
func TestTC_M5_14_HTTP_NotSampledStillListed(t *testing.T) {
	e, ck := m5HTTPEnv(t)
	cust, mat := m5MasterIDs(t, e.st)
	truckID, _ := m5Truck(t, e, ck, cust, mat)

	resp, out := e.do("GET", "/api/insp/tasks?state=未取样", ck, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("任务列表应 200，实际 %d", resp.StatusCode)
	}
	var row map[string]interface{}
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truckID {
			row = m
		}
	}
	if row == nil {
		t.Fatalf("★★ 未取样对象不得从列表消失（A2/§6-24），实际 %v", out)
	}
	if s := fmt.Sprint(row["insp_state"]); s != "未取样" {
		t.Fatalf("insp_state 应「未取样」，实际 %s", s)
	}
	if !strings.Contains(fmt.Sprint(row["hint"]), "尚未取样") {
		t.Fatalf("应给可读提示「尚未取样」，实际 %v", row["hint"])
	}

	// 建单 ⇒ 400（无大样拒绝），但列表仍在
	resp, out = e.do("POST", "/api/insp", ck,
		fmt.Sprintf(`{"target_type":"车次","target_id":%d}`, truckID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("★ 无大样建单应 400，实际 %d body=%v", resp.StatusCode, out)
	}
	resp, out = e.do("GET", "/api/insp/tasks?state=未取样", ck, "")
	stillThere := false
	for _, it := range out["rows"].([]interface{}) {
		m, _ := it.(map[string]interface{})
		if m["target_type"] == "车次" && numOf(m["target_id"]) == truckID {
			stillThere = true
		}
	}
	if !stillThere {
		t.Fatal("★ 建单失败后对象不得被藏起来")
	}
}
