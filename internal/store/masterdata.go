package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/permission"
)

// ===== 主数据通用层（M1 / D1）=====
//
// ★ 版本链（任务包 D1，docs/01 §2 P1）：改 = 作废 + 新增，**绝不 UPDATE 原行业务字段**：
//	① 原行 is_current=0、valid_to=now；
//	② INSERT 新行 version=原+1、is_current=1、supersedes_id=原行 id、valid_from=now；
//	③ 同一业务键（code / plate_no / 判定限 scope）下**有且仅有一行 is_current=1**。
// ★★ 只存**前向指针 supersedes_id**，不存反向列（COLLAB N-006 定案）；
//	反向关系由 `WHERE supersedes_id = ?` 反查得出。
// ★ **只存业务字段的版本**：status 属「当前处于哪一步」，按 P1 允许原地流转，
//	故「停用 / 启用」走 UPDATE status，不产生新版本（TC-M1-08）。
// ★ 豁免：m_team / m_user 不带版本链（schema 注释：人员与班组的来去不是版本语义）。
//
// ★★ 边界（如实说）：
//	· 能保证：同事务内「旧版本失效 + 新版本插入 + 审计」三者同生共死；
//	  并发下用 SELECT ... FOR UPDATE 串行化同一行的改写；业务键重复一律 409。
//	· 不能保证：绕过本层直连库的写入；也**不能**替 spec 修掉
//	  `UNIQUE(code)` 与版本链的冲突 —— 见 COLLAB N-009（6 张表的唯一键漏了 version）。

// MDActor 是一次写操作的执行者（审计用）。
type MDActor struct {
	OpenID string
	Name   string
	Role   string
	IP     string
}

// 主数据层的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrMDEntityNotFound = errors.New("主数据实体不存在")
	ErrMDRowNotFound    = errors.New("主数据记录不存在")
	ErrMDDuplicate      = errors.New("业务键已存在")
	ErrMDStale          = errors.New("该记录已被新版本取代，请刷新后重试")
	ErrMDBadInput       = errors.New("输入不合法")
	ErrMDNeedReason     = errors.New("更正业务字段必须填写原因")
	ErrMDNoChange       = errors.New("内容未变化")
)

// MDEntity 描述一张主数据表的可写面与语义。
type MDEntity struct {
	Key       string          // URL 段（复数形式）
	Table     string          // 表名
	Label     string          // 中文名（错误与审计可读）
	Perm      permission.Code // 保护该实体的权限点
	Versioned bool            // 是否带版本链
	HasValid  bool            // 是否有 valid_from / valid_to（m_test_item_limit 没有）
	BizKey    []string        // 业务唯一键列（版本链下仅在 is_current=1 的行间唯一）
	Writable  []string        // 可写业务列白名单（其余列一律拒绝写入）
	Required  []string        // 必填列
	NameCol   string          // 可模糊检索的名称列（空 = 该表无名称列）
	Validate  func(values map[string]interface{}) error
}

var digits4 = regexp.MustCompile(`^[0-9]{4}$`)

func strVal(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

func requireStr(values map[string]interface{}, keys ...string) error {
	for _, k := range keys {
		if strings.TrimSpace(strVal(values[k])) == "" {
			return fmt.Errorf("%w：字段 %s 必填", ErrMDBadInput, k)
		}
	}
	return nil
}

func oneOf(values map[string]interface{}, key string, allowed ...string) error {
	got := strings.TrimSpace(strVal(values[key]))
	if got == "" {
		return nil
	}
	for _, a := range allowed {
		if got == a {
			return nil
		}
	}
	return fmt.Errorf("%w：字段 %s 只允许 %s（收到 %q）", ErrMDBadInput, key, strings.Join(allowed, " / "), got)
}

// mdEntities 是 8 张主数据表的注册表（键 = URL 段）。
// ★ 列名与 spec/schema.sql 严格对齐，由 TestMDEntitiesMatchSpec 逐列机检。
var mdEntities = []MDEntity{
	{
		Key: "customers", Table: "m_customer", Label: "客户",
		Perm: permission.MdCustomer, Versioned: true, HasValid: true,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name", "short_name", "contact", "phone", "is_internal", "external_id"},
		Required: []string{"code", "name"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			if err := requireStr(values, "code", "name"); err != nil {
				return err
			}
			if !digits4.MatchString(strings.TrimSpace(strVal(values["code"]))) {
				return fmt.Errorf("%w：客户编号必须是 4 位数字", ErrMDBadInput)
			}
			return nil
		},
	},
	{
		Key: "compositions", Table: "m_composition", Label: "原料组成",
		Perm: permission.MdMaterial, Versioned: true, HasValid: true,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name", "external_id"},
		Required: []string{"code", "name"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			return requireStr(values, "code", "name")
		},
	},
	{
		Key: "material-types", Table: "m_material_type", Label: "原料类型",
		Perm: permission.MdMaterial, Versioned: true, HasValid: true,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name", "external_id"},
		Required: []string{"code", "name"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			return requireStr(values, "code", "name")
		},
	},
	{
		Key: "materials", Table: "m_material", Label: "物料",
		Perm: permission.MdMaterial, Versioned: true, HasValid: true,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name", "composition_id", "material_type_id", "kind", "spec", "unit", "external_id"},
		Required: []string{"code", "name", "kind"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			if err := requireStr(values, "code", "name", "kind"); err != nil {
				return err
			}
			if !digits4.MatchString(strings.TrimSpace(strVal(values["code"]))) {
				return fmt.Errorf("%w：物料编号必须是 4 位数字", ErrMDBadInput)
			}
			return oneOf(values, "kind", "原料", "成品")
		},
	},
	{
		Key: "test-items", Table: "m_test_item", Label: "检测项目",
		Perm: permission.MdTestItem, Versioned: true, HasValid: true,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name", "unit", "method", "value_type", "decimals", "enum_values", "sort", "external_id"},
		Required: []string{"code", "name", "value_type"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			if err := requireStr(values, "code", "name", "value_type"); err != nil {
				return err
			}
			if err := oneOf(values, "value_type", "数值", "文本", "枚举"); err != nil {
				return err
			}
			if strVal(values["value_type"]) == "枚举" && strings.TrimSpace(strVal(values["enum_values"])) == "" {
				return fmt.Errorf("%w：value_type=枚举 时 enum_values 必填", ErrMDBadInput)
			}
			return nil
		},
	},
	{
		// ★ 判定限独立成表（不得内联在 m_test_item 上）；customer_id/material_id = 0 ⇒ 通用默认。
		// ★ 只带 version / supersedes_id / is_current，**无** valid_from / valid_to（口径 4）。
		Key: "test-item-limits", Table: "m_test_item_limit", Label: "判定限",
		Perm: permission.MdTestItem, Versioned: true, HasValid: false,
		BizKey:   []string{"test_item_id", "customer_id", "material_id"},
		Writable: []string{"test_item_id", "customer_id", "material_id", "lower_limit", "upper_limit", "is_required", "note"},
		Required: []string{"test_item_id"},
		Validate: func(values map[string]interface{}) error {
			if err := requireStr(values, "test_item_id"); err != nil {
				return err
			}
			for _, k := range []string{"test_item_id", "customer_id", "material_id"} {
				if v, ok := values[k]; ok && v != nil {
					if f, ok2 := toFloat(v); !ok2 || f < 0 || f != math.Trunc(f) {
						return fmt.Errorf("%w：字段 %s 必须是非负整数", ErrMDBadInput, k)
					}
				}
			}
			return nil
		},
	},
	{
		Key: "vehicles", Table: "m_vehicle", Label: "车辆",
		Perm: permission.MdVehicle, Versioned: true, HasValid: true,
		BizKey:   []string{"plate_no"},
		Writable: []string{"plate_no", "default_driver", "default_phone", "carrier", "note", "external_id"},
		Required: []string{"plate_no"},
		Validate: func(values map[string]interface{}) error {
			return requireStr(values, "plate_no")
		},
	},
	{
		// ★ 豁免版本链（schema.sql:166 注释）—— 来去用 status 表达。
		Key: "teams", Table: "m_team", Label: "班组",
		Perm: permission.MdTeam, Versioned: false, HasValid: false,
		BizKey:   []string{"code"},
		Writable: []string{"code", "name"},
		Required: []string{"code", "name"},
		NameCol:  "name",
		Validate: func(values map[string]interface{}) error {
			return requireStr(values, "code", "name")
		},
	},
}

// MDEntityByKey 按 URL 段取实体定义。
func MDEntityByKey(key string) (MDEntity, bool) {
	for _, e := range mdEntities {
		if e.Key == key {
			return e, true
		}
	}
	return MDEntity{}, false
}

// MDEntities 返回全部主数据实体定义（路由装配与自检用）。
func MDEntities() []MDEntity { return mdEntities }

// MDRow 是一行主数据（列名 → 值，值已按数据库列类型归一为 JSON 友好形式）。
type MDRow map[string]interface{}

// MDOpts 是列表查询选项。
type MDOpts struct {
	IncludeHistory bool   // true ⇒ 列出所有版本；false ⇒ 只列 is_current=1
	Status         string // 非空则按 status 过滤
	Search         string // 非空则对业务键/名称做模糊匹配
	Limit          int
}

func (o MDOpts) limit() int {
	if o.Limit <= 0 || o.Limit > 1000 {
		return 500
	}
	return o.Limit
}

// MDList 列出主数据（默认只列当前版本）。
func (s *Store) MDList(ctx context.Context, key string, opts MDOpts) ([]MDRow, error) {
	e, ok := MDEntityByKey(key)
	if !ok {
		return nil, ErrMDEntityNotFound
	}
	var conds []string
	var args []interface{}
	if !opts.IncludeHistory && e.Versioned {
		conds = append(conds, "is_current = 1")
	}
	if opts.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, opts.Status)
	}
	if opts.Search != "" && (len(e.BizKey) > 0 || e.NameCol != "") {
		like := "%" + opts.Search + "%"
		var ors []string
		for _, c := range e.BizKey {
			ors = append(ors, "CAST(`"+c+"` AS CHAR) LIKE ?")
			args = append(args, like)
		}
		if e.NameCol != "" {
			ors = append(ors, "`"+e.NameCol+"` LIKE ?")
			args = append(args, like)
		}
		conds = append(conds, "("+strings.Join(ors, " OR ")+")")
	}
	q := "SELECT * FROM `" + e.Table + "`"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, opts.limit())

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询%s失败: %w", e.Label, err)
	}
	defer rows.Close()
	out, err := scanMDRows(rows)
	if err != nil {
		return nil, fmt.Errorf("读取%s失败: %w", e.Label, err)
	}
	return out, nil
}

// MDGet 取单行（任意版本）。
func (s *Store) MDGet(ctx context.Context, key string, id int64) (MDRow, error) {
	e, err := mdEntityForWrite(key)
	if err != nil {
		return MDRow{}, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT * FROM `"+e.Table+"` WHERE id = ?", id)
	if err != nil {
		return nil, fmt.Errorf("查询%s失败: %w", e.Label, err)
	}
	defer rows.Close()
	list, err := scanMDRows(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrMDRowNotFound
	}
	return list[0], nil
}

// MDHistory 返回该记录所在**完整版本链**（版本号升序，含已失效版本）。
//
// ★ 起点可以是链上任意一行：先沿 supersedes_id 走到根（最早版本），
//
//	再由根沿 `supersedes_id = ?` **反查**走到最新版本 —— 反向关系不存列（N-006）。
func (s *Store) MDHistory(ctx context.Context, key string, id int64) ([]MDRow, error) {
	e, err := mdEntityForWrite(key)
	if err != nil {
		return nil, err
	}
	if !e.Versioned {
		row, err := s.MDGet(ctx, key, id)
		if err != nil {
			return nil, err
		}
		return []MDRow{row}, nil
	}

	type node struct {
		id  int64
		sup sql.NullInt64
		row MDRow
	}
	// 向前（supersedes_id 指向更早版本）走到根
	seen := map[int64]bool{}
	var chain []node
	cur := id
	for {
		if seen[cur] {
			return nil, fmt.Errorf("%s 版本链出现环（id=%d）", e.Label, cur)
		}
		seen[cur] = true
		rows, err := s.db.QueryContext(ctx,
			"SELECT * FROM `"+e.Table+"` WHERE id = ?", cur)
		if err != nil {
			return nil, fmt.Errorf("查询版本链失败: %w", err)
		}
		list, err := scanMDRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			if len(chain) == 0 {
				return nil, ErrMDRowNotFound
			}
			break
		}
		n := node{id: cur, row: list[0]}
		if v, ok := list[0]["supersedes_id"]; ok && v != nil {
			n.sup = sql.NullInt64{Int64: toInt64(v), Valid: true}
		}
		chain = append(chain, n)
		if !n.sup.Valid {
			break
		}
		cur = n.sup.Int64
	}
	// chain 现在是 [最新...根]；反查反向（根 → 最新）
	root := chain[len(chain)-1].id
	frontier := []int64{root}
	for len(frontier) > 0 {
		id := frontier[0]
		frontier = frontier[1:]
		rows, err := s.db.QueryContext(ctx,
			"SELECT id FROM `"+e.Table+"` WHERE supersedes_id = ?", id)
		if err != nil {
			return nil, fmt.Errorf("反查版本链失败: %w", err)
		}
		for rows.Next() {
			var nid int64
			if err := rows.Scan(&nid); err != nil {
				rows.Close()
				return nil, err
			}
			if seen[nid] {
				continue
			}
			seen[nid] = true
			frontier = append(frontier, nid)
			got, err := s.MDGet(ctx, key, nid)
			if err != nil {
				rows.Close()
				return nil, err
			}
			chain = append(chain, node{id: nid, row: got})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	// 按 version 升序（同版本按 id 升序兜底）
	for i := 0; i < len(chain); i++ {
		for j := i + 1; j < len(chain); j++ {
			vi := toInt64(chain[i].row["version"])
			vj := toInt64(chain[j].row["version"])
			if vj < vi || (vj == vi && chain[j].id < chain[i].id) {
				chain[i], chain[j] = chain[j], chain[i]
			}
		}
	}
	out := make([]MDRow, 0, len(chain))
	for _, n := range chain {
		out = append(out, n.row)
	}
	return out, nil
}

// MDCreate 新增一条主数据（version=1、is_current=1）。
func (s *Store) MDCreate(ctx context.Context, key string, values map[string]interface{}, actor MDActor) (MDRow, error) {
	e, err := mdEntityForWrite(key)
	if err != nil {
		return MDRow{}, err
	}
	fields, err := sanitizeMDValues(e, values, true)
	if err != nil {
		return MDRow{}, err
	}
	if err := e.Validate(allValues(e, fields, nil)); err != nil {
		return MDRow{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	if err := mdCheckDuplicate(ctx, tx, e, fields, 0); err != nil {
		return nil, err
	}

	cols := make([]string, 0, len(fields)+6)
	args := make([]interface{}, 0, len(fields)+6)
	for _, c := range e.Writable {
		if v, ok := fields[c]; ok {
			cols = append(cols, "`"+c+"`")
			args = append(args, v)
		}
	}
	if e.Versioned {
		cols = append(cols, "`version`", "`is_current`")
		args = append(args, 1, 1)
		if e.HasValid {
			cols = append(cols, "`valid_from`")
			args = append(args, time.Now())
		}
	}
	cols = append(cols, "`created_by`")
	args = append(args, actor.OpenID)

	q := "INSERT INTO `" + e.Table + "` (" + strings.Join(cols, ", ") + ") VALUES (" + placeholders(len(cols)) + ")"
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		if isDuplicateErr(err) {
			return nil, ErrMDDuplicate
		}
		return nil, fmt.Errorf("新增%s失败: %w", e.Label, err)
	}
	newID, _ := res.LastInsertId()

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: e.Table, EntityID: newID, Action: "create",
		// ★ field 是 VARCHAR(64)：**不塞整列清单**（test-item 的 9 列会直接 1406），
		//	完整内容放 old/new_value（TEXT）。
		NewValue:    mdSummary(fields),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.MDGet(ctx, key, newID)
}

// MDUpdate 修改业务字段：
//   - 带版本链的表 ⇒ **作废 + 新增**（原行业务字段一字不改，只盖 is_current/valid_to）；
//   - 豁免表（m_team）⇒ 原地 UPDATE。
//
// reason 必填（D1：更正须填原因并落审计；本批不做审批流）。
func (s *Store) MDUpdate(ctx context.Context, key string, id int64, values map[string]interface{}, reason string, actor MDActor) (MDRow, error) {
	e, err := mdEntityForWrite(key)
	if err != nil {
		return MDRow{}, err
	}
	if e.Versioned && strings.TrimSpace(reason) == "" {
		return nil, ErrMDNeedReason
	}
	fields, err := sanitizeMDValues(e, values, false)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	old, err := mdSelectForUpdate(ctx, tx, e, id)
	if err != nil {
		return nil, err
	}
	if e.Versioned && !mdIsCurrent(old) {
		return nil, ErrMDStale
	}

	// 计算实际差异（没变化就不造新版本）
	changed := map[string]interface{}{}
	changedOld := map[string]interface{}{}
	for c, v := range fields {
		if mdEqual(old[c], v) {
			continue
		}
		changed[c] = v
		changedOld[c] = old[c]
	}
	if len(changed) == 0 {
		return nil, ErrMDNoChange
	}
	if err := e.Validate(allValues(e, fields, old)); err != nil {
		return nil, err
	}
	if err := mdCheckDuplicateTx(ctx, tx, e, fields, changed, id); err != nil {
		return nil, err
	}

	var newID int64
	if e.Versioned {
		now := time.Now()
		set := []string{"`is_current` = 0", "`valid_to` = ?"}
		args := []interface{}{now}
		if !e.HasValid {
			// m_test_item_limit 没有 valid_from/valid_to：只切 is_current
			set = []string{"`is_current` = 0"}
			args = []interface{}{}
		}
		uq := "UPDATE `" + e.Table + "` SET " + strings.Join(set, ", ") + " WHERE id = ?"
		if _, err := tx.ExecContext(ctx, uq, append(args, id)...); err != nil {
			return nil, fmt.Errorf("作废旧版本失败: %w", err)
		}

		// 新行 = 旧行业务字段 + 差异
		cols := make([]string, 0, len(e.Writable)+8)
		vals := make([]interface{}, 0, len(e.Writable)+8)
		for _, c := range e.Writable {
			v, ok := changed[c]
			if !ok {
				v = old[c]
			}
			cols = append(cols, "`"+c+"`")
			vals = append(vals, v)
		}
		cols = append(cols, "`version`", "`supersedes_id`", "`is_current`", "`created_by`")
		vals = append(vals, toInt64(old["version"])+1, id, 1, actor.OpenID)
		if e.HasValid {
			cols = append(cols, "`valid_from`")
			vals = append(vals, now)
		}
		// 保留外部 id / 状态等非白名单列
		for _, c := range []string{"status", "external_id"} {
			if _, writable := indexOf(e.Writable, c); writable {
				continue
			}
			if v, ok := old[c]; ok {
				cols = append(cols, "`"+c+"`")
				vals = append(vals, v)
			}
		}
		q := "INSERT INTO `" + e.Table + "` (" + strings.Join(cols, ", ") + ") VALUES (" + placeholders(len(cols)) + ")"
		res, err := tx.ExecContext(ctx, q, vals...)
		if err != nil {
			if isDuplicateErr(err) {
				// ★ 根因是 spec 的 UNIQUE(code) 未含 version（COLLAB N-009）：
				//	6 张带版本链的表都插不进第二行。原样报 409，不吞错、不改期望。
				return nil, fmt.Errorf("%w：插入新版本被业务键唯一约束拒绝（%s）",
					ErrMDDuplicate, e.Label)
			}
			return nil, fmt.Errorf("插入新版本失败: %w", err)
		}
		newID, _ = res.LastInsertId()

		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: e.Table, EntityID: newID, Action: "supersede",
			Field: sortedKeys(changed), OldValue: mdSummary(changedOld), NewValue: mdSummary(changed),
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
			Reason: strings.TrimSpace(reason),
		}); err != nil {
			return nil, err
		}
	} else {
		cols := make([]string, 0, len(changed))
		vals := make([]interface{}, 0, len(changed))
		for _, c := range sortedKeysMap(changed) {
			cols = append(cols, "`"+c+"` = ?")
			vals = append(vals, changed[c])
		}
		q := "UPDATE `" + e.Table + "` SET " + strings.Join(cols, ", ") + " WHERE id = ?"
		if _, err := tx.ExecContext(ctx, q, append(vals, id)...); err != nil {
			if isDuplicateErr(err) {
				return nil, ErrMDDuplicate
			}
			return nil, fmt.Errorf("更新%s失败: %w", e.Label, err)
		}
		newID = id
		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: e.Table, EntityID: newID, Action: "update",
			Field: sortedKeys(changed), OldValue: mdSummary(changedOld), NewValue: mdSummary(changed),
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
			Reason: strings.TrimSpace(reason),
		}); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.MDGet(ctx, key, newID)
}

// MDSetStatus 停用 / 启用（status 是「当前处于哪一步」，按 P1 允许原地流转，不产生新版本）。
// ★ 停用不删除（UC-M1-01）；停用后的记录不出现在默认下拉里，但历史单据仍能显示（TC-M1-08）。
func (s *Store) MDSetStatus(ctx context.Context, key string, id int64, status, reason string, actor MDActor) (MDRow, error) {
	e, err := mdEntityForWrite(key)
	if err != nil {
		return MDRow{}, err
	}
	status = strings.TrimSpace(status)
	if status != "启用" && status != "停用" {
		return nil, fmt.Errorf("%w：status 只允许 启用 / 停用", ErrMDBadInput)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	old, err := mdSelectForUpdate(ctx, tx, e, id)
	if err != nil {
		return nil, err
	}
	cur := strVal(old["status"])
	if cur == status {
		return nil, ErrMDNoChange
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE `"+e.Table+"` SET `status` = ? WHERE id = ?", status, id); err != nil {
		return nil, fmt.Errorf("更新状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: e.Table, EntityID: id, Action: "status",
		Field: "status", OldValue: cur, NewValue: status,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: strings.TrimSpace(reason),
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.MDGet(ctx, key, id)
}

// ---- 内部工具 ----

func mdEntityForWrite(key string) (MDEntity, error) {
	e, ok := MDEntityByKey(key)
	if !ok {
		return MDEntity{}, ErrMDEntityNotFound
	}
	return e, nil
}

// sanitizeMDValues：白名单过滤 + 类型收敛。
// create=true 时把缺省的必填列留空交给 Validate 报错；update 时只处理传入的列。
func sanitizeMDValues(e MDEntity, values map[string]interface{}, create bool) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	for k, v := range values {
		if _, ok := indexOf(e.Writable, k); !ok {
			return nil, fmt.Errorf("%w：字段 %s 不可写（只允许 %s）", ErrMDBadInput, k, strings.Join(e.Writable, ", "))
		}
		cv, err := mdCoerceInput(v)
		if err != nil {
			return nil, fmt.Errorf("%w：字段 %s %v", ErrMDBadInput, k, err)
		}
		out[k] = cv
	}
	if create {
		for _, r := range e.Required {
			if _, ok := out[r]; !ok {
				// 交给 Validate 给出带字段名的错误
				out[r] = nil
			}
		}
	}
	return out, nil
}

func mdCoerceInput(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return t, nil
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return int64(t), nil
		}
		return t, nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float32:
		return float64(t), nil
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		if f, err := t.Float64(); err == nil {
			return f, nil
		}
		return nil, fmt.Errorf("不是合法数字")
	default:
		return nil, fmt.Errorf("不支持的类型 %T", v)
	}
}

// allValues：合并「本次提交的字段」与「行上既有值」，供 Validate 统一校验
// （这样未提交的必填列会拿旧行的值参与校验，而不是被当成空）。
func allValues(e MDEntity, fields map[string]interface{}, old MDRow) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range old {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	for _, c := range e.Required {
		if _, ok := out[c]; !ok {
			out[c] = nil
		}
	}
	return out
}

func mdCheckDuplicate(ctx context.Context, tx *sql.Tx, e MDEntity, fields map[string]interface{}, selfID int64) error {
	return mdCheckDuplicateTx(ctx, tx, e, fields, fields, selfID)
}

func mdCheckDuplicateTx(ctx context.Context, tx *sql.Tx, e MDEntity, all, changed map[string]interface{}, selfID int64) error {
	if len(e.BizKey) == 0 {
		return nil
	}
	var conds []string
	var args []interface{}
	for _, k := range e.BizKey {
		v, ok := all[k]
		if !ok {
			if _, cok := changed[k]; !cok {
				return nil // 没碰业务键 ⇒ 不查
			}
			v = changed[k]
		}
		if v == nil {
			return nil
		}
		conds = append(conds, "`"+k+"` = ?")
		args = append(args, v)
	}
	if e.Versioned {
		conds = append(conds, "is_current = 1")
	}
	if selfID > 0 {
		conds = append(conds, "id <> ?")
		args = append(args, selfID)
	}
	q := "SELECT id FROM `" + e.Table + "` WHERE " + strings.Join(conds, " AND ") + " LIMIT 1"
	var id int64
	err := tx.QueryRowContext(ctx, q, args...).Scan(&id)
	if err == nil {
		return ErrMDDuplicate
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return fmt.Errorf("查重失败: %w", err)
}

func mdSelectForUpdate(ctx context.Context, tx *sql.Tx, e MDEntity, id int64) (MDRow, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM `"+e.Table+"` WHERE id = ? FOR UPDATE", id)
	if err != nil {
		return nil, fmt.Errorf("查询失败: %w", err)
	}
	defer rows.Close()
	list, err := scanMDRows(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrMDRowNotFound
	}
	return list[0], nil
}

func mdIsCurrent(row MDRow) bool {
	v, ok := row["is_current"]
	if !ok {
		return true
	}
	switch t := v.(type) {
	case json.Number:
		i, _ := t.Int64()
		return i == 1
	case int64:
		return t == 1
	case float64:
		return t == 1
	default:
		return true
	}
}

func mdEqual(a, b interface{}) bool {
	return fmt.Sprint(normalizeForCompare(a)) == fmt.Sprint(normalizeForCompare(b))
}

func normalizeForCompare(v interface{}) interface{} {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case float64:
		if t == math.Trunc(t) {
			return int64(t)
		}
		return t
	case int:
		return int64(t)
	case nil:
		return ""
	default:
		return v
	}
}

func mdSummary(m map[string]interface{}) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprint(m)
	}
	return string(b)
}

func sortedKeys(m map[string]interface{}) string {
	return strings.Join(sortedKeysMap(m), ",")
}

func sortedKeysMap(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func indexOf(list []string, v string) (int, bool) {
	for i, s := range list {
		if s == v {
			return i, true
		}
	}
	return -1, false
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case json.Number:
		i, _ := t.Int64()
		return i
	case int64:
		return t
	case float64:
		return int64(t)
	case []byte:
		i, _ := strconv.ParseInt(string(t), 10, 64)
		return i
	case string:
		i, _ := strconv.ParseInt(t, 10, 64)
		return i
	default:
		return 0
	}
}

func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case []byte:
		f, err := strconv.ParseFloat(string(t), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// scanMDRows 把任意 SELECT * 的结果按**数据库列类型**归一为 JSON 友好值。
func scanMDRows(rows *sql.Rows) ([]MDRow, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	raw := make([]interface{}, len(cols))
	ptrs := make([]interface{}, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	var out []MDRow
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := MDRow{}
		for i, c := range cols {
			row[c] = coerceOut(types[i], raw[i])
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func coerceOut(ct *sql.ColumnType, v interface{}) interface{} {
	switch t := v.(type) {
	case nil:
		return nil
	case time.Time:
		return t.Format("2006-01-02T15:04:05.000") + tzSuffix(t)
	case []byte:
		s := string(t)
		if numericDBType(ct.DatabaseTypeName()) {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
			}
		}
		return s
	case string:
		return t
	case int64:
		return json.Number(strconv.FormatInt(t, 10))
	case float64:
		return json.Number(strconv.FormatFloat(t, 'f', -1, 64))
	case bool:
		if t {
			return json.Number("1")
		}
		return json.Number("0")
	default:
		return fmt.Sprint(v)
	}
}

func tzSuffix(t time.Time) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s%02d:%02d", sign, off/3600, (off%3600)/60)
}

func numericDBType(dt string) bool {
	switch strings.ToUpper(dt) {
	case "DECIMAL", "NUMERIC", "FLOAT", "DOUBLE", "REAL",
		"INT", "INTEGER", "TINYINT", "SMALLINT", "MEDIUMINT", "BIGINT",
		"YEAR":
		return true
	}
	return false
}

// isDuplicateErr 判定 MySQL 1062（唯一键冲突）。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
