package store

// ===== M4 取样与留样 · 库侧（D1~D6）=====
//
// ★★ 三条硬口径（MIMO-NEXT-BATCH-04 §1 / §6）：
//	① 样品由「扫码行为」自然产生，绝不预生成 —— 只有扫到的对象才有 b_sample 行；
//	② 样品编号是派生串（§6-4）：父对象码 = 父对象人读行**前 7 组**（去 SEQ3 与校验位），
//	   sample_no = 父码 + "-" + 角色码 + 2 位序号（I/C/R/A），按「父对象 × 角色」各自递增，
//	   超 99 明确报错（spec#rules.auto_carry_on_overflow = false），不自动加宽；
//	③ 保留样与检测样独立跟踪 —— 本文件及其姊妹文件是留样生命周期的**唯一**写入口，
//	   不存在任何「随检测完成清掉保留样」的路径（M5 未实现，见回执 A11）。
//
// ★ 扫码解析一律复用 internal/codec（批 3 交付）：本包只做「解析 → 定位对象 → 生成样品」，
//	不自写第二套码解析；「格式正确但系统内不存在」由 ErrCodeUnknown（404）单独表达。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// 取样/留样的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrSampleBadInput          = errors.New("取样输入不合法")
	ErrSampleNotFound          = errors.New("样品不存在")
	ErrSampleTargetUnknown     = errors.New("取样组目标对象不存在")
	ErrSampleTargetUnsupported = errors.New("该对象类型不支持取样")
	ErrTargetVoided            = errors.New("对象已作废，不得取样")
	ErrSampleSeqOverflow       = errors.New("样品序号超过 99，需人工决策（编号不自动加宽）")
	ErrSampleState             = errors.New("样品状态不允许该操作")
	ErrRetentionExists         = errors.New("该样品已登记留样")
	ErrLendState               = errors.New("借还状态不允许该操作")
	ErrDestroyNeedApprover     = errors.New("销毁必须填写审批人（approved_by）")
	ErrDestroyNotInit          = errors.New("销毁尚未发起")
	ErrDestroyAlready          = errors.New("销毁已发起或已完成")
)

// 样品角色（★ 取值即 spec/schema.sql 列注释里的中文原词）。
const (
	SampleRoleIncremental = "份样"
	SampleRoleComposite   = "大样"
	SampleRoleRetention   = "保留样"
	SampleRoleArbitration = "仲裁样"
)

// 留样/借还/销毁共用的状态枚举（b_sample.status 与 b_sample_retention.status 同步）。
const (
	SampleStatusInStock   = "在库"
	SampleStatusLent      = "已借出"
	SampleStatusDestroyed = "已销毁"
)

// 取样组目标类型（schema 列注释原词）。
const (
	TargetTruck = "车次"
	TargetBatch = "生产批"
	TargetFgLot = "成品批"
)

// 角色码（spec/code-rules.json#sample_number_rule.roles）。
var sampleRoleCode = map[string]string{
	SampleRoleIncremental: "I",
	SampleRoleComposite:   "C",
	SampleRoleRetention:   "R",
	SampleRoleArbitration: "A",
}

// sampleNoSeqMax 是 2 位序号的上限（auto_carry_on_overflow=false ⇒ 超限报错由人决策）。
const sampleNoSeqMax = 99

// Sample 是 b_sample 的一行。
type Sample struct {
	ID           int64      `json:"id"`
	SampleNo     string     `json:"sample_no"`
	Role         string     `json:"role"`
	GroupID      *int64     `json:"group_id,omitempty"`
	BagID        *int64     `json:"bag_id,omitempty"`
	BatchID      *int64     `json:"batch_id,omitempty"`
	FgLotID      *int64     `json:"fg_lot_id,omitempty"`
	SampledAt    *time.Time `json:"sampled_at,omitempty"`
	SampledBy    string     `json:"sampled_by,omitempty"`
	SampleWeight *float64   `json:"sample_weight,omitempty"`
	Status       string     `json:"status"`
	Remark       string     `json:"remark,omitempty"`
	CreatedBy    string     `json:"created_by,omitempty"`
}

// TakeResult 是一次扫码取样的结果（1 份样 + 1 保留样）。
type TakeResult struct {
	Parent  string   `json:"parent"` // 派生串的父码（人读行前 7 组）
	Code    string   `json:"code"`   // 被扫的 27 位全码
	Human   string   `json:"human"`
	Samples []Sample `json:"samples"`
}

const sampleSelect = `
SELECT id, sample_no, role, group_id, bag_id, batch_id, fg_lot_id,
       sampled_at, sampled_by, sample_weight, status, IFNULL(remark,''), created_by
  FROM b_sample`

func scanSample(row interface{ Scan(...interface{}) error }) (Sample, error) {
	var s Sample
	var groupID, bagID, batchID, fgLotID sql.NullInt64
	var sampledAt sql.NullTime
	var weight sql.NullFloat64
	var sampledBy, remark, createdBy sql.NullString
	err := row.Scan(&s.ID, &s.SampleNo, &s.Role, &groupID, &bagID, &batchID, &fgLotID,
		&sampledAt, &sampledBy, &weight, &s.Status, &remark, &createdBy)
	if err != nil {
		return Sample{}, err
	}
	assign := func(dst **int64, v sql.NullInt64) {
		if v.Valid {
			x := v.Int64
			*dst = &x
		}
	}
	assign(&s.GroupID, groupID)
	assign(&s.BagID, bagID)
	assign(&s.BatchID, batchID)
	assign(&s.FgLotID, fgLotID)
	if sampledAt.Valid {
		t := sampledAt.Time
		s.SampledAt = &t
	}
	if weight.Valid {
		w := weight.Float64
		s.SampleWeight = &w
	}
	s.SampledBy = sampledBy.String
	s.Remark = remark.String
	s.CreatedBy = createdBy.String
	return s, nil
}

// parentOfHuman 取人读行的**前 7 组**作为派生串父码（§6-4：去 SEQ3 组与校验位组）。
func parentOfHuman(human string) (string, error) {
	parts := strings.Split(human, "-")
	if len(parts) < 7 {
		return "", fmt.Errorf("%w：人读行组数 %d < 7", ErrSampleBadInput, len(parts))
	}
	return strings.Join(parts[:7], "-"), nil
}

// nextSampleSeq 取「父对象 × 角色」的下一个序号（★ 必须在持命名锁的事务内调用）。
func nextSampleSeq(ctx context.Context, tx *sql.Tx, parent, roleCode string) (int, error) {
	// LIKE 模式只在末两位放通配：父码由 [0-9A-Z-] 组成，不含 % 或 _，不会误伤。
	rows, err := tx.QueryContext(ctx,
		`SELECT sample_no FROM b_sample WHERE sample_no LIKE ?`, parent+"-"+roleCode+"__")
	if err != nil {
		return 0, fmt.Errorf("读取样品序号失败: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return 0, fmt.Errorf("读取样品序号失败: %w", err)
		}
		if len(no) < 2 {
			continue
		}
		n, err := strconv.Atoi(no[len(no)-2:])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	next := max + 1
	if next > sampleNoSeqMax {
		return 0, ErrSampleSeqOverflow
	}
	return next, nil
}

// sampleSeqLockName 生成样品编号空间的命名锁名（≤64 字符）。
func sampleSeqLockName(parent, roleCode string) string {
	name := "jxlab.sample." + parent + "." + roleCode
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// ===== D1 / D2 · 扫码取样 =====

// TakeInput 是一次扫码取样的入参。
type TakeInput struct {
	Code string `json:"code"`
}

// TakeSample 扫码取样：解析 → 定位对象 → 生成 1 份样 + 1 保留样（★ 绝不预生成）。
//
// 三种码错误与批 3 一致：ErrNotOurs / ErrChecksum / ErrBadInput（400）；
// 格式正确但系统内不存在 ⇒ ErrCodeUnknown（404）；
// 已作废对象 ⇒ ErrTargetVoided（409）。
func (s *Store) TakeSample(ctx context.Context, in TakeInput, actor MDActor) (TakeResult, error) {
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	p, err := codec.Parse(code)
	if err != nil {
		return TakeResult{}, err // codec 三类错误，httpapi 映射 400
	}
	parent, err := parentOfHuman(p.Human)
	if err != nil {
		return TakeResult{}, err
	}

	var bagID, batchID, fgLotID sql.NullInt64
	switch p.Seg.T {
	case "B":
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT id, status FROM b_bag WHERE code = ?`, code).
			Scan(&bagID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return TakeResult{}, fmt.Errorf("%w：%s", ErrCodeUnknown, code)
		}
		if err != nil {
			return TakeResult{}, fmt.Errorf("定位吨袋失败: %w", err)
		}
		if status == BagStatusVoid {
			return TakeResult{}, fmt.Errorf("%w：吨袋 %s 已作废", ErrTargetVoided, code)
		}
	case "C":
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT id, status FROM b_production_batch WHERE code = ?`, code).
			Scan(&batchID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return TakeResult{}, fmt.Errorf("%w：%s", ErrCodeUnknown, code)
		}
		if err != nil {
			return TakeResult{}, fmt.Errorf("定位生产批失败: %w", err)
		}
		if status == "已作废" {
			return TakeResult{}, fmt.Errorf("%w：生产批 %s 已作废", ErrTargetVoided, code)
		}
	case "D":
		var status string
		err := s.db.QueryRowContext(ctx, `SELECT id, status FROM b_fg_lot WHERE code = ?`, code).
			Scan(&fgLotID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return TakeResult{}, fmt.Errorf("%w：%s", ErrCodeUnknown, code)
		}
		if err != nil {
			return TakeResult{}, fmt.Errorf("定位成品批失败: %w", err)
		}
		if status == "已作废" {
			return TakeResult{}, fmt.Errorf("%w：成品批 %s 已作废", ErrTargetVoided, code)
		}
	default:
		return TakeResult{}, fmt.Errorf("%w：%s", ErrSampleTargetUnsupported, p.Seg.T)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return TakeResult{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	// 编号递增必须串行：命名锁（对空空间同样有效）+ 同事务读取（spec#rules.concurrent_alloc）。
	lock := sampleSeqLockName(parent, "IR")
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return TakeResult{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return TakeResult{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	now := time.Now()
	out := TakeResult{Parent: parent, Code: code, Human: p.Human}
	for _, role := range []string{SampleRoleIncremental, SampleRoleRetention} {
		seq, err := nextSampleSeq(ctx, tx, parent, sampleRoleCode[role])
		if err != nil {
			return TakeResult{}, err
		}
		sampleNo := fmt.Sprintf("%s-%s%02d", parent, sampleRoleCode[role], seq)
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_sample
  (sample_no, role, bag_id, batch_id, fg_lot_id, sampled_at, sampled_by, status, created_by)
VALUES (?,?,?,?,?,?,?,'在库',?)`,
			sampleNo, role, nullInt64(bagID), nullInt64(batchID), nullInt64(fgLotID),
			now, actor.Name, actor.OpenID)
		if err != nil {
			if isDuplicateErr(err) {
				return TakeResult{}, fmt.Errorf("%w：样品编号冲突 %s（请重扫）", ErrSampleBadInput, sampleNo)
			}
			return TakeResult{}, fmt.Errorf("生成样品失败: %w", err)
		}
		id, _ := res.LastInsertId()
		row := Sample{ID: id, SampleNo: sampleNo, Role: role,
			SampledAt: &now, SampledBy: actor.Name, Status: SampleStatusInStock, CreatedBy: actor.OpenID}
		row.BagID = nullToPtr(bagID)
		row.BatchID = nullToPtr(batchID)
		row.FgLotID = nullToPtr(fgLotID)
		out.Samples = append(out.Samples, row)

		if err := appendAuditTx(ctx, tx, audit.Entry{
			Entity: "b_sample", EntityID: id, Action: "take",
			NewValue:    sampleNo + " " + role + " ← " + code,
			ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		}); err != nil {
			return TakeResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return TakeResult{}, fmt.Errorf("提交失败: %w", err)
	}
	return out, nil
}

func nullInt64(v sql.NullInt64) interface{} {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

func nullToPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

// ===== D3 · 取样组（大样）=====

// GroupInput 是建组并入份样的入参。
type GroupInput struct {
	TargetType string  `json:"target_type"` // 车次 / 生产批 / 成品批
	TargetID   int64   `json:"target_id"`
	SampleIDs  []int64 `json:"sample_ids"` // 要并入的份样
	Remark     string  `json:"remark"`
}

// SampleGroup 是 b_sample_group 的一行。
type SampleGroup struct {
	ID          int64      `json:"id"`
	GroupNo     string     `json:"group_no"` // = 对应大样的 sample_no（§6-5）
	TargetType  string     `json:"target_type"`
	TargetID    int64      `json:"target_id"`
	SampleCount int        `json:"sample_count"`
	SampledAt   *time.Time `json:"sampled_at,omitempty"`
	SampledBy   string     `json:"sampled_by,omitempty"`
	Remark      string     `json:"remark,omitempty"`
	Composite   *Sample    `json:"composite,omitempty"` // role=大样 的那行
}

// lookupTargetCode 按目标类型/行 id 取对象码（组的大样以它为派生父码）。
func (s *Store) lookupTargetCode(ctx context.Context, targetType string, targetID int64) (string, error) {
	var table string
	switch targetType {
	case TargetTruck:
		table = "b_truck_lot"
	case TargetBatch:
		table = "b_production_batch"
	case TargetFgLot:
		table = "b_fg_lot"
	default:
		return "", fmt.Errorf("%w：target_type 必须是 车次/生产批/成品批，实际 %q", ErrSampleBadInput, targetType)
	}
	var code string
	err := s.db.QueryRowContext(ctx,
		`SELECT code FROM `+table+` WHERE id = ?`, targetID).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w：%s #%d", ErrSampleTargetUnknown, targetType, targetID)
	}
	if err != nil {
		return "", fmt.Errorf("查询取样组目标对象失败: %w", err)
	}
	return code, nil
}

// CreateSampleGroup 建取样组 + 生成大样（b_sample role=大样）+ 把份样并入（group_id 回填）。
//
// ★ group_no 与大样 sample_no 同值（一个对象一个编号口径，任务包 §6-5）。
// ★ 检测挂大样不挂袋（docs/01 §5.4）—— 本批只把大样建出来，检测在批 5。
func (s *Store) CreateSampleGroup(ctx context.Context, in GroupInput, actor MDActor) (SampleGroup, error) {
	if len(in.SampleIDs) > 0 && len(in.SampleIDs) > 200 {
		return SampleGroup{}, fmt.Errorf("%w：一次最多并入 200 个份样", ErrSampleBadInput)
	}
	code, err := s.lookupTargetCode(ctx, in.TargetType, in.TargetID)
	if err != nil {
		return SampleGroup{}, err
	}
	p, err := codec.Parse(code)
	if err != nil {
		return SampleGroup{}, fmt.Errorf("目标对象码非法: %w", err)
	}
	parent, err := parentOfHuman(p.Human)
	if err != nil {
		return SampleGroup{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return SampleGroup{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := sampleSeqLockName(parent, sampleRoleCode[SampleRoleComposite])
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return SampleGroup{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return SampleGroup{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// ① 校验并锁住成员份样（必须是份样、未并入过、与目标对象同源）
	for _, id := range in.SampleIDs {
		var role string
		var groupID, bagID, batchID, fgLotID sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT role, group_id, bag_id, batch_id, fg_lot_id FROM b_sample WHERE id = ? FOR UPDATE`, id).
			Scan(&role, &groupID, &bagID, &batchID, &fgLotID)
		if errors.Is(err, sql.ErrNoRows) {
			return SampleGroup{}, fmt.Errorf("%w：份样 #%d", ErrSampleNotFound, id)
		}
		if err != nil {
			return SampleGroup{}, fmt.Errorf("读取份样失败: %w", err)
		}
		if role != SampleRoleIncremental {
			return SampleGroup{}, fmt.Errorf("%w：只有份样可以并入取样组（#%d 是%s）", ErrSampleBadInput, id, role)
		}
		if groupID.Valid {
			return SampleGroup{}, fmt.Errorf("%w：份样 #%d 已在取样组 #%d 中", ErrSampleState, id, groupID.Int64)
		}
		if err := s.matchMemberTarget(ctx, tx, in.TargetType, in.TargetID, bagID, batchID, fgLotID); err != nil {
			return SampleGroup{}, err
		}
	}

	// ② 大样编号（父对象 × 角色 C 独立递增）
	seq, err := nextSampleSeq(ctx, tx, parent, sampleRoleCode[SampleRoleComposite])
	if err != nil {
		return SampleGroup{}, err
	}
	compositeNo := fmt.Sprintf("%s-%s%02d", parent, sampleRoleCode[SampleRoleComposite], seq)

	// ③ 插组（group_no = 大样 sample_no）
	now := time.Now()
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_sample_group
  (group_no, target_type, target_id, sample_count, sampled_at, sampled_by, remark, created_by)
VALUES (?,?,?,?,?,?,?,?)`,
		compositeNo, in.TargetType, in.TargetID, len(in.SampleIDs), now, actor.Name,
		nullIfEmptyStr(in.Remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return SampleGroup{}, fmt.Errorf("%w：组号冲突 %s", ErrSampleBadInput, compositeNo)
		}
		return SampleGroup{}, fmt.Errorf("创建取样组失败: %w", err)
	}
	groupID, _ := res.LastInsertId()

	// ④ 插大样行（role=大样，group_id 指向本组）
	var bat, fg sql.NullInt64
	switch in.TargetType {
	case TargetBatch:
		bat = sql.NullInt64{Int64: in.TargetID, Valid: true}
	case TargetFgLot:
		fg = sql.NullInt64{Int64: in.TargetID, Valid: true}
	}
	res, err = tx.ExecContext(ctx, `
INSERT INTO b_sample
  (sample_no, role, group_id, batch_id, fg_lot_id, sampled_at, sampled_by, status, created_by)
VALUES (?,?,?,?,?,?,?,'在库',?)`,
		compositeNo, SampleRoleComposite, groupID, nullInt64(bat), nullInt64(fg),
		now, actor.Name, actor.OpenID)
	if err != nil {
		return SampleGroup{}, fmt.Errorf("生成大样失败: %w", err)
	}
	compositeID, _ := res.LastInsertId()

	// ⑤ 份样回填 group_id
	for _, id := range in.SampleIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE b_sample SET group_id = ? WHERE id = ?`, groupID, id); err != nil {
			return SampleGroup{}, fmt.Errorf("并入份样失败: %w", err)
		}
	}

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_sample_group", EntityID: groupID, Action: "create",
		NewValue: fmt.Sprintf("%s %s#%d 份样%d 大样=%s", compositeNo, in.TargetType, in.TargetID,
			len(in.SampleIDs), compositeNo),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: in.Remark,
	}); err != nil {
		return SampleGroup{}, err
	}
	if err := tx.Commit(); err != nil {
		return SampleGroup{}, fmt.Errorf("提交失败: %w", err)
	}

	grp, err := s.GetSampleGroup(ctx, groupID)
	if err != nil {
		return SampleGroup{}, err
	}
	comp, err := s.GetSample(ctx, compositeID)
	if err == nil {
		grp.Composite = &comp
	}
	return grp, nil
}

// matchMemberTarget 校验份样与取样组目标同源（绑列按类型，docs/01 §5.4）。
func (s *Store) matchMemberTarget(ctx context.Context, tx *sql.Tx, targetType string, targetID int64,
	bagID, batchID, fgLotID sql.NullInt64) error {
	switch targetType {
	case TargetTruck:
		if !bagID.Valid {
			return fmt.Errorf("%w：份样未绑吨袋，不能并入车次取样组", ErrSampleBadInput)
		}
		var truckID int64
		if err := tx.QueryRowContext(ctx, `SELECT truck_lot_id FROM b_bag WHERE id = ?`, bagID.Int64).
			Scan(&truckID); err != nil {
			return fmt.Errorf("读取吨袋归属失败: %w", err)
		}
		if truckID != targetID {
			return fmt.Errorf("%w：份样属于车次 #%d，目标是 #%d", ErrSampleBadInput, truckID, targetID)
		}
	case TargetBatch:
		if !batchID.Valid || batchID.Int64 != targetID {
			return fmt.Errorf("%w：份样未绑该生产批", ErrSampleBadInput)
		}
	case TargetFgLot:
		if !fgLotID.Valid || fgLotID.Int64 != targetID {
			return fmt.Errorf("%w：份样未绑该成品批", ErrSampleBadInput)
		}
	}
	return nil
}

// ===== 查询 =====

// SampleFilter 是样品列表的过滤条件。
type SampleFilter struct {
	Role       string
	GroupID    *int64 // 非 nil 时按组过滤（-1 表示未并入）
	TargetType string // 与 TargetID 成对：按目标对象（车次/生产批/成品批）过滤份样
	TargetID   int64
	Limit      int
}

// ListSamples 按过滤条件列样品（默认最近 200 条）。
func (s *Store) ListSamples(ctx context.Context, f SampleFilter) ([]Sample, error) {
	where := []string{"1=1"}
	args := []interface{}{}
	if f.Role != "" {
		where = append(where, "role = ?")
		args = append(args, f.Role)
	}
	switch {
	case f.GroupID == nil:
	case *f.GroupID < 0:
		where = append(where, "group_id IS NULL")
	default:
		where = append(where, "group_id = ?")
		args = append(args, *f.GroupID)
	}
	if f.TargetType != "" {
		switch f.TargetType {
		case TargetTruck:
			where = append(where, "bag_id IN (SELECT id FROM b_bag WHERE truck_lot_id = ?)")
			args = append(args, f.TargetID)
		case TargetBatch:
			where = append(where, "batch_id = ?")
			args = append(args, f.TargetID)
		case TargetFgLot:
			where = append(where, "fg_lot_id = ?")
			args = append(args, f.TargetID)
		default:
			return nil, fmt.Errorf("%w：target_type 必须是 车次/生产批/成品批", ErrSampleBadInput)
		}
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := sampleSelect + " WHERE " + strings.Join(where, " AND ") + " ORDER BY id DESC LIMIT " + strconv.Itoa(limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询样品失败: %w", err)
	}
	defer rows.Close()
	return scanSamples(rows)
}

func scanSamples(rows *sql.Rows) ([]Sample, error) {
	var out []Sample
	for rows.Next() {
		s, err := scanSample(rows)
		if err != nil {
			return nil, fmt.Errorf("读取样品失败: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSample 按 id 取样品。
func (s *Store) GetSample(ctx context.Context, id int64) (Sample, error) {
	row, err := scanSample(s.db.QueryRowContext(ctx, sampleSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Sample{}, fmt.Errorf("%w：#%d", ErrSampleNotFound, id)
	}
	if err != nil {
		return Sample{}, fmt.Errorf("读取样品失败: %w", err)
	}
	return row, nil
}

// GetSampleGroup 按 id 取取样组。
func (s *Store) GetSampleGroup(ctx context.Context, id int64) (SampleGroup, error) {
	var g SampleGroup
	var sampledAt sql.NullTime
	var sampledBy, remark sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, group_no, target_type, target_id, sample_count, sampled_at,
       IFNULL(sampled_by,''), IFNULL(remark,'')
  FROM b_sample_group WHERE id = ?`, id).
		Scan(&g.ID, &g.GroupNo, &g.TargetType, &g.TargetID, &g.SampleCount,
			&sampledAt, &sampledBy, &remark)
	if errors.Is(err, sql.ErrNoRows) {
		return SampleGroup{}, fmt.Errorf("%w：取样组 #%d", ErrSampleNotFound, id)
	}
	if err != nil {
		return SampleGroup{}, fmt.Errorf("读取取样组失败: %w", err)
	}
	if sampledAt.Valid {
		t := sampledAt.Time
		g.SampledAt = &t
	}
	g.SampledBy = sampledBy.String
	g.Remark = remark.String
	return g, nil
}

// ListSampleGroups 列取样组（可按目标过滤）。
func (s *Store) ListSampleGroups(ctx context.Context, targetType string, targetID int64) ([]SampleGroup, error) {
	q := `SELECT id, group_no, target_type, target_id, sample_count, sampled_at,
       IFNULL(sampled_by,''), IFNULL(remark,'')
  FROM b_sample_group`
	args := []interface{}{}
	if targetType != "" {
		q += ` WHERE target_type = ?`
		args = append(args, targetType)
		if targetID > 0 {
			q += ` AND target_id = ?`
			args = append(args, targetID)
		}
	}
	q += ` ORDER BY id DESC LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询取样组失败: %w", err)
	}
	defer rows.Close()
	var out []SampleGroup
	for rows.Next() {
		var g SampleGroup
		var sampledAt sql.NullTime
		var sampledBy, remark sql.NullString
		if err := rows.Scan(&g.ID, &g.GroupNo, &g.TargetType, &g.TargetID, &g.SampleCount,
			&sampledAt, &sampledBy, &remark); err != nil {
			return nil, fmt.Errorf("读取取样组失败: %w", err)
		}
		if sampledAt.Valid {
			t := sampledAt.Time
			g.SampledAt = &t
		}
		g.SampledBy = sampledBy.String
		g.Remark = remark.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListGroupMembers 返回取样组的成员份样（大样行另取）。
func (s *Store) ListGroupMembers(ctx context.Context, groupID int64) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx,
		sampleSelect+` WHERE group_id = ? AND role = ? ORDER BY id`, groupID, SampleRoleIncremental)
	if err != nil {
		return nil, fmt.Errorf("查询组成员失败: %w", err)
	}
	defer rows.Close()
	return scanSamples(rows)
}

// SampleTarget 是取样组目标对象的下拉行（★ 只取有效对象）。
type SampleTarget struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Human  string `json:"human"`
	Status string `json:"status"`
	Extra  string `json:"extra"` // 车次=到货日 / 生产批=生产日 / 成品批=来源批
}

// ListSampleTargets 列可建取样组的目标对象（排除作废/已退货 —— 主数据口径的「启用项」）。
func (s *Store) ListSampleTargets(ctx context.Context, targetType string) ([]SampleTarget, error) {
	var q string
	switch targetType {
	case TargetTruck:
		q = `SELECT id, code, status, DATE_FORMAT(arrive_date, '%Y-%m-%d')
		   FROM b_truck_lot WHERE status NOT IN ('作废','已作废','已退货') ORDER BY id DESC LIMIT 200`
	case TargetBatch:
		q = `SELECT id, code, status, DATE_FORMAT(batch_date, '%Y-%m-%d')
		   FROM b_production_batch WHERE status <> '已作废' ORDER BY id DESC LIMIT 200`
	case TargetFgLot:
		q = `SELECT id, code, status, IFNULL(CAST(batch_id AS CHAR), '')
		   FROM b_fg_lot WHERE status <> '已作废' ORDER BY id DESC LIMIT 200`
	default:
		return nil, fmt.Errorf("%w：target_type 必须是 车次/生产批/成品批", ErrSampleBadInput)
	}
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询取样目标失败: %w", err)
	}
	defer rows.Close()
	var out []SampleTarget
	for rows.Next() {
		var t SampleTarget
		if err := rows.Scan(&t.ID, &t.Code, &t.Status, &t.Extra); err != nil {
			return nil, fmt.Errorf("读取取样目标失败: %w", err)
		}
		if full, err := codec.Parse(t.Code); err == nil {
			t.Human = full.Human
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullIfEmptyStr(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
