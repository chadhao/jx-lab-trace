package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// ===== 主数据层测试 =====
//
// ★ 分两档：
//	① 纯单测（不连库）—— 实体定义与 spec/schema.sql 逐列对齐；
//	② DB 集成测试 —— 需 JX_TEST_DB=1（按 docs/05：本机只跑纯单测）。

// ---- ① 实体定义 ⇄ spec/schema.sql ----

// specTableColumns 解析 spec/schema.sql 中某张表的列名集合（跳过 PRIMARY/UNIQUE/KEY 等约束行）。
func specTableColumns(t *testing.T, table string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("../../spec/schema.sql")
	if err != nil {
		t.Fatalf("读取 spec/schema.sql 失败: %v", err)
	}
	text := string(raw)
	marker := "CREATE TABLE " + table + " ("
	idx := strings.Index(text, marker)
	if idx < 0 {
		t.Fatalf("spec/schema.sql 里找不到建表语句 %s", table)
	}
	rest := text[idx+len(marker):]
	end := strings.Index(rest, "ENGINE=InnoDB")
	if end < 0 {
		t.Fatalf("表 %s 的 DDL 没有 ENGINE=InnoDB 收尾", table)
	}
	cols := map[string]bool{}
	for _, line := range strings.Split(rest[:end], "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "--") || strings.HasPrefix(s, ")") {
			continue
		}
		if strings.HasPrefix(s, "PRIMARY ") || strings.HasPrefix(s, "UNIQUE ") ||
			strings.HasPrefix(s, "KEY ") || strings.HasPrefix(s, "CONSTRAINT ") ||
			strings.HasPrefix(s, "FOREIGN ") {
			continue
		}
		name := strings.Fields(s)[0]
		name = strings.Trim(name, "`")
		cols[name] = true
	}
	return cols
}

// TestMD_EntityDefinitionsMatchSpec：实体的表名 / 可写列 / 必填列 / 业务键
// 必须**逐列**存在于 spec/schema.sql；版本链开关必须与该表实际列一致。
func TestMD_EntityDefinitionsMatchSpec(t *testing.T) {
	ents := MDEntities()
	if len(ents) != 8 {
		t.Fatalf("主数据应为 8 类，实际 %d", len(ents))
	}
	for _, e := range ents {
		cols := specTableColumns(t, e.Table)
		check := func(what string, names []string) {
			for _, n := range names {
				if !cols[n] {
					t.Errorf("%s（%s）的列 %q 在 spec/schema.sql 中不存在", e.Table, what, n)
				}
			}
		}
		check("Writable", e.Writable)
		check("Required", e.Required)
		check("BizKey", e.BizKey)
		if e.NameCol != "" {
			check("NameCol", []string{e.NameCol})
		}
		for _, r := range e.Required {
			if _, ok := indexOf(e.Writable, r); !ok {
				t.Errorf("%s：必填列 %q 不在可写白名单里（必填却写不了）", e.Table, r)
			}
		}
		// 版本链开关必须与真实列一致
		if e.Versioned && !cols["version"] {
			t.Errorf("%s 标了 Versioned，但表里没有 version 列", e.Table)
		}
		if !e.Versioned && cols["version"] {
			t.Errorf("%s 有 version 列却没标 Versioned（版本链会静默失效）", e.Table)
		}
		if e.HasValid && !cols["valid_from"] {
			t.Errorf("%s 标了 HasValid，但表里没有 valid_from 列", e.Table)
		}
		if e.Versioned && !cols["supersedes_id"] {
			t.Errorf("%s 标了 Versioned，但表里没有 supersedes_id 列（前向指针是版本链的唯一线索）", e.Table)
		}
		if !e.HasValid && cols["valid_from"] && e.Versioned {
			t.Errorf("%s 有 valid_from 却没标 HasValid", e.Table)
		}
	}

	// ★ 口径 3 / 4：m_team 豁免版本链；m_test_item_limit 带版本链但没有 valid_from/valid_to
	team, _ := MDEntityByKey("teams")
	if team.Versioned || team.HasValid {
		t.Fatal("m_team 按 COLLAB N-006 口径应完全豁免版本链")
	}
	lim, _ := MDEntityByKey("test-item-limits")
	if !lim.Versioned {
		t.Fatal("m_test_item_limit 应带版本链（version / supersedes_id / is_current）")
	}
	if lim.HasValid {
		t.Fatal("m_test_item_limit 按口径不得有 valid_from / valid_to")
	}
	// ★ 口径 4：判定限唯一键必须含 version（这正是 6 张表缺的东西，见 N-009）
	if !strings.Contains(string(mustReadSchema(t)), "uk_limit_scope (test_item_id, customer_id, material_id, version)") {
		t.Fatal("spec 的 uk_limit_scope 应包含 version（判定限的唯一键口径）")
	}
}

func mustReadSchema(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../spec/schema.sql")
	if err != nil {
		t.Fatalf("读取 spec/schema.sql 失败: %v", err)
	}
	return b
}

// ---- ② DB 集成 ----

func mdTestActor() MDActor {
	return MDActor{OpenID: "ou_test_m1_qc", Name: "测试质检员", Role: "qc", IP: "127.0.0.1"}
}

// wipeMD 清掉测试写入的主数据（跑前清一次，保证可重复执行；跑后由各测试的 t.Cleanup 收尾）。
func wipeMD(t *testing.T, st *Store, tables ...string) {
	t.Helper()
	for _, tb := range tables {
		if _, err := st.db.Exec("DELETE FROM `" + tb + "` WHERE created_by LIKE 'ou\\_test\\_m1%'"); err != nil {
			t.Fatalf("清理 %s 失败: %v", tb, err)
		}
	}
}

// TC-M1-01 正常：新增 4 位数字编号客户 ⇒ 成功且可被检索。
func TestTC_M1_01_CreateCustomer(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_customer")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_customer WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	row, err := st.MDCreate(ctx, "customers", map[string]interface{}{
		"code": "9001", "name": "测试客户甲",
	}, mdTestActor())
	if err != nil {
		t.Fatalf("新增客户失败: %v", err)
	}
	if row["code"] != "9001" {
		t.Fatalf("code 应为 9001，实际 %v", row["code"])
	}
	if toInt64(row["version"]) != 1 || !mdIsCurrent(row) {
		t.Fatalf("新建行应是 v1 且 is_current=1，实际 version=%v is_current=%v",
			row["version"], row["is_current"])
	}

	list, err := st.MDList(ctx, "customers", MDOpts{Search: "9001"})
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	found := false
	for _, r := range list {
		if r["code"] == "9001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("新增的客户应可被检索到，实际 %d 行", len(list))
	}
}

// TC-M1-02 异常：再用同一 code 新增 ⇒ 唯一约束拒绝。
func TestTC_M1_02_DuplicateCustomerRejected(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_customer")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_customer WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	if _, err := st.MDCreate(ctx, "customers", map[string]interface{}{
		"code": "9002", "name": "测试客户乙",
	}, mdTestActor()); err != nil {
		t.Fatalf("第一次新增应成功: %v", err)
	}
	_, err := st.MDCreate(ctx, "customers", map[string]interface{}{
		"code": "9002", "name": "重号客户",
	}, mdTestActor())
	if err == nil {
		t.Fatal("同 code 再次新增必须被唯一约束拒绝")
	}
	if !isErr(err, ErrMDDuplicate) {
		t.Fatalf("应返回 ErrMDDuplicate，实际: %v", err)
	}
}

// TC-M1-03 正常：改客户名 ⇒ 产生新版本；旧版本业务字段原样保留；is_current 唯一切换。
//
// ★★ 该用例当前会**失败**，根因是 COLLAB N-009：spec 的 uk_customer_code(code)
//
//	未含 version ⇒ 版本链第二行 INSERT 必然 1062。测试按 docs/04 原样写、**不改期望**。
func TestTC_M1_03_RenameCreatesNewVersion(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_customer")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_customer WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	old, err := st.MDCreate(ctx, "customers", map[string]interface{}{
		"code": "9003", "name": "改名前",
	}, mdTestActor())
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	oldID := toInt64(old["id"])

	fresh, err := st.MDUpdate(ctx, "customers", oldID,
		map[string]interface{}{"name": "改名后"}, "TC-M1-03 改客户名", mdTestActor())
	if err != nil {
		t.Fatalf("改客户名应产生新版本（失败根因见 COLLAB N-009）: %v", err)
	}
	newID := toInt64(fresh["id"])
	if newID == oldID {
		t.Fatal("新版本必须是新行（id 不同）")
	}
	if toInt64(fresh["version"]) != 2 {
		t.Fatalf("新版本 version 应为 2，实际 %v", fresh["version"])
	}
	if fresh["name"] != "改名后" {
		t.Fatalf("新版本应为改名后的值，实际 %v", fresh["name"])
	}
	if toInt64(fresh["supersedes_id"]) != oldID {
		t.Fatalf("新版本 supersedes_id 应指向原行 %d，实际 %v", oldID, fresh["supersedes_id"])
	}

	// 直连库断言：原行业务字段一字未改、盖了失效戳
	var name string
	var isCur int
	var validTo interface{}
	if err := st.db.QueryRowContext(ctx,
		`SELECT name, is_current, valid_to FROM m_customer WHERE id = ?`, oldID).
		Scan(&name, &isCur, &validTo); err != nil {
		t.Fatalf("读原行失败: %v", err)
	}
	if name != "改名前" {
		t.Fatalf("原行业务字段不得被 UPDATE，实际 name=%q", name)
	}
	if isCur != 0 {
		t.Fatalf("原行 is_current 应为 0，实际 %d", isCur)
	}
	if validTo == nil {
		t.Fatal("原行应盖 valid_to 失效戳")
	}

	// 同 code 下 is_current=1 必须唯一
	var curCount int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM m_customer WHERE code = '9003' AND is_current = 1`).
		Scan(&curCount); err != nil {
		t.Fatalf("统计当前版本失败: %v", err)
	}
	if curCount != 1 {
		t.Fatalf("同 code 下 is_current=1 应恰好 1 行，实际 %d", curCount)
	}
}

// TC-M1-04 边界：查该客户历史版本 ⇒ 能查到改名前的值。
func TestTC_M1_04_HistoryKeepsOldValue(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_customer")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_customer WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	v1, err := st.MDCreate(ctx, "customers", map[string]interface{}{
		"code": "9004", "name": "历史版本甲",
	}, mdTestActor())
	if err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	v1ID := toInt64(v1["id"])
	if _, err := st.MDUpdate(ctx, "customers", v1ID,
		map[string]interface{}{"name": "历史版本乙"}, "TC-M1-04 查历史", mdTestActor()); err != nil {
		t.Fatalf("改名失败（根因见 COLLAB N-009）: %v", err)
	}

	cur, err := st.MDList(ctx, "customers", MDOpts{Search: "9004"})
	if err != nil || len(cur) == 0 {
		t.Fatalf("查当前版本失败: %v / %d 行", err, len(cur))
	}
	chain, err := st.MDHistory(ctx, "customers", toInt64(cur[0]["id"]))
	if err != nil {
		t.Fatalf("查版本链失败: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("版本链应有 2 版，实际 %d", len(chain))
	}
	if chain[0]["name"] != "历史版本甲" {
		t.Fatalf("链上第一版应保留改名前的值，实际 %v", chain[0]["name"])
	}
	if chain[len(chain)-1]["name"] != "历史版本乙" {
		t.Fatalf("链上最后一版应是改名后的值，实际 %v", chain[len(chain)-1]["name"])
	}
}

// TC-M1-07 正常：车牌重复 ⇒ 第二次被拒。
func TestTC_M1_07_DuplicatePlateRejected(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_vehicle")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_vehicle WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	if _, err := st.MDCreate(ctx, "vehicles", map[string]interface{}{
		"plate_no": "湘F12345",
	}, mdTestActor()); err != nil {
		t.Fatalf("第一次新增车牌应成功: %v", err)
	}
	_, err := st.MDCreate(ctx, "vehicles", map[string]interface{}{
		"plate_no": "湘F12345",
	}, mdTestActor())
	if err == nil {
		t.Fatal("重复车牌必须被拒")
	}
	if !isErr(err, ErrMDDuplicate) {
		t.Fatalf("应返回 ErrMDDuplicate，实际: %v", err)
	}
}

// TC-M1-08 异常：停用物料后，默认（启用）列表不再出现，但记录本身仍可查。
func TestTC_M1_08_DisabledMaterialHiddenFromActiveList(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_material")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_material WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	row, err := st.MDCreate(ctx, "materials", map[string]interface{}{
		"code": "9008", "name": "测试物料", "kind": "原料",
	}, mdTestActor())
	if err != nil {
		t.Fatalf("新增物料失败: %v", err)
	}
	id := toInt64(row["id"])

	if _, err := st.MDSetStatus(ctx, "materials", id, "停用", "TC-M1-08 停用后不应出现在下拉",
		mdTestActor()); err != nil {
		t.Fatalf("停用失败: %v", err)
	}

	// ① 只看启用 ⇒ 不再出现
	active, err := st.MDList(ctx, "materials", MDOpts{Status: "启用"})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	for _, r := range active {
		if toInt64(r["id"]) == id {
			t.Fatal("已停用物料不得出现在「启用」下拉里")
		}
	}
	// ② 记录仍在（历史单据仍能显示它）
	got, err := st.MDGet(ctx, "materials", id)
	if err != nil {
		t.Fatalf("停用后记录必须仍可查: %v", err)
	}
	if got["status"] != "停用" {
		t.Fatalf("status 应为 停用，实际 %v", got["status"])
	}
	if toInt64(got["version"]) != 1 {
		t.Fatalf("停用是状态流转，不应产生新版本，实际 version=%v", got["version"])
	}
}

// 判定限：写入 + 按客户×物料解析（覆盖 SQL 侧；优先级纯单测见 limits_test.go）。
func TestLimit_CRUDAndLookup_SQL(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	wipeMD(t, st, "m_test_item_limit", "m_test_item")
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_test_item_limit WHERE created_by LIKE 'ou\_test\_m1%'`)
		_, _ = st.db.ExecContext(ctx, `DELETE FROM m_test_item WHERE created_by LIKE 'ou\_test\_m1%'`)
	})

	ti, err := st.MDCreate(ctx, "test-items", map[string]interface{}{
		"code": "T9001", "name": "水分", "value_type": "数值", "unit": "%",
	}, mdTestActor())
	if err != nil {
		t.Fatalf("新增检测项失败: %v", err)
	}
	tiID := toInt64(ti["id"])

	// 通用默认 + A 客户专属
	if _, err := st.MDCreate(ctx, "test-item-limits", map[string]interface{}{
		"test_item_id": tiID, "customer_id": 0, "material_id": 0, "upper_limit": 0.5,
	}, mdTestActor()); err != nil {
		t.Fatalf("新增通用判定限失败: %v", err)
	}
	if _, err := st.MDCreate(ctx, "test-item-limits", map[string]interface{}{
		"test_item_id": tiID, "customer_id": 7001, "material_id": 0, "upper_limit": 0.3,
	}, mdTestActor()); err != nil {
		t.Fatalf("新增客户专属判定限失败: %v", err)
	}

	// A 客户 ⇒ 专属（0.3）；B 客户 ⇒ 通用（0.5）
	hit, ok, err := st.LookupLimit(ctx, tiID, 7001, 9001)
	if err != nil || !ok {
		t.Fatalf("A 客户应命中专属限值: ok=%v err=%v", ok, err)
	}
	if numStr(hit.Upper) != "0.3" {
		t.Fatalf("A 客户应取到 0.3，实际 %v", hit.Upper)
	}
	hit, ok, err = st.LookupLimit(ctx, tiID, 7002, 9001)
	if err != nil || !ok {
		t.Fatalf("B 客户应命中通用限值: ok=%v err=%v", ok, err)
	}
	if numStr(hit.Upper) != "0.5" {
		t.Fatalf("B 客户应取到 0.5，实际 %v", hit.Upper)
	}

	// 判定限的版本链（唯一键含 version ⇒ 可用）
	// ★ 只取**本用例自己那个检测项**下的行 —— 第一版按「全表第一个通用行」找，
	//	结果改到了冒烟脚本留下的别的检测项的行（跨用例污染），已修。
	var zeroScope int64
	if err := st.db.QueryRowContext(ctx,
		`SELECT id FROM m_test_item_limit
		  WHERE test_item_id = ? AND customer_id = 0 AND material_id = 0 AND is_current = 1
		  ORDER BY id DESC LIMIT 1`, tiID).Scan(&zeroScope); err != nil {
		t.Fatalf("定位本用例的通用判定限失败: %v", err)
	}
	fresh, err := st.MDUpdate(ctx, "test-item-limits", zeroScope,
		map[string]interface{}{"upper_limit": 0.6}, "调整通用上限", mdTestActor())
	if err != nil {
		t.Fatalf("判定限版本链应可用（其唯一键含 version）: %v", err)
	}
	if toInt64(fresh["version"]) != 2 {
		t.Fatalf("判定限改后应为 v2，实际 %v", fresh["version"])
	}
}

// isErr 判断错误链里是否含某个哨兵错误。
func isErr(err, target error) bool { return errors.Is(err, target) }
