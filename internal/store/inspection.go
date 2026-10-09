package store

// ===== M5 检测 · 库侧（D1~D7）=====
//
// ★★ 三条硬口径（MIMO-NEXT-BATCH-05 §1 / §6）：
//	① 检测挂「大样」（b_inspection.group_id），不挂袋 —— 建单必须挂现行取样组，
//	  无大样 ⇒ ErrInspNoSample（400），但任务列表照常列出该对象（TC-M5-14）；
//	② 清单 = b_inspection_result 的行集合（含 未测 行），不另建表；三态不可合并；
//	③ 数据不可变（P1）：结果只能从「未测」单向写一次（UPDATE 必带 AND state='未测'
//	  且校验受影响行数 == 1）；结论/双签同理带 conclusion IS NULL / 签署列空守卫；
//	  其余更正一律「作废原单（b_obj_void）+ 新开单」，不得原地 UPDATE。
//
// ★「现行单」= 该 (target_type, target_id) 上未被 b_obj_void 作废的最新一张
//	（任务包 §6-5）—— 列表 / 退车前置 / 车次状态回写只认现行单。
// ★ 判定限复用批 2 的 LookupLimit（内部走 ResolveLimit 纯函数），本文件不另写一套。
// ★ 紧急放行落 s_audit_log（§6-10 我方定案）：不新增表、不新增列；
//	服务层同人校验 + 权限层 INIT/APPROVE 双保障（§6-11）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
)

// M5 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrInspBadInput             = errors.New("检测输入不合法")
	ErrInspNotFound             = errors.New("检测单不存在")
	ErrInspNoSample             = errors.New("尚未取样，无法检测")
	ErrInspTargetUnknown        = errors.New("被检对象不存在")
	ErrInspState                = errors.New("检测单当前状态不允许该操作")
	ErrInspResultRecorded       = errors.New("结果已录入，如需更改请走修正")
	ErrInspConclusionSet        = errors.New("已有结论，如需更改请走修正/复检")
	ErrInspDispositionSet       = errors.New("已填写处置")
	ErrInspNeedConclusion       = errors.New("尚未出结论，不能填写处置")
	ErrInspConcessionIncomplete = errors.New("让步接收必填四字段（授权人/何时告知客户/告知谁/渠道）")
	ErrInspBadDisposition       = errors.New("处置与结论不匹配")
	ErrInspSigned               = errors.New("该方已签署")
	ErrInspItemInvalid          = errors.New("检测项不存在或未启用")
	ErrInspSeqOverflow          = errors.New("当日检测单号超过 999，需人工决策（编号不自动加宽）")
	ErrInspVoided               = errors.New("检测单已作废")
	ErrInspVoidReason           = errors.New("修正必须填写原因")
	ErrUrgentNotInit            = errors.New("紧急放行尚未发起")
	ErrUrgentSameActor          = errors.New("发起人不得自批（发起 ≠ 审批）")
	ErrUrgentAlready            = errors.New("紧急放行已发起/已审批")
)

// 枚举字面量（★ 一律用 spec/schema.sql 列注释里的中文原词，任务包 §6-13）。
const (
	InspStateTodo      = "未测"
	InspStateDone      = "已测"
	InspStateNA        = "不适用"
	InspJudgePass      = "合格"
	InspJudgeFail      = "不合格"
	InspConclusionPass = "合格"
	InspConclusionFail = "不合格"
	InspConclusionCons = "CONCESSION" // 界面显示「让步接收」（D14）

	InspTargetTruck = TargetTruck // 车次
	InspTargetBatch = TargetBatch // 生产批
	InspTargetFgLot = TargetFgLot // 成品批

	TruckStatusQualified  = "合格"
	TruckStatusUnqualif   = "不合格"
	TruckStatusConcession = "让步接收"
)

// validInspTargetType 被检对象类型白名单。
func validInspTargetType(t string) bool {
	switch t {
	case InspTargetTruck, InspTargetBatch, InspTargetFgLot:
		return true
	}
	return false
}

// ===== 检测单结构 =====

// Inspection 是 b_inspection 的一行。
type Inspection struct {
	ID             int64  `json:"id"`
	InspectionNo   string `json:"inspection_no"`
	TargetType     string `json:"target_type"`
	TargetID       int64  `json:"target_id"`
	GroupID        *int64 `json:"group_id,omitempty"`
	GroupNo        string `json:"group_no,omitempty"` // 关联取样组的 group_no（= 大样编号）
	TestDate       string `json:"test_date,omitempty"`
	Inspector      string `json:"inspector,omitempty"`
	Method         string `json:"method,omitempty"`
	Conclusion     string `json:"conclusion,omitempty"`
	DefectDesc     string `json:"defect_desc,omitempty"`
	Disposition    string `json:"disposition,omitempty"`
	AuthorizedBy   string `json:"authorized_by,omitempty"`
	QcSignedBy     string `json:"qc_signed_by,omitempty"`
	DeptSignedBy   string `json:"dept_signed_by,omitempty"`
	CustNotifiedAt string `json:"cust_notified_at,omitempty"`
	CustContact    string `json:"cust_contact,omitempty"`
	CustChannel    string `json:"cust_channel,omitempty"`
	IsRecheck      bool   `json:"is_recheck"`
	RecheckOf      *int64 `json:"recheck_of,omitempty"`
	Remark         string `json:"remark,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	CreatedBy      string `json:"created_by,omitempty"`
	Voided         bool   `json:"voided"` // 现行判定：是否已被 b_obj_void 作废
}

const inspSelect = `
SELECT i.id, i.inspection_no, i.target_type, i.target_id, i.group_id,
       IFNULL(g.group_no, ''), IFNULL(DATE_FORMAT(i.test_date, '%Y-%m-%d'), ''),
       IFNULL(i.inspector, ''), IFNULL(i.method, ''),
       IFNULL(i.conclusion, ''), IFNULL(i.defect_desc, ''), IFNULL(i.disposition, ''),
       IFNULL(i.authorized_by, ''), IFNULL(i.qc_signed_by, ''), IFNULL(i.dept_signed_by, ''),
       IFNULL(DATE_FORMAT(i.cust_notified_at, '%Y-%m-%d %H:%i:%s'), ''),
       IFNULL(i.cust_contact, ''), IFNULL(i.cust_channel, ''),
       i.is_recheck, i.recheck_of, IFNULL(i.remark, ''),
       DATE_FORMAT(i.created_at, '%Y-%m-%d %H:%i:%s'), i.created_by,
       EXISTS(SELECT 1 FROM b_obj_void v
               WHERE v.entity = 'b_inspection' AND v.entity_id = i.id)
  FROM b_inspection i
  LEFT JOIN b_sample_group g ON g.id = i.group_id`

func scanInsp(row interface{ Scan(...interface{}) error }) (Inspection, error) {
	var it Inspection
	var groupID, recheckOf sql.NullInt64
	var isRecheck, voided int
	err := row.Scan(&it.ID, &it.InspectionNo, &it.TargetType, &it.TargetID, &groupID,
		&it.GroupNo, &it.TestDate, &it.Inspector, &it.Method,
		&it.Conclusion, &it.DefectDesc, &it.Disposition,
		&it.AuthorizedBy, &it.QcSignedBy, &it.DeptSignedBy,
		&it.CustNotifiedAt, &it.CustContact, &it.CustChannel,
		&isRecheck, &recheckOf, &it.Remark,
		&it.CreatedAt, &it.CreatedBy, &voided)
	if err != nil {
		return Inspection{}, err
	}
	if groupID.Valid {
		v := groupID.Int64
		it.GroupID = &v
	}
	if recheckOf.Valid {
		v := recheckOf.Int64
		it.RecheckOf = &v
	}
	it.IsRecheck = isRecheck == 1
	it.Voided = voided == 1
	return it, nil
}

// GetInspection 按 id 取检测单。
func (s *Store) GetInspection(ctx context.Context, id int64) (Inspection, error) {
	it, err := scanInsp(s.db.QueryRowContext(ctx, inspSelect+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspNotFound, id)
	}
	if err != nil {
		return Inspection{}, fmt.Errorf("读取检测单失败: %w", err)
	}
	return it, nil
}

// currentInspectionIDTx 取该对象的「现行单」id（未被作废的最新一张）。
func currentInspectionIDTx(ctx context.Context, q queryRower, targetType string, targetID int64) (sql.NullInt64, error) {
	var id sql.NullInt64
	err := q.QueryRowContext(ctx, `
SELECT i.id FROM b_inspection i
  LEFT JOIN b_obj_void v ON v.entity = 'b_inspection' AND v.entity_id = i.id
 WHERE i.target_type = ? AND i.target_id = ? AND v.id IS NULL
 ORDER BY i.id DESC LIMIT 1`, targetType, targetID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, nil
	}
	if err != nil {
		return sql.NullInt64{}, fmt.Errorf("查询现行检测单失败: %w", err)
	}
	return id, nil
}

// queryRower 是 *sql.DB 与 *sql.Tx 的公共查询接口。
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// ===== 取号（§6-4：JC + YYMMDD + '-' + 3 位当日序号）=====

// nextInspectionNo 在**持命名锁的事务内**取当日检测单号（超 999 明确报错）。
func nextInspectionNo(ctx context.Context, tx *sql.Tx, now time.Time) (string, error) {
	prefix := fmt.Sprintf("JC%s-", now.Format("060102"))
	rows, err := tx.QueryContext(ctx,
		`SELECT inspection_no FROM b_inspection WHERE inspection_no LIKE ?`, prefix+"%")
	if err != nil {
		return "", fmt.Errorf("读取检测单号失败: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return "", fmt.Errorf("读取检测单号失败: %w", err)
		}
		n, err := strconv.Atoi(strings.TrimPrefix(no, prefix))
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	next := max + 1
	if next > 999 {
		return "", ErrInspSeqOverflow
	}
	return fmt.Sprintf("%s%03d", prefix, next), nil
}

// inspSeqLockName 检测单号空间的命名锁名（≤64 字符）。
func inspSeqLockName(now time.Time) string {
	return "jxlab.insp." + now.Format("060102")
}

// ===== D2 · 建单（挂大样）=====

// CreateInspInput 是建单入参。
type CreateInspInput struct {
	TargetType string `json:"target_type"` // 车次 / 生产批 / 成品批
	TargetID   int64  `json:"target_id"`
	Inspector  string `json:"inspector"` // 可空
	Method     string `json:"method"`    // 可空：全检 / 抽检
	TestDate   string `json:"test_date"` // 可空 ⇒ 服务端当日（YYYY-MM-DD）
	Remark     string `json:"remark"`
}

// CreateInspection 建检测单：必须挂该对象的**现行取样组（大样）**。
//
// ★ 无大样 ⇒ ErrInspNoSample（400，「尚未取样，无法检测」）；
// ★ 并反向校验取样组的 target_type/target_id 与被检对象一致（防串挂）。
func (s *Store) CreateInspection(ctx context.Context, in CreateInspInput, actor MDActor) (Inspection, error) {
	if !validInspTargetType(in.TargetType) {
		return Inspection{}, fmt.Errorf("%w：target_type 必须是 车次/生产批/成品批，实际 %q", ErrInspBadInput, in.TargetType)
	}
	if err := s.checkInspTargetExists(ctx, in.TargetType, in.TargetID); err != nil {
		return Inspection{}, err
	}
	groupID, groupNo, err := s.currentSampleGroup(ctx, in.TargetType, in.TargetID)
	if err != nil {
		return Inspection{}, err
	}
	testDate := strings.TrimSpace(in.TestDate)
	if testDate == "" {
		testDate = time.Now().Format("2006-01-02")
	} else if _, err := time.Parse("2006-01-02", testDate); err != nil {
		return Inspection{}, fmt.Errorf("%w：test_date 须为 YYYY-MM-DD", ErrInspBadInput)
	}
	_ = groupNo

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Inspection{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	now := time.Now()
	lock := inspSeqLockName(now)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return Inspection{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Inspection{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	no, err := nextInspectionNo(ctx, tx, now)
	if err != nil {
		return Inspection{}, err
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_inspection
  (inspection_no, target_type, target_id, group_id, test_date, inspector, method,
   remark, created_by)
VALUES (?,?,?,?,?,?,?,?,?)`,
		no, in.TargetType, in.TargetID, groupID, testDate,
		nullIfEmptyStr(in.Inspector), nullIfEmptyStr(in.Method),
		nullIfEmptyStr(in.Remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return Inspection{}, fmt.Errorf("%w：单号冲突 %s", ErrInspBadInput, no)
		}
		return Inspection{}, fmt.Errorf("创建检测单失败: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("%s %s#%d group=%d", no, in.TargetType, in.TargetID, groupID),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: in.Remark,
	}); err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Inspection{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetInspection(ctx, id)
}

// checkInspTargetExists 被检对象必须存在（不存在 ⇒ 404）。
func (s *Store) checkInspTargetExists(ctx context.Context, targetType string, targetID int64) error {
	table, err := inspTargetTable(targetType)
	if err != nil {
		return err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, targetID).Scan(&n); err != nil {
		return fmt.Errorf("查询被检对象失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w：%s #%d", ErrInspTargetUnknown, targetType, targetID)
	}
	return nil
}

func inspTargetTable(targetType string) (string, error) {
	switch targetType {
	case InspTargetTruck:
		return "b_truck_lot", nil
	case InspTargetBatch:
		return "b_production_batch", nil
	case InspTargetFgLot:
		return "b_fg_lot", nil
	}
	return "", fmt.Errorf("%w：target_type 必须是 车次/生产批/成品批", ErrInspBadInput)
}

// currentSampleGroup 取该对象的现行取样组（大样）—— 无 ⇒ ErrInspNoSample。
// ★ 反向校验：组的 target_type/target_id 必须与被检对象一致（查询即校验，另再断言一次防串挂）。
func (s *Store) currentSampleGroup(ctx context.Context, targetType string, targetID int64) (int64, string, error) {
	var id int64
	var gType string
	var gTarget int64
	var groupNo string
	err := s.db.QueryRowContext(ctx, `
SELECT id, target_type, target_id, group_no FROM b_sample_group
 WHERE target_type = ? AND target_id = ? ORDER BY id DESC LIMIT 1`,
		targetType, targetID).Scan(&id, &gType, &gTarget, &groupNo)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("%w：%s #%d", ErrInspNoSample, targetType, targetID)
	}
	if err != nil {
		return 0, "", fmt.Errorf("查询取样组失败: %w", err)
	}
	if gType != targetType || gTarget != targetID {
		return 0, "", fmt.Errorf("%w：取样组 #%d 归属 %s#%d，与被检对象不符（防串挂）",
			ErrInspBadInput, id, gType, gTarget)
	}
	return id, groupNo, nil
}

// ===== D2 · 批次检测项清单（= b_inspection_result 行集合）=====

// InspResultRow 是 b_inspection_result 的一行（带字典信息）。
type InspResultRow struct {
	ID           int64    `json:"id"`
	InspectionID int64    `json:"inspection_id"`
	ItemID       int64    `json:"item_id"`
	ItemCode     string   `json:"item_code"`
	ItemName     string   `json:"item_name"`
	Unit         string   `json:"unit"`
	Method       string   `json:"method"`
	ValueType    string   `json:"value_type"`
	EnumValues   string   `json:"enum_values,omitempty"`
	State        string   `json:"state"` // 未测 / 已测 / 不适用
	ValueNum     *float64 `json:"value_num,omitempty"`
	ValueText    string   `json:"value_text,omitempty"`
	RowUnit      string   `json:"row_unit,omitempty"`
	Judge        string   `json:"judge,omitempty"`
	Source       string   `json:"source"`
	LowerLimit   *float64 `json:"lower_limit,omitempty"`
	UpperLimit   *float64 `json:"upper_limit,omitempty"`
	Remark       string   `json:"remark,omitempty"`
	CreatedAt    string   `json:"created_at,omitempty"`
	CreatedBy    string   `json:"created_by,omitempty"`
}

const inspResultSelect = `
SELECT r.id, r.inspection_id, r.item_id, t.code, t.name,
       IFNULL(t.unit, ''), IFNULL(t.method, ''), t.value_type, IFNULL(t.enum_values, ''),
       r.state, r.value_num, IFNULL(r.value_text, ''), IFNULL(r.unit, ''),
       IFNULL(r.judge, ''), r.source, r.lower_limit, r.upper_limit,
       IFNULL(r.remark, ''), DATE_FORMAT(r.created_at, '%Y-%m-%d %H:%i:%s'), r.created_by
  FROM b_inspection_result r
  JOIN m_test_item t ON t.id = r.item_id`

func scanInspResult(row interface{ Scan(...interface{}) error }) (InspResultRow, error) {
	var r InspResultRow
	var valueNum, lower, upper sql.NullFloat64
	err := row.Scan(&r.ID, &r.InspectionID, &r.ItemID, &r.ItemCode, &r.ItemName,
		&r.Unit, &r.Method, &r.ValueType, &r.EnumValues,
		&r.State, &valueNum, &r.ValueText, &r.RowUnit,
		&r.Judge, &r.Source, &lower, &upper,
		&r.Remark, &r.CreatedAt, &r.CreatedBy)
	if err != nil {
		return InspResultRow{}, err
	}
	if valueNum.Valid {
		v := valueNum.Float64
		r.ValueNum = &v
	}
	if lower.Valid {
		v := lower.Float64
		r.LowerLimit = &v
	}
	if upper.Valid {
		v := upper.Float64
		r.UpperLimit = &v
	}
	return r, nil
}

// ListInspectionItems 返回该单**全部**清单行（含 未测 行）+ 字典信息。
func (s *Store) ListInspectionItems(ctx context.Context, inspectionID int64) ([]InspResultRow, error) {
	if _, err := s.GetInspection(ctx, inspectionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, inspResultSelect+` WHERE r.inspection_id = ? ORDER BY r.id`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("查询检测清单失败: %w", err)
	}
	defer rows.Close()
	var out []InspResultRow
	for rows.Next() {
		r, err := scanInspResult(rows)
		if err != nil {
			return nil, fmt.Errorf("读取检测清单失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AddInspectionItems 加项：逐项插入 state='未测' 行（item 必须存在、启用、is_current=1）。
// ★ 已录值后仍**允许**加项（TC-M5-03）。
func (s *Store) AddInspectionItems(ctx context.Context, inspectionID int64, itemIDs []int64, actor MDActor) ([]InspResultRow, error) {
	if len(itemIDs) == 0 {
		return nil, fmt.Errorf("%w：item_ids 不能为空", ErrInspBadInput)
	}
	if _, err := s.GetInspection(ctx, inspectionID); err != nil {
		return nil, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	added := 0
	for _, itemID := range itemIDs {
		var name string
		err := tx.QueryRowContext(ctx, `
SELECT name FROM m_test_item
 WHERE id = ? AND status = '启用' AND is_current = 1`, itemID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w：#%d", ErrInspItemInvalid, itemID)
		}
		if err != nil {
			return nil, fmt.Errorf("校验检测项失败: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_inspection_result (inspection_id, item_id, state, created_by)
VALUES (?, ?, ?, ?)`, inspectionID, itemID, InspStateTodo, actor.OpenID)
		if err != nil {
			if isDuplicateErr(err) {
				return nil, fmt.Errorf("%w：该项已在清单中（inspection=%d item=%d）", ErrInspState, inspectionID, itemID)
			}
			return nil, fmt.Errorf("加项失败: %w", err)
		}
		rid, _ := res.LastInsertId()
		added++
		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: "b_inspection_result", EntityID: rid, Action: "add",
			NewValue:    fmt.Sprintf("inspection=%d item=%d %s", inspectionID, itemID, name),
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		}); err != nil {
			return nil, err
		}
	}
	if added > 0 {
		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: "b_inspection", EntityID: inspectionID, Action: "add_items",
			NewValue:    fmt.Sprintf("新增 %d 项", added),
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.ListInspectionItems(ctx, inspectionID)
}

// DeleteInspectionItem 删项：★★ **仅当被删项自身 state='未测'** 才允许（§6-6 宽读定案）。
//
//	已测/不适用 ⇒ ErrInspResultRecorded（409，TC-M5-02）。
//	未测行 = 物理 DELETE（P1 明文例外：不含业务事实的待办行），
//	★ 但必须写审计（action='delete'，old_value 记 item 快照）。
func (s *Store) DeleteInspectionItem(ctx context.Context, inspectionID, itemID int64, actor MDActor) error {
	if _, err := s.GetInspection(ctx, inspectionID); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var rowID int64
	var state string
	var valueNum sql.NullFloat64
	var valueText, judge string
	err = tx.QueryRowContext(ctx, `
SELECT id, state, value_num, IFNULL(value_text, ''), IFNULL(judge, '')
  FROM b_inspection_result WHERE inspection_id = ? AND item_id = ? FOR UPDATE`,
		inspectionID, itemID).Scan(&rowID, &state, &valueNum, &valueText, &judge)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：该项不在清单中", ErrInspState)
	}
	if err != nil {
		return fmt.Errorf("读取清单项失败: %w", err)
	}
	if state != InspStateTodo {
		return fmt.Errorf("%w：该项为「%s」，不可删除", ErrInspResultRecorded, state)
	}
	snapshot := fmt.Sprintf("inspection=%d item=%d state=%s", inspectionID, itemID, state)
	if valueNum.Valid {
		snapshot += fmt.Sprintf(" value_num=%g", valueNum.Float64)
	}
	if valueText != "" {
		snapshot += " value_text=" + valueText
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM b_inspection_result WHERE id = ? AND state = ?`, rowID, InspStateTodo); err != nil {
		return fmt.Errorf("删除清单项失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection_result", EntityID: rowID, Action: "delete",
		OldValue:    snapshot,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ===== D3 · 结果三态录入（P1 单向一次性写）=====

// RecordInspInput 是结果录录入入参。
type RecordInspInput struct {
	InspectionID int64    `json:"inspection_id"`
	ItemID       int64    `json:"item_id"`
	State        string   `json:"state"` // 已测 / 不适用（目标态；行当前必须是 未测）
	ValueNum     *float64 `json:"value_num"`
	ValueText    string   `json:"value_text"`
	Unit         string   `json:"unit"`
	Remark       string   `json:"remark"`
}

// RecordInspectionResult 录入结果：
//
//	★★ UPDATE 必带 `AND state='未测'` 且校验受影响行数恰为 1（≠1 ⇒ ErrInspResultRecorded）；
//	已测行再次录入 ⇒ 拒绝（409），原值不动（A13/A19）。
//	判定限快照：state=已测 且 value_type=数值 时按被检对象 (customer, material) 调
//	store.LookupLimit（复用 ResolveLimit）快照进 lower/upper；judge 按口径判定。
func (s *Store) RecordInspectionResult(ctx context.Context, in RecordInspInput, actor MDActor) (InspResultRow, error) {
	if in.State != InspStateDone && in.State != InspStateNA {
		return InspResultRow{}, fmt.Errorf("%w：state 只能是 已测/不适用（当前态须为 未测）", ErrInspBadInput)
	}
	insp, err := s.GetInspection(ctx, in.InspectionID)
	if err != nil {
		return InspResultRow{}, err
	}
	if insp.Voided {
		return InspResultRow{}, fmt.Errorf("%w：#%d", ErrInspVoided, in.InspectionID)
	}

	var valueType, itemName string
	var unit sql.NullString
	err = s.db.QueryRowContext(ctx, `
SELECT value_type, name, unit FROM m_test_item WHERE id = ?`, in.ItemID).
		Scan(&valueType, &itemName, &unit)
	if errors.Is(err, sql.ErrNoRows) {
		return InspResultRow{}, fmt.Errorf("%w：#%d", ErrInspItemInvalid, in.ItemID)
	}
	if err != nil {
		return InspResultRow{}, fmt.Errorf("读取检测项失败: %w", err)
	}

	// 判定限快照（仅 已测 + 数值）
	var lower, upper *float64
	var judge string
	if in.State == InspStateDone && valueType == "数值" {
		custID, matID, err := s.inspTargetScope(ctx, insp.TargetType, insp.TargetID)
		if err != nil {
			return InspResultRow{}, err
		}
		hit, ok, err := s.LookupLimit(ctx, in.ItemID, custID, matID)
		if err != nil {
			return InspResultRow{}, err
		}
		if ok {
			lower = limitToFloat(hit.Lower)
			upper = limitToFloat(hit.Upper)
		}
		if in.ValueNum != nil && lower != nil && upper != nil {
			v := *in.ValueNum
			switch {
			case v < *lower || v > *upper:
				judge = InspJudgeFail
			default:
				judge = InspJudgePass
			}
		}
	}
	if in.State == InspStateNA {
		// 三态不可合并：不适用 = 结论，value/judge 一律留空（§6-14）
		in.ValueNum = nil
		in.ValueText = ""
		judge = ""
		lower, upper = nil, nil
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return InspResultRow{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return InspResultRow{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var rowID int64
	var curState string
	err = tx.QueryRowContext(ctx, `
SELECT id, state FROM b_inspection_result
 WHERE inspection_id = ? AND item_id = ? FOR UPDATE`, in.InspectionID, in.ItemID).
		Scan(&rowID, &curState)
	if errors.Is(err, sql.ErrNoRows) {
		return InspResultRow{}, fmt.Errorf("%w：该项不在清单中", ErrInspState)
	}
	if err != nil {
		return InspResultRow{}, fmt.Errorf("读取清单项失败: %w", err)
	}
	if curState != InspStateTodo {
		return InspResultRow{}, fmt.Errorf("%w：该项当前「%s」", ErrInspResultRecorded, curState)
	}

	rowUnit := strings.TrimSpace(in.Unit)
	if rowUnit == "" && unit.Valid {
		rowUnit = unit.String
	}
	res, err := tx.ExecContext(ctx, `
UPDATE b_inspection_result
   SET state = ?, value_num = ?, value_text = ?, unit = ?, judge = ?,
       lower_limit = ?, upper_limit = ?, remark = ?
 WHERE id = ? AND state = '未测'`,
		in.State, in.ValueNum, nullIfEmptyStr(in.ValueText), nullIfEmptyStr(rowUnit),
		nullIfEmptyStr(judge), lower, upper, nullIfEmptyStr(in.Remark), rowID)
	if err != nil {
		return InspResultRow{}, fmt.Errorf("录入结果失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return InspResultRow{}, fmt.Errorf("校验录入结果失败: %w", err)
	}
	if n != 1 {
		// 无 state 守卫的裸 UPDATE 到这里必被拦下（A19）
		return InspResultRow{}, fmt.Errorf("%w：受影响行数 %d ≠ 1", ErrInspResultRecorded, n)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection_result", EntityID: rowID, Action: "record",
		OldValue: InspStateTodo, NewValue: in.State,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return InspResultRow{}, err
	}
	if err := tx.Commit(); err != nil {
		return InspResultRow{}, fmt.Errorf("提交失败: %w", err)
	}

	// 回读
	rows, err := s.ListInspectionItems(ctx, in.InspectionID)
	if err != nil {
		return InspResultRow{}, err
	}
	for _, r := range rows {
		if r.ItemID == in.ItemID {
			return r, nil
		}
	}
	return InspResultRow{}, fmt.Errorf("%w：回读清单项失败", ErrInspState)
}

// limitToFloat 把 LookupLimit 的 interface{} 限制值转成 *float64（nil 表示无限制）。
func limitToFloat(v interface{}) *float64 {
	switch t := v.(type) {
	case nil:
		return nil
	case float64:
		return &t
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil
		}
		return &f
	case string:
		if t == "" {
			return nil
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	return nil
}

// inspTargetScope 按被检对象推 (customer_id, material_id)（任务包 §3-D3 / §6-15）：
//
//	车次 ⇒ b_truck_lot 的 customer/material；生产批 ⇒ customer + input_material；
//	成品批 ⇒ customer + output_material。
func (s *Store) inspTargetScope(ctx context.Context, targetType string, targetID int64) (int64, int64, error) {
	var q string
	switch targetType {
	case InspTargetTruck:
		q = `SELECT customer_id, material_id FROM b_truck_lot WHERE id = ?`
	case InspTargetBatch:
		q = `SELECT customer_id, input_material_id FROM b_production_batch WHERE id = ?`
	case InspTargetFgLot:
		q = `SELECT customer_id, output_material_id FROM b_fg_lot WHERE id = ?`
	default:
		return 0, 0, fmt.Errorf("%w：target_type %q", ErrInspBadInput, targetType)
	}
	var cust, mat int64
	if err := s.db.QueryRowContext(ctx, q, targetID).Scan(&cust, &mat); err != nil {
		return 0, 0, fmt.Errorf("读取被检对象判定限维度失败: %w", err)
	}
	return cust, mat, nil
}

// ===== D5 · 出结论 + 处置 + 让步双签 =====

// ConclusionInput 是出结论入参。
type ConclusionInput struct {
	InspectionID   int64  `json:"inspection_id"`
	Conclusion     string `json:"conclusion"` // 合格 / 不合格 / CONCESSION
	DefectDesc     string `json:"defect_desc"`
	Remark         string `json:"remark"`
	AuthorizedBy   string `json:"authorized_by"`    // 让步必填
	CustNotifiedAt string `json:"cust_notified_at"` // 让步必填 YYYY-MM-DD HH:MM:SS
	CustContact    string `json:"cust_contact"`     // 让步必填
	CustChannel    string `json:"cust_channel"`     // 让步必填 电话/微信/邮件/书面
}

// validCustChannel 让步渠道白名单（§6-13 枚举原词）。
func validCustChannel(ch string) bool {
	switch ch {
	case "电话", "微信", "邮件", "书面":
		return true
	}
	return false
}

// SetConclusion 出结论：
//
//	★ 单向一次：UPDATE ... WHERE id = ? AND conclusion IS NULL，受影响行数 ≠ 1 ⇒ 409；
//	★ CONCESSION 同一次必须提交四字段（授权人/何时告知客户/告知谁/渠道），缺任一 ⇒ 400（TC-M5-08）；
//	★ target_type=车次 时同步 b_truck_lot.status（合格/不合格/让步接收）；生产批/成品批不回写。
func (s *Store) SetConclusion(ctx context.Context, in ConclusionInput, actor MDActor) (Inspection, error) {
	switch in.Conclusion {
	case InspConclusionPass, InspConclusionFail, InspConclusionCons:
	default:
		return Inspection{}, fmt.Errorf("%w：conclusion 只能是 合格/不合格/CONCESSION", ErrInspBadInput)
	}
	insp, err := s.GetInspection(ctx, in.InspectionID)
	if err != nil {
		return Inspection{}, err
	}
	if insp.Voided {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspVoided, in.InspectionID)
	}
	if in.Conclusion == InspConclusionCons {
		if strings.TrimSpace(in.AuthorizedBy) == "" ||
			strings.TrimSpace(in.CustNotifiedAt) == "" ||
			strings.TrimSpace(in.CustContact) == "" ||
			strings.TrimSpace(in.CustChannel) == "" {
			return Inspection{}, fmt.Errorf("%w：授权人 / 何时告知客户 / 告知谁 / 渠道 均必填", ErrInspConcessionIncomplete)
		}
		if !validCustChannel(in.CustChannel) {
			return Inspection{}, fmt.Errorf("%w：cust_channel 只能是 电话/微信/邮件/书面", ErrInspBadInput)
		}
		if _, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(in.CustNotifiedAt)); err != nil {
			return Inspection{}, fmt.Errorf("%w：cust_notified_at 须为 YYYY-MM-DD HH:MM:SS", ErrInspBadInput)
		}
	}

	truckStatus := ""
	if insp.TargetType == InspTargetTruck {
		switch in.Conclusion {
		case InspConclusionPass:
			truckStatus = TruckStatusQualified
		case InspConclusionFail:
			truckStatus = TruckStatusUnqualif
		case InspConclusionCons:
			truckStatus = TruckStatusConcession
		}
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Inspection{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Inspection{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var custNotified, authorized, contact, channel interface{}
	if in.Conclusion == InspConclusionCons {
		custNotified = strings.TrimSpace(in.CustNotifiedAt)
		authorized = strings.TrimSpace(in.AuthorizedBy)
		contact = strings.TrimSpace(in.CustContact)
		channel = strings.TrimSpace(in.CustChannel)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE b_inspection
   SET conclusion = ?, defect_desc = ?, remark = COALESCE(?, remark),
       authorized_by = ?, cust_notified_at = ?, cust_contact = ?, cust_channel = ?
 WHERE id = ? AND conclusion IS NULL`,
		in.Conclusion, nullIfEmptyStr(in.DefectDesc), nullIfEmptyStr(in.Remark),
		authorized, custNotified, contact, channel, in.InspectionID)
	if err != nil {
		return Inspection{}, fmt.Errorf("出结论失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Inspection{}, fmt.Errorf("校验出结论失败: %w", err)
	}
	if n != 1 {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspConclusionSet, in.InspectionID)
	}
	if truckStatus != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE b_truck_lot SET status = ? WHERE id = ?`, truckStatus, insp.TargetID); err != nil {
			return Inspection{}, fmt.Errorf("同步车次状态失败: %w", err)
		}
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection", EntityID: in.InspectionID, Action: "conclusion",
		OldValue: "", NewValue: in.Conclusion,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: in.Remark,
	}); err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Inspection{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetInspection(ctx, in.InspectionID)
}

// SetDisposition 填处置：
//
//	前置：本单已有 conclusion（否则 ErrInspNeedConclusion / 409）；
//	配套规则：合格 ⇒ 不得填；不合格 ⇒ 必填；CONCESSION ⇒ 必须恰为 让步接收；
//	★ 单向一次：WHERE ... AND disposition IS NULL；车次 + 退货 ⇒ status='已退货'。
func (s *Store) SetDisposition(ctx context.Context, inspectionID int64, disposition string, actor MDActor) (Inspection, error) {
	switch disposition {
	case "退货", "换货", "让步接收", "返工":
	default:
		return Inspection{}, fmt.Errorf("%w：disposition 只能是 退货/换货/让步接收/返工", ErrInspBadInput)
	}
	insp, err := s.GetInspection(ctx, inspectionID)
	if err != nil {
		return Inspection{}, err
	}
	if insp.Voided {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspVoided, inspectionID)
	}
	if insp.Conclusion == "" {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspNeedConclusion, inspectionID)
	}
	switch insp.Conclusion {
	case InspConclusionPass:
		return Inspection{}, fmt.Errorf("%w：结论为「合格」不得填处置", ErrInspBadDisposition)
	case InspConclusionCons:
		if disposition != "让步接收" {
			return Inspection{}, fmt.Errorf("%w：结论为 CONCESSION 时处置必须是「让步接收」", ErrInspBadDisposition)
		}
	}

	truckStatus := ""
	if insp.TargetType == InspTargetTruck && disposition == "退货" {
		truckStatus = TruckStatusReturned
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Inspection{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Inspection{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE b_inspection SET disposition = ? WHERE id = ? AND disposition IS NULL`,
		disposition, inspectionID)
	if err != nil {
		return Inspection{}, fmt.Errorf("填处置失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Inspection{}, fmt.Errorf("校验处置失败: %w", err)
	}
	if n != 1 {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspDispositionSet, inspectionID)
	}
	if truckStatus != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE b_truck_lot SET status = ? WHERE id = ?`, truckStatus, insp.TargetID); err != nil {
			return Inspection{}, fmt.Errorf("同步车次状态失败: %w", err)
		}
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection", EntityID: inspectionID, Action: "disposition",
		OldValue: "", NewValue: disposition,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Inspection{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetInspection(ctx, inspectionID)
}

// SignConcession 让步双签（qc / dept 两个入口，各自单向一次）。
//
//	前置：本单 conclusion = 'CONCESSION'；已签再签 ⇒ ErrInspSigned（409）。
//	★ 两个权限点、两个入口在 httpapi 层（insp.concession.qc_sign / dept_sign）。
func (s *Store) SignConcession(ctx context.Context, inspectionID int64, signer string, actor MDActor) (Inspection, error) {
	insp, err := s.GetInspection(ctx, inspectionID)
	if err != nil {
		return Inspection{}, err
	}
	if insp.Conclusion != InspConclusionCons {
		return Inspection{}, fmt.Errorf("%w：本单结论不是 CONCESSION，不能签署", ErrInspState)
	}
	col := "qc_signed_by"
	switch signer {
	case "qc":
		col = "qc_signed_by"
	case "dept":
		col = "dept_signed_by"
	default:
		return Inspection{}, fmt.Errorf("%w：signer 只能是 qc/dept", ErrInspBadInput)
	}
	signValue := actor.Name
	if strings.TrimSpace(signValue) == "" {
		signValue = actor.OpenID
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Inspection{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Inspection{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE b_inspection SET `+col+` = ? WHERE id = ? AND conclusion = 'CONCESSION' AND (`+col+` IS NULL OR `+col+` = '')`,
		signValue, inspectionID)
	if err != nil {
		return Inspection{}, fmt.Errorf("签署失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Inspection{}, fmt.Errorf("校验签署失败: %w", err)
	}
	if n != 1 {
		if insp.QcSignedBy != "" && signer == "qc" || insp.DeptSignedBy != "" && signer == "dept" {
			return Inspection{}, fmt.Errorf("%w：#%d %s 方已签署", ErrInspSigned, inspectionID, signer)
		}
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspState, inspectionID)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection", EntityID: inspectionID, Action: "sign_" + signer,
		Field: col, OldValue: "", NewValue: signValue,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Inspection{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetInspection(ctx, inspectionID)
}

// ===== D6 · 紧急放行（落 s_audit_log，§6-10 定案）=====

// urgentEntities 允许紧急放行的对象表（任务包 D6）。
var urgentEntities = map[string]bool{
	"b_truck_lot":        true,
	"b_production_batch": true,
	"b_fg_lot":           true,
}

// UrgentReleaseState 是某对象的紧急放行态（由审计行拼出）。
type UrgentReleaseState struct {
	Entity      string `json:"entity"`
	EntityID    int64  `json:"entity_id"`
	InitBy      string `json:"init_by,omitempty"`
	InitAt      string `json:"init_at,omitempty"`
	ApprovedBy  string `json:"approved_by,omitempty"`
	ApprovedAt  string `json:"approved_at,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Initialized bool   `json:"initialized"`
	Approved    bool   `json:"approved"`
}

func checkUrgentEntity(entity string) error {
	if !urgentEntities[entity] {
		return fmt.Errorf("%w：entity 只能是 b_truck_lot / b_production_batch / b_fg_lot", ErrInspBadInput)
	}
	return nil
}

// UrgentReleaseInit 紧急放行**发起**：审计落 action='urgent_release_init'（reason 必填）。
func (s *Store) UrgentReleaseInit(ctx context.Context, entity string, entityID int64, reason string, actor MDActor) (UrgentReleaseState, error) {
	if err := checkUrgentEntity(entity); err != nil {
		return UrgentReleaseState{}, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return UrgentReleaseState{}, fmt.Errorf("%w：reason（放行理由）必填", ErrInspBadInput)
	}
	if err := s.checkInspTargetExists(ctx, urgentTargetType(entity), entityID); err != nil {
		return UrgentReleaseState{}, err
	}
	cur, err := s.GetUrgentRelease(ctx, entity, entityID)
	if err != nil {
		return UrgentReleaseState{}, err
	}
	if cur.Initialized {
		return UrgentReleaseState{}, fmt.Errorf("%w：%s #%d", ErrUrgentAlready, entity, entityID)
	}
	if err := s.AppendAudit(ctx, audit.Entry{
		Entity: entity, EntityID: entityID, Action: "urgent_release_init",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: reason,
	}); err != nil {
		return UrgentReleaseState{}, err
	}
	return s.GetUrgentRelease(ctx, entity, entityID)
}

// UrgentReleaseApprove 紧急放行**审批**：
//
//	前置：必须先有 init 行（否则 409）；★★ 服务层同人校验：init 的 actor_open_id
//	等于本次审批人 ⇒ ErrUrgentSameActor（403）—— 仅靠权限层不算通过（§6-11）。
func (s *Store) UrgentReleaseApprove(ctx context.Context, entity string, entityID int64, reason string, actor MDActor) (UrgentReleaseState, error) {
	if err := checkUrgentEntity(entity); err != nil {
		return UrgentReleaseState{}, err
	}
	if err := s.checkInspTargetExists(ctx, urgentTargetType(entity), entityID); err != nil {
		return UrgentReleaseState{}, err
	}
	cur, err := s.GetUrgentRelease(ctx, entity, entityID)
	if err != nil {
		return UrgentReleaseState{}, err
	}
	if !cur.Initialized {
		return UrgentReleaseState{}, fmt.Errorf("%w：%s #%d", ErrUrgentNotInit, entity, entityID)
	}
	if cur.Approved {
		return UrgentReleaseState{}, fmt.Errorf("%w：%s #%d 已审批", ErrUrgentAlready, entity, entityID)
	}
	if cur.InitBy == actor.OpenID {
		return UrgentReleaseState{}, fmt.Errorf("%w：发起人 %s", ErrUrgentSameActor, actor.OpenID)
	}
	if err := s.AppendAudit(ctx, audit.Entry{
		Entity: entity, EntityID: entityID, Action: "urgent_release_approve",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: strings.TrimSpace(reason),
	}); err != nil {
		return UrgentReleaseState{}, err
	}
	return s.GetUrgentRelease(ctx, entity, entityID)
}

// GetUrgentRelease 查某对象的紧急放行态（读 s_audit_log）。
func (s *Store) GetUrgentRelease(ctx context.Context, entity string, entityID int64) (UrgentReleaseState, error) {
	if err := checkUrgentEntity(entity); err != nil {
		return UrgentReleaseState{}, err
	}
	st := UrgentReleaseState{Entity: entity, EntityID: entityID}
	rows, err := s.db.QueryContext(ctx, `
SELECT action, IFNULL(actor_open_id, ''), IFNULL(reason, ''),
       DATE_FORMAT(at, '%Y-%m-%d %H:%i:%s')
  FROM s_audit_log
 WHERE entity = ? AND entity_id = ? AND action IN ('urgent_release_init','urgent_release_approve')
 ORDER BY id ASC`, entity, entityID)
	if err != nil {
		return UrgentReleaseState{}, fmt.Errorf("查询紧急放行失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var action, openID, reason, at string
		if err := rows.Scan(&action, &openID, &reason, &at); err != nil {
			return UrgentReleaseState{}, fmt.Errorf("读取紧急放行失败: %w", err)
		}
		switch action {
		case "urgent_release_init":
			if !st.Initialized {
				st.Initialized = true
				st.InitBy = openID
				st.InitAt = at
				st.Reason = reason
			}
		case "urgent_release_approve":
			if !st.Approved {
				st.Approved = true
				st.ApprovedBy = openID
				st.ApprovedAt = at
				if st.Reason == "" {
					st.Reason = reason
				}
			}
		}
	}
	return st, rows.Err()
}

// urgentTargetType 把审计实体表名映射回 target_type。
func urgentTargetType(entity string) string {
	switch entity {
	case "b_truck_lot":
		return InspTargetTruck
	case "b_production_batch":
		return InspTargetBatch
	case "b_fg_lot":
		return InspTargetFgLot
	}
	return ""
}

// ===== D7 · 复检 + 结果修正 =====

// RecheckInspection 复检：**新开一张检测单**（is_recheck=1、recheck_of=原单），
// ★ 原单一切不动（不作废、结果一字不改 —— TC-M5-11）。
func (s *Store) RecheckInspection(ctx context.Context, inspectionID int64, actor MDActor) (Inspection, error) {
	return s.newFollowupInspection(ctx, inspectionID, true, "", actor)
}

// CorrectInspection 结果修正：① b_obj_void 作废原单（reason 必填、approved_by 留空 ——
// 更正类不审批）；② 新开一张检测单（is_recheck=0、recheck_of=原单）。
// ★ 原单的 b_inspection_result 行一字不动（TC-M5-12 红线）。
func (s *Store) CorrectInspection(ctx context.Context, inspectionID int64, reason string, actor MDActor) (Inspection, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Inspection{}, ErrInspVoidReason
	}
	return s.newFollowupInspection(ctx, inspectionID, false, reason, actor)
}

// newFollowupInspection 复检/修正的公共「新开单」路径。
//
//	recheck=true  ⇒ 复检（原单不动）；false ⇒ 修正（作废原单 + 新开单）。
func (s *Store) newFollowupInspection(ctx context.Context, inspectionID int64, recheck bool, reason string, actor MDActor) (Inspection, error) {
	orig, err := s.GetInspection(ctx, inspectionID)
	if err != nil {
		return Inspection{}, err
	}
	if orig.Voided {
		return Inspection{}, fmt.Errorf("%w：#%d", ErrInspVoided, inspectionID)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Inspection{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	now := time.Now()
	lock := inspSeqLockName(now)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return Inspection{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Inspection{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 修正：作废原单（b_obj_void；uk_void_entity 保证只作废一次；approved_by 留空）
	if !recheck {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO b_obj_void (entity, entity_id, reason, voided_by, approved_by, created_by)
VALUES ('b_inspection', ?, ?, ?, NULL, ?)`,
			inspectionID, reason, actor.Name, actor.OpenID); err != nil {
			if isDuplicateErr(err) {
				return Inspection{}, fmt.Errorf("%w：#%d", ErrInspVoided, inspectionID)
			}
			return Inspection{}, fmt.Errorf("作废原单失败: %w", err)
		}
		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: "b_inspection", EntityID: inspectionID, Action: "void",
			OldValue: orig.InspectionNo, NewValue: "作废（修正）",
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
			Reason: reason,
		}); err != nil {
			return Inspection{}, err
		}
	}

	no, err := nextInspectionNo(ctx, tx, now)
	if err != nil {
		return Inspection{}, err
	}
	inspector := orig.Inspector
	if strings.TrimSpace(inspector) == "" {
		inspector = actor.Name
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_inspection
  (inspection_no, target_type, target_id, group_id, test_date, inspector, method,
   is_recheck, recheck_of, remark, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		no, orig.TargetType, orig.TargetID, orig.GroupID, now.Format("2006-01-02"),
		nullIfEmptyStr(inspector), nullIfEmptyStr(orig.Method),
		boolTinyInt(recheck), inspectionID, nullIfEmptyStr(orig.Remark), actor.OpenID)
	if err != nil {
		return Inspection{}, fmt.Errorf("新开检测单失败: %w", err)
	}
	newID, _ := res.LastInsertId()
	action := "recheck"
	if !recheck {
		action = "correct"
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_inspection", EntityID: newID, Action: action,
		NewValue:    fmt.Sprintf("%s ← %s（原单 #%d）", no, orig.InspectionNo, inspectionID),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: reason,
	}); err != nil {
		return Inspection{}, err
	}
	if err := tx.Commit(); err != nil {
		return Inspection{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetInspection(ctx, newID)
}

func boolTinyInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ===== D1 · 检测任务列表（三来源并集）=====

// InspTaskRow 是检测任务列表的一行。
type InspTaskRow struct {
	TargetType   string `json:"target_type"`
	TargetID     int64  `json:"target_id"`
	TargetCode   string `json:"target_code"`
	TargetStatus string `json:"target_status"` // 对象自身业务状态
	InspState    string `json:"insp_state"`    // 未取样 / 待检 / 已检
	Hint         string `json:"hint,omitempty"`
	InspectionNo string `json:"inspection_no,omitempty"`
	Conclusion   string `json:"conclusion,omitempty"`
	Disposition  string `json:"disposition,omitempty"`
	InspectionID int64  `json:"inspection_id,omitempty"`
	TodoCount    int    `json:"todo_count"` // 现行单待办数（state=未测 行数）
}

// ListInspTasks 检测任务列表：车次 ∪ 生产批 ∪ 成品批。
//
//	★★ insp_state 判定（D1）：无现行大样 ⇒ 未取样；有现行单且 conclusion 空 ⇒ 待检；
//	有现行单且有 conclusion ⇒ 已检。★ 未取样照常列出（hint=尚未取样，TC-M5-14）。
//	★ status='已作废' 的对象不列出。
func (s *Store) ListInspTasks(ctx context.Context, stateFilter string) ([]InspTaskRow, error) {
	switch stateFilter {
	case "", "未取样", "待检", "已检":
	default:
		return nil, fmt.Errorf("%w：state 只能是 未取样/待检/已检", ErrInspBadInput)
	}
	q := `
SELECT tgt.target_type, tgt.id, tgt.code, tgt.status,
       EXISTS(SELECT 1 FROM b_sample_group g WHERE g.target_type = tgt.target_type AND g.target_id = tgt.id),
       IFNULL(i.id, 0), IFNULL(i.inspection_no, ''), IFNULL(i.conclusion, ''), IFNULL(i.disposition, '')
  FROM (
    SELECT '车次' AS target_type, t.id, t.code, t.status
      FROM b_truck_lot t WHERE t.status <> '已作废'
    UNION ALL
    SELECT '生产批', b.id, b.code, b.status
      FROM b_production_batch b WHERE b.status <> '已作废'
    UNION ALL
    SELECT '成品批', f.id, f.code, f.status
      FROM b_fg_lot f WHERE f.status <> '已作废'
  ) tgt
  LEFT JOIN b_inspection i ON i.id = (
      SELECT i2.id FROM b_inspection i2
        LEFT JOIN b_obj_void v ON v.entity = 'b_inspection' AND v.entity_id = i2.id
       WHERE i2.target_type = tgt.target_type AND i2.target_id = tgt.id AND v.id IS NULL
       ORDER BY i2.id DESC LIMIT 1)
 ORDER BY tgt.target_type, tgt.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询检测任务列表失败: %w", err)
	}
	defer rows.Close()

	var out []InspTaskRow
	for rows.Next() {
		var r InspTaskRow
		var hasGroup int
		var inspID int64
		if err := rows.Scan(&r.TargetType, &r.TargetID, &r.TargetCode, &r.TargetStatus,
			&hasGroup, &inspID, &r.InspectionNo, &r.Conclusion, &r.Disposition); err != nil {
			return nil, fmt.Errorf("读取检测任务列表失败: %w", err)
		}
		switch {
		case hasGroup == 0:
			r.InspState = "未取样"
			r.Hint = "尚未取样"
		case r.Conclusion == "":
			r.InspState = "待检"
		default:
			r.InspState = "已检"
		}
		if inspID > 0 {
			r.InspectionID = inspID
			if r.InspState == "待检" {
				if n, err := s.inspTodoCount(ctx, inspID); err == nil {
					r.TodoCount = n
				}
			}
		}
		if stateFilter != "" && r.InspState != stateFilter {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// inspTodoCount 现行单待办数（state=未测 行数）。
func (s *Store) inspTodoCount(ctx context.Context, inspectionID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_inspection_result WHERE inspection_id = ? AND state = ?`,
		inspectionID, InspStateTodo).Scan(&n)
	return n, err
}

// ===== D4 · 检测附件（库中只存路径）=====

// InspFileRow 是 b_inspection_file 的一行。
type InspFileRow struct {
	ID           int64  `json:"id"`
	InspectionID int64  `json:"inspection_id"`
	FileName     string `json:"file_name"`
	FilePath     string `json:"file_path"` // ★ 只存路径，附件不进数据库
	FileType     string `json:"file_type,omitempty"`
	FileSize     int64  `json:"file_size,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	CreatedBy    string `json:"created_by,omitempty"`
}

// AddInspectionFile 登记附件元数据（文件本体已由 httpapi 落盘到 cfg.AttachDir）。
func (s *Store) AddInspectionFile(ctx context.Context, inspectionID int64, name, path, ftype string, size int64, actor MDActor) (InspFileRow, error) {
	if _, err := s.GetInspection(ctx, inspectionID); err != nil {
		return InspFileRow{}, err
	}
	if strings.TrimSpace(path) == "" {
		return InspFileRow{}, fmt.Errorf("%w：file_path 不能为空", ErrInspBadInput)
	}
	res, err := s.db.ExecContext(ctx, `
INSERT INTO b_inspection_file (inspection_id, file_name, file_path, file_type, file_size, created_by)
VALUES (?,?,?,?,?,?)`, inspectionID, name, path, nullIfEmptyStr(ftype), size, actor.OpenID)
	if err != nil {
		return InspFileRow{}, fmt.Errorf("登记附件失败: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := s.AppendAudit(ctx, audit.Entry{
		Entity: "b_inspection_file", EntityID: id, Action: "file_add",
		NewValue:    fmt.Sprintf("inspection=%d %s → %s (%d B)", inspectionID, name, path, size),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return InspFileRow{}, err
	}
	return s.GetInspectionFile(ctx, id)
}

// GetInspectionFile 按 id 取附件行。
func (s *Store) GetInspectionFile(ctx context.Context, id int64) (InspFileRow, error) {
	var r InspFileRow
	var ftype sql.NullString
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
SELECT id, inspection_id, file_name, file_path, IFNULL(file_type, ''), file_size,
       DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s'), created_by
  FROM b_inspection_file WHERE id = ?`, id).
		Scan(&r.ID, &r.InspectionID, &r.FileName, &r.FilePath, &ftype, &size,
			&r.CreatedAt, &r.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return InspFileRow{}, fmt.Errorf("%w：附件 #%d", ErrInspNotFound, id)
	}
	if err != nil {
		return InspFileRow{}, fmt.Errorf("读取附件失败: %w", err)
	}
	r.FileType = ftype.String
	if size.Valid {
		r.FileSize = size.Int64
	}
	return r, nil
}

// ListInspectionFiles 列某单的附件。
func (s *Store) ListInspectionFiles(ctx context.Context, inspectionID int64) ([]InspFileRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, inspection_id, file_name, file_path, IFNULL(file_type, ''), IFNULL(file_size, 0),
       DATE_FORMAT(created_at, '%Y-%m-%d %H:%i:%s'), created_by
  FROM b_inspection_file WHERE inspection_id = ? ORDER BY id`, inspectionID)
	if err != nil {
		return nil, fmt.Errorf("查询附件失败: %w", err)
	}
	defer rows.Close()
	var out []InspFileRow
	for rows.Next() {
		var r InspFileRow
		var ftype sql.NullString
		if err := rows.Scan(&r.ID, &r.InspectionID, &r.FileName, &r.FilePath, &ftype,
			&r.FileSize, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, fmt.Errorf("读取附件失败: %w", err)
		}
		r.FileType = ftype.String
		out = append(out, r)
	}
	return out, rows.Err()
}
