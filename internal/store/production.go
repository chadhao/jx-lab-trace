package store

// ===== M6 生产与谱系 · 持久化（D1 建批 / D2 投料 / D3 作业段 / D4 成品 / D5 谱系 / D6 返工）=====
//
// ★★ 承重墙：投料扫码必须写 b_feed_record —— 谱系是**多对多**，绝不编进码
//	（一车可拆多批、多车可并入一批；任务包 §1-1 / §6-13）。
// ★ 码生成一律复用 internal/codec（不得另写）；BT 段固定 CG（§6-5：b_production_batch
//	无 biz_type 列，自购链在当前 schema 下无法表达 —— 若要支持须开议题）。
// ★ 取号一律「GET_LOCK 命名锁 + 同事务取最大 +1」（§6-4 / A14）：
//	禁止“先查最大值再 +1”；超限（批序 99 / 成品批序 999 / 成品袋序 999）
//	明确报错，**不自动进位**（加宽段位会破坏定长码）。
// ★★ D/E 的序1 = **来源生产批序**、日期段 = 生产批 batch_date（链根日期，docs/02 §4-4）
//	—— 成品袋码自带完整祖先链（docs/01 D11）。★ 不得用生成当日 / 自己的流水。
// ★★ 实际产出只在 b_fg_lot 记一次（D23）：b_production_batch 只留计划产出物料，
//	本文件不存在任何向 b_production_batch 写产出数据的语句。
// ★ 「未出结论不得投料」的判定对象是**袋所属车次的现行检测单**（复用 M5
//	currentInspectionIDTx）**或**该车次**生效的**紧急放行（s_audit_log 上
//	init + approve 两笔齐且两人不同 —— 只有 init 一笔 ⇒ 不生效）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// M6 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrProdNotFound     = errors.New("对象不存在")
	ErrProdBadInput     = errors.New("输入不合法")
	ErrProdState        = errors.New("当前状态不允许该操作")
	ErrProdSeqBusy      = errors.New("批号取号繁忙，请重试")
	ErrBatchSeqOverflow = errors.New("当日该客户该成品物料的批序已达上限 99，请人工决策（超限不自动进位）")
	ErrFgSeqOverflow    = errors.New("该生产批的成品批序已达上限 999，请人工决策（超限不自动进位）")
	ErrFgBagSeqOverflow = errors.New("该成品批的成品袋序已达上限 999，请人工决策（超限不自动进位）")
	ErrFeedNotReleased  = errors.New("该车尚未出结论，不得投料")
	ErrFeedDup          = errors.New("该袋已投料，一袋只投一次")
	ErrFeedReason       = errors.New("投料记录更正 / 删除必须填写原因")
	ErrFgMismatch       = errors.New("成品批的客户 / 物料必须与来源生产批一致")
	ErrReworkReason     = errors.New("返工必须填写原因")
	ErrReworkSrcState   = errors.New("原批当前状态不允许返工")
	ErrTruckFed         = errors.New("该车已有投料，不能退车")
)

// 状态字面量（★ 逐字取自 spec/schema.sql 列注释的中文原词，§6-14）。
const (
	BatchStatusRunning = "进行中"
	BatchStatusDone    = "已完成"
	BatchStatusVoid    = "已作废"

	FgLotStatusInStock = "在库"
	FgBagStatusInStock = "在库"
	BagStatusFed       = "已投料"

	// BatchSeqMax 批序上限（spec#sequence_spaces.批序 max=99）。
	BatchSeqMax = 99
	// FgSeqMax 成品批序上限（max=999）。
	FgSeqMax = 999
	// FgBagSeqMax 成品袋序上限（max=999）。
	FgBagSeqMax = 999
	// OpSeqMax 作业段序号上限（b_batch_operation.seq 为 SMALLINT UNSIGNED）。
	OpSeqMax = 65535
)

// ===== 读模型 =====

// ProductionBatch 是生产批的读模型（★ 不含任何产出字段 —— D23）。
type ProductionBatch struct {
	ID                      int64      `json:"id"`
	Code                    string     `json:"code"`
	Human                   string     `json:"human"`
	CustomerID              int64      `json:"customer_id"`
	InputMaterialID         int64      `json:"input_material_id"`
	PlannedOutputMaterialID int64      `json:"planned_output_material_id"`
	BatchDate               string     `json:"batch_date"`
	Status                  string     `json:"status"`
	Remark                  string     `json:"remark"`
	CreatedAt               *time.Time `json:"created_at"`
	CreatedBy               string     `json:"created_by"`
}

// FeedRecord 是投料记录（★ 谱系承重墙的行）。
type FeedRecord struct {
	ID         int64      `json:"id"`
	BatchID    int64      `json:"batch_id"`
	BatchCode  string     `json:"batch_code,omitempty"`
	BagID      int64      `json:"bag_id"`
	BagCode    string     `json:"bag_code"`
	BagHuman   string     `json:"bag_human"`
	TruckID    int64      `json:"truck_id"`
	TruckCode  string     `json:"truck_code"`
	CustomerID int64      `json:"customer_id"`
	MaterialID int64      `json:"material_id"`
	FeedWeight *float64   `json:"feed_weight"`
	FedAt      *time.Time `json:"fed_at"`
	Operator   string     `json:"operator"`
	Remark     string     `json:"remark"`
	CreatedAt  *time.Time `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
}

// BatchOperation 是作业段的读模型。
type BatchOperation struct {
	ID           int64      `json:"id"`
	BatchID      int64      `json:"batch_id"`
	Seq          int        `json:"seq"`
	TeamID       *int64     `json:"team_id"`
	Operator     string     `json:"operator"`
	StartAt      *time.Time `json:"start_at"`
	EndAt        *time.Time `json:"end_at"`
	OutputWeight *float64   `json:"output_weight"`
	Remark       string     `json:"remark"`
	CreatedAt    *time.Time `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
}

// FgLot 是成品批的读模型（★ 实际产出的唯一记录点，D23）。
type FgLot struct {
	ID               int64      `json:"id"`
	Code             string     `json:"code"`
	Human            string     `json:"human"`
	BatchID          int64      `json:"batch_id"`
	CustomerID       int64      `json:"customer_id"`
	OutputMaterialID int64      `json:"output_material_id"`
	PackSpec         string     `json:"pack_spec"`
	QtyBag           int        `json:"qty_bag"`
	NetWeight        *float64   `json:"net_weight"`
	ProducedAt       *time.Time `json:"produced_at"`
	Status           string     `json:"status"`
	Remark           string     `json:"remark"`
	CreatedAt        *time.Time `json:"created_at"`
	CreatedBy        string     `json:"created_by"`
}

// FgBag 是成品袋的读模型。
type FgBag struct {
	ID                int64      `json:"id"`
	Code              string     `json:"code"`
	Human             string     `json:"human"`
	FgLotID           int64      `json:"fg_lot_id"`
	BagSeq            int        `json:"bag_seq"`
	WeightAllocated   *float64   `json:"weight_allocated"`
	WeightIsAllocated int        `json:"weight_is_allocated"`
	Status            string     `json:"status"`
	CreatedAt         *time.Time `json:"created_at"`
	CreatedBy         string     `json:"created_by"`
}

// ReworkRow 是返工关联的读模型。
type ReworkRow struct {
	ID           int64      `json:"id"`
	NewBatchID   int64      `json:"new_batch_id"`
	NewBatchCode string     `json:"new_batch_code"`
	SrcBatchID   int64      `json:"src_batch_id"`
	SrcBatchCode string     `json:"src_batch_code"`
	Reason       string     `json:"reason"`
	CreatedAt    *time.Time `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
}

// GenealogyBag 是反向谱系的读模型：一个袋 → 多条投料 → 多个生产批（多对多）。
type GenealogyBag struct {
	BagID      int64        `json:"bag_id"`
	BagCode    string       `json:"bag_code"`
	BagHuman   string       `json:"bag_human"`
	Status     string       `json:"status"`
	TruckID    int64        `json:"truck_id"`
	TruckCode  string       `json:"truck_code"`
	CustomerID int64        `json:"customer_id"`
	MaterialID int64        `json:"material_id"`
	Feeds      []FeedRecord `json:"feeds"`
}

// ===== 入参 =====

// CreateBatchInput 是建生产批的入参。
type CreateBatchInput struct {
	CustomerID              int64  `json:"customer_id"`
	InputMaterialID         int64  `json:"input_material_id"`
	PlannedOutputMaterialID int64  `json:"planned_output_material_id"`
	BatchDate               string `json:"batch_date"` // 空 ⇒ 服务端当日（链根日期）
	Remark                  string `json:"remark"`
}

// FeedInput 是投料扫码的入参。
type FeedInput struct {
	BagCode    string `json:"bag_code"`
	FeedWeight *tons  `json:"feed_weight"` // 可空
	FedAt      string `json:"fed_at"`      // 可空：YYYY-MM-DD HH:MM:SS
	Operator   string `json:"operator"`
	Remark     string `json:"remark"`
}

// FeedCorrectInput 是投料更正的入参（★ 只有这两个可变量，§6-15）。
type FeedCorrectInput struct {
	FeedWeight *tons   `json:"feed_weight"`
	Remark     *string `json:"remark"`
	Reason     string  `json:"reason"`
}

// OperationInput 是作业段入参（output_weight = **本作业段**产出，非整批产出）。
type OperationInput struct {
	TeamID       *int64 `json:"team_id"`
	Operator     string `json:"operator"`
	StartAt      string `json:"start_at"`
	EndAt        string `json:"end_at"`
	OutputWeight *tons  `json:"output_weight"`
	Remark       string `json:"remark"`
}

// CreateFgLotInput 是生成成品批的入参。
type CreateFgLotInput struct {
	OutputMaterialID int64  `json:"output_material_id"` // 空 ⇒ 继承生产批计划产出物料
	PackSpec         string `json:"pack_spec"`
	QtyBag           *int   `json:"qty_bag"`
	NetWeight        *tons  `json:"net_weight"`
	ProducedAt       string `json:"produced_at"`
	Remark           string `json:"remark"`
}

// FgPrintInput 是成品袋打印 / 补打入参（沿用 M3 口径：补打必填原因）。
type FgPrintInput struct {
	BagSeqs []int  `json:"bag_seqs"` // 空 ⇒ 全部
	Reprint bool   `json:"is_reprint"`
	Reason  string `json:"reason"`
}

// ReworkInput 是返工入参（三项物料 / 客户缺省继承原批）。
type ReworkInput struct {
	SrcBatchID              int64  `json:"src_batch_id"`
	Reason                  string `json:"reason"`
	CustomerID              int64  `json:"customer_id"`
	InputMaterialID         int64  `json:"input_material_id"`
	PlannedOutputMaterialID int64  `json:"planned_output_material_id"`
}

// ===== 行扫描 =====

func scanBatch(sc interface{ Scan(...interface{}) error }) (ProductionBatch, error) {
	var b ProductionBatch
	var created sql.NullTime
	var remark, human string
	err := sc.Scan(&b.ID, &b.Code, &human, &b.CustomerID, &b.InputMaterialID,
		&b.PlannedOutputMaterialID, &b.BatchDate, &b.Status, &remark, &created, &b.CreatedBy)
	if err != nil {
		return ProductionBatch{}, err
	}
	b.Human = human
	b.Remark = remark
	if created.Valid {
		t := created.Time
		b.CreatedAt = &t
	}
	return b, nil
}

const batchSelect = `
SELECT id, code, code, customer_id, input_material_id, planned_output_material_id,
       DATE_FORMAT(batch_date, '%Y-%m-%d'), status, COALESCE(remark, ''), created_at, created_by
  FROM b_production_batch`

func scanFeed(sc interface{ Scan(...interface{}) error }) (FeedRecord, error) {
	var f FeedRecord
	var bagHuman, truckCode, remark string
	var fedAt, created sql.NullTime
	var weight sql.NullFloat64
	err := sc.Scan(&f.ID, &f.BatchID, &f.BatchCode, &f.BagID, &f.BagCode, &bagHuman,
		&f.TruckID, &truckCode, &f.CustomerID, &f.MaterialID,
		&weight, &fedAt, &f.Operator, &remark, &created, &f.CreatedBy)
	if err != nil {
		return FeedRecord{}, err
	}
	f.BagHuman, f.TruckCode, f.Remark = bagHuman, truckCode, remark
	if weight.Valid {
		w := weight.Float64
		f.FeedWeight = &w
	}
	if fedAt.Valid {
		t := fedAt.Time
		f.FedAt = &t
	}
	if created.Valid {
		t := created.Time
		f.CreatedAt = &t
	}
	return f, nil
}

// feedSelect 投料行的统一查询（带袋 / 车次 / 客户 / 物料的关联展示字段）。
const feedSelect = `
SELECT f.id, f.batch_id, COALESCE(pb.code, ''), f.bag_id, b.code, COALESCE(b.code, ''),
       b.truck_lot_id, COALESCE(t.code, ''), COALESCE(t.customer_id, 0), COALESCE(t.material_id, 0),
       f.feed_weight, f.fed_at, f.operator, COALESCE(f.remark, ''), f.created_at, f.created_by
  FROM b_feed_record f
  JOIN b_bag b ON b.id = f.bag_id
  LEFT JOIN b_truck_lot t ON t.id = b.truck_lot_id
  LEFT JOIN b_production_batch pb ON pb.id = f.batch_id`

func scanOp(sc interface{ Scan(...interface{}) error }) (BatchOperation, error) {
	var o BatchOperation
	var teamID sql.NullInt64
	var startAt, endAt, created sql.NullTime
	var weight sql.NullFloat64
	var remark string
	err := sc.Scan(&o.ID, &o.BatchID, &o.Seq, &teamID, &o.Operator, &startAt, &endAt,
		&weight, &remark, &created, &o.CreatedBy)
	if err != nil {
		return BatchOperation{}, err
	}
	if teamID.Valid {
		v := teamID.Int64
		o.TeamID = &v
	}
	if startAt.Valid {
		t := startAt.Time
		o.StartAt = &t
	}
	if endAt.Valid {
		t := endAt.Time
		o.EndAt = &t
	}
	if weight.Valid {
		w := weight.Float64
		o.OutputWeight = &w
	}
	o.Remark = remark
	if created.Valid {
		t := created.Time
		o.CreatedAt = &t
	}
	return o, nil
}

func scanFgLot(sc interface{ Scan(...interface{}) error }) (FgLot, error) {
	var l FgLot
	var human, packSpec, remark string
	var netWeight sql.NullFloat64
	var producedAt, created sql.NullTime
	err := sc.Scan(&l.ID, &l.Code, &human, &l.BatchID, &l.CustomerID, &l.OutputMaterialID,
		&packSpec, &l.QtyBag, &netWeight, &producedAt, &l.Status, &remark, &created, &l.CreatedBy)
	if err != nil {
		return FgLot{}, err
	}
	l.Human, l.PackSpec, l.Remark = human, packSpec, remark
	if netWeight.Valid {
		w := netWeight.Float64
		l.NetWeight = &w
	}
	if producedAt.Valid {
		t := producedAt.Time
		l.ProducedAt = &t
	}
	if created.Valid {
		t := created.Time
		l.CreatedAt = &t
	}
	return l, nil
}

const fgLotSelect = `
SELECT id, code, COALESCE(code, ''), batch_id, customer_id, output_material_id,
       COALESCE(pack_spec, ''), qty_bag, net_weight, produced_at, status,
       COALESCE(remark, ''), created_at, created_by
  FROM b_fg_lot`

func scanFgBag(sc interface{ Scan(...interface{}) error }) (FgBag, error) {
	var b FgBag
	var human string
	var weight sql.NullFloat64
	var created sql.NullTime
	err := sc.Scan(&b.ID, &b.Code, &human, &b.FgLotID, &b.BagSeq, &weight,
		&b.WeightIsAllocated, &b.Status, &created, &b.CreatedBy)
	if err != nil {
		return FgBag{}, err
	}
	b.Human = human
	if weight.Valid {
		w := weight.Float64
		b.WeightAllocated = &w
	}
	if created.Valid {
		t := created.Time
		b.CreatedAt = &t
	}
	return b, nil
}

const fgBagSelect = `
SELECT id, code, COALESCE(code, ''), fg_lot_id, bag_seq, weight_allocated,
       weight_is_allocated, status, created_at, created_by
  FROM b_fg_bag`

// ===== 主数据校验（★ 不得复用 lookupCodesCtx —— 它硬编码 kind='原料'，§6-6）=====

// lookupMaterialCode 校验物料存在且 kind 匹配、启用、当前版本，返回 4 位编号。
func lookupMaterialCode(ctx context.Context, q queryer, materialID int64, kind string) (string, error) {
	if materialID <= 0 {
		return "", fmt.Errorf("%w：必须选择物料", ErrProdBadInput)
	}
	var code, status string
	var current int
	err := q.QueryRowContext(ctx,
		`SELECT code, status, is_current FROM m_material WHERE id = ? AND kind = ?`,
		materialID, kind).Scan(&code, &status, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w：%s物料 #%d", ErrProdNotFound, kind, materialID)
	}
	if err != nil {
		return "", fmt.Errorf("查询物料失败: %w", err)
	}
	if current != 1 || status != "启用" {
		return "", fmt.Errorf("%w：%s物料 #%d 已停用或不是当前版本", ErrProdBadInput, kind, materialID)
	}
	return code, nil
}

// lookupProdMaster 校验客户 + 投入（原料）+ 计划产出（成品），返回客户号与成品物料号。
//
// ★ C 码的物料段 = 计划产出成品物料（docs/01 D11 / §6-4），客户段 = 客户编号；
//
//	BT 恒 CG（§6-5：客供 / 受托加工，且 b_production_batch 无 biz_type 列）。
func lookupProdMaster(ctx context.Context, q queryer, customerID, inputMatID, outMatID int64) (custCode, outCode string, err error) {
	if customerID <= 0 {
		return "", "", fmt.Errorf("%w：必须选择客户（本项目为受托加工，BT 恒为 CG）", ErrProdBadInput)
	}
	var code, status string
	var current int
	if err := q.QueryRowContext(ctx,
		`SELECT code, status, is_current FROM m_customer WHERE id = ?`, customerID).
		Scan(&code, &status, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("%w：客户 #%d", ErrProdNotFound, customerID)
		}
		return "", "", fmt.Errorf("查询客户失败: %w", err)
	}
	if current != 1 || status != "启用" {
		return "", "", fmt.Errorf("%w：客户 #%d 已停用或不是当前版本", ErrProdBadInput, customerID)
	}
	if _, err := lookupMaterialCode(ctx, q, inputMatID, "原料"); err != nil {
		return "", "", err
	}
	outCode, err = lookupMaterialCode(ctx, q, outMatID, "成品")
	if err != nil {
		return "", "", err
	}
	return code, outCode, nil
}

// ===== 取号（GET_LOCK + 同事务）=====

// batchLockName 批序空间的命名锁名（客户 + 成品物料 + 创建日，≤64 字符）。
func batchLockName(customerID, outMatID int64, date string) string {
	return fmt.Sprintf("jxlab.batch.%d.%d.%s", customerID, outMatID, strings.ReplaceAll(date, "-", ""))
}

// nextBatchSeq 在**持锁的事务内**取下一个批序（前缀 LIKE + 当前读，超 99 报错）。
func nextBatchSeq(ctx context.Context, tx *sql.Tx, custCode, outCode, date string) (int, error) {
	prefix := "1C" + BizTypeCG + custCode + outCode + yymmdd(date)
	var code string
	err := tx.QueryRowContext(ctx,
		`SELECT code FROM b_production_batch WHERE code LIKE ? ORDER BY code DESC LIMIT 1 FOR UPDATE`,
		prefix+"%").Scan(&code)
	next := 1
	if err == nil {
		p, perr := codec.Parse(code)
		if perr != nil {
			return 0, fmt.Errorf("解析既有批码 %s 失败: %w", code, perr)
		}
		n, aerr := atoiFixed(p.Seg.SEQ1)
		if aerr != nil {
			return 0, fmt.Errorf("解析既有批序失败: %w", aerr)
		}
		next = n + 1
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("读取批序失败: %w", err)
	}
	if next > BatchSeqMax {
		return 0, ErrBatchSeqOverflow
	}
	return next, nil
}

func atoiFixed(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("非数字序段 %q", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// insertBatchTx 在**已持命名锁的事务内**取号 + 生成 C 码 + 落行（★ 同一事务）。
func insertBatchTx(ctx context.Context, tx *sql.Tx, custID, inMatID, outMatID int64,
	date, custCode, outCode, remark string, actor MDActor) (int64, string, error) {
	seq, err := nextBatchSeq(ctx, tx, custCode, outCode, date)
	if err != nil {
		return 0, "", err
	}
	code, err := codec.Generate(codec.Segments{
		T:        "C",
		BT:       BizTypeCG,
		Customer: custCode,
		Material: outCode,
		Date:     yymmdd(date),
		SEQ1:     pad(seq, 2),
		SEQ2:     codec.Placeholder,
		SEQ3:     codec.Placeholder,
	})
	if err != nil {
		return 0, "", fmt.Errorf("生成生产批码失败: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_production_batch
  (code, customer_id, input_material_id, planned_output_material_id, batch_date, status, remark, created_by)
VALUES (?,?,?,?,?,?,?,?)`,
		code, custID, inMatID, outMatID, date, BatchStatusRunning, nullStr(remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, "", fmt.Errorf("%w：批码 %s 已存在", ErrProdSeqBusy, code)
		}
		return 0, "", fmt.Errorf("建生产批失败: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_production_batch", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("%s 批序%s %s→%s", code, pad(seq, 2), date, BatchStatusRunning),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return 0, "", err
	}
	return id, code, nil
}

// ===== D1 · 建生产批 =====

// CreateProductionBatch 建生产批（D1）：校验主数据 → 持锁取号 → 落行 → 审计。
//
// ★ 批序 scope = 客户 + 成品物料 + **创建日**（§6-4）；batch_date 缺省 = 服务端当日，
//
//	该字段即**链根日期**（docs/02 §4-4）；status 恒建为「进行中」（§5 明确不做完成推进）。
func (s *Store) CreateProductionBatch(ctx context.Context, in CreateBatchInput, actor MDActor) (ProductionBatch, error) {
	date, err := resolveBatchDate(in.BatchDate)
	if err != nil {
		return ProductionBatch{}, err
	}
	custCode, outCode, err := lookupProdMaster(ctx, s.db, in.CustomerID, in.InputMaterialID, in.PlannedOutputMaterialID)
	if err != nil {
		return ProductionBatch{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ProductionBatch{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := batchLockName(in.CustomerID, in.PlannedOutputMaterialID, date)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return ProductionBatch{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ProductionBatch{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	id, _, err := insertBatchTx(ctx, tx, in.CustomerID, in.InputMaterialID, in.PlannedOutputMaterialID,
		date, custCode, outCode, in.Remark, actor)
	if err != nil {
		return ProductionBatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProductionBatch{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetProductionBatch(ctx, id)
}

// resolveBatchDate 解析 / 补齐链根日期（空 ⇒ 服务端当日）。
func resolveBatchDate(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Now().Format("2006-01-02"), nil
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return "", fmt.Errorf("%w：batch_date 应为 YYYY-MM-DD，实际 %q", ErrProdBadInput, v)
	}
	return t.Format("2006-01-02"), nil
}

// GetProductionBatch 读单个生产批。
func (s *Store) GetProductionBatch(ctx context.Context, id int64) (ProductionBatch, error) {
	b, err := scanBatch(s.db.QueryRowContext(ctx, batchSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProductionBatch{}, fmt.Errorf("%w：生产批 #%d", ErrProdNotFound, id)
	}
	if err != nil {
		return ProductionBatch{}, fmt.Errorf("读取生产批失败: %w", err)
	}
	return b, nil
}

// ListProductionBatches 列生产批（status 可空 ⇒ 全部；读入口 prod.batch.create/READ）。
func (s *Store) ListProductionBatches(ctx context.Context, status string) ([]ProductionBatch, error) {
	q := batchSelect
	args := []interface{}{}
	if strings.TrimSpace(status) != "" {
		q += ` WHERE status = ?`
		args = append(args, strings.TrimSpace(status))
	}
	q += ` ORDER BY id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询生产批失败: %w", err)
	}
	defer rows.Close()
	out := []ProductionBatch{}
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("读取生产批失败: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ===== D2 · 投料扫码（★★ 谱系承重墙）=====

// truckReleasedTx 判定「袋所属车次」是否可投料：
//
//	① 现行检测单（复用 M5 currentInspectionIDTx，已作废单不算）conclusion ∈ {合格, CONCESSION}；
//	② 或该车次存在**生效的**紧急放行 —— §6-12：s_audit_log 上 init + approve 两笔齐
//	   且两人不同（与 M5 GetUrgentRelease 读同一张审计表、同一组 action；只有 init ⇒ 不生效）。
func truckReleasedTx(ctx context.Context, tx *sql.Tx, truckID int64) (bool, error) {
	cur, err := currentInspectionIDTx(ctx, tx, TargetTruck, truckID)
	if err != nil {
		return false, err
	}
	if cur.Valid {
		var conclusion string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(conclusion, '') FROM b_inspection WHERE id = ?`, cur.Int64).
			Scan(&conclusion); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("读取现行检测单结论失败: %w", err)
		}
		if conclusion == InspConclusionPass || conclusion == InspConclusionCons {
			return true, nil
		}
	}
	return urgentEffectiveTx(ctx, tx, "b_truck_lot", truckID)
}

// urgentEffectiveTx 紧急放行是否**生效**（init + approve 两笔齐且两人不同）。
func urgentEffectiveTx(ctx context.Context, tx *sql.Tx, entity string, entityID int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT action, COALESCE(actor_open_id, '')
  FROM s_audit_log
 WHERE entity = ? AND entity_id = ?
   AND action IN ('urgent_release_init', 'urgent_release_approve')
 ORDER BY id ASC`, entity, entityID)
	if err != nil {
		return false, fmt.Errorf("查询紧急放行失败: %w", err)
	}
	defer rows.Close()
	var initBy, approveBy string
	for rows.Next() {
		var action, actor string
		if err := rows.Scan(&action, &actor); err != nil {
			return false, fmt.Errorf("读取紧急放行失败: %w", err)
		}
		switch action {
		case "urgent_release_init":
			if initBy == "" {
				initBy = actor
			}
		case "urgent_release_approve":
			if approveBy == "" {
				approveBy = actor
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return initBy != "" && approveBy != "" && initBy != approveBy, nil
}

// normalizeBagCode 解析吨袋码（裸串或人读行）；★ 校验位不过即拒（codec.Parse）。
func normalizeBagCode(raw string) (codec.Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return codec.Parsed{}, fmt.Errorf("%w：bag_code 不能为空", ErrProdBadInput)
	}
	full := strings.ToUpper(s)
	if strings.Contains(s, "-") {
		f, err := codec.FromHuman(s)
		if err != nil {
			return codec.Parsed{}, err
		}
		full = f
	}
	p, err := codec.Parse(full)
	if err != nil {
		return codec.Parsed{}, err
	}
	if p.Seg.T != "B" {
		return codec.Parsed{}, fmt.Errorf("%w：投料只扫原料吨袋码（B），实际 %s", ErrProdBadInput, p.Seg.T)
	}
	return p, nil
}

// FeedScan 投料扫码（D2）：三项前置缺一即拒 ⇒ INSERT b_feed_record + 同事务置袋「已投料」。
//
// ★ 一袋只投一次：袋行 FOR UPDATE + status 守卫 + b_feed_record 存在性双向检查（§6-13）；
//
//	★ 不新增唯一索引（schema 冻结）；★ 生产批已作废 ⇒ 拒绝。
func (s *Store) FeedScan(ctx context.Context, batchID int64, in FeedInput, actor MDActor) (FeedRecord, error) {
	parsed, err := normalizeBagCode(in.BagCode)
	if err != nil {
		return FeedRecord{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FeedRecord{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	batch, err := scanBatch(tx.QueryRowContext(ctx, batchSelect+` WHERE id = ? FOR UPDATE`, batchID))
	if errors.Is(err, sql.ErrNoRows) {
		return FeedRecord{}, fmt.Errorf("%w：生产批 #%d", ErrProdNotFound, batchID)
	}
	if err != nil {
		return FeedRecord{}, err
	}
	if batch.Status == BatchStatusVoid {
		return FeedRecord{}, fmt.Errorf("%w：生产批 #%d 已作废", ErrProdState, batchID)
	}

	var bagID, truckID int64
	var bagStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT id, truck_lot_id, status FROM b_bag WHERE code = ? FOR UPDATE`, parsed.Full).
		Scan(&bagID, &truckID, &bagStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return FeedRecord{}, fmt.Errorf("%w：袋码 %s", ErrCodeUnknown, parsed.Full)
	}
	if err != nil {
		return FeedRecord{}, fmt.Errorf("读取吨袋失败: %w", err)
	}
	switch bagStatus {
	case BagStatusInStock:
		// 可投
	case BagStatusFed:
		return FeedRecord{}, fmt.Errorf("%w：袋 #%d", ErrFeedDup, bagID)
	case BagStatusVoid:
		return FeedRecord{}, fmt.Errorf("%w：袋 #%d", ErrAlreadyVoid, bagID)
	default:
		return FeedRecord{}, fmt.Errorf("%w：袋 #%d 当前状态「%s」", ErrProdState, bagID, bagStatus)
	}

	var fed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_feed_record WHERE bag_id = ?`, bagID).Scan(&fed); err != nil {
		return FeedRecord{}, fmt.Errorf("检查投料记录失败: %w", err)
	}
	if fed > 0 {
		return FeedRecord{}, fmt.Errorf("%w：袋 #%d", ErrFeedDup, bagID)
	}

	// ★★ 三项前置的第 2/3 条：车次现行单出结论 **或** 生效紧急放行（§6-11）
	released, err := truckReleasedTx(ctx, tx, truckID)
	if err != nil {
		return FeedRecord{}, err
	}
	if !released {
		return FeedRecord{}, ErrFeedNotReleased
	}

	var fedAt interface{}
	if strings.TrimSpace(in.FedAt) != "" {
		t, perr := parseDateTime(in.FedAt)
		if perr != nil {
			return FeedRecord{}, perr
		}
		fedAt = t
	}
	var weight interface{}
	if in.FeedWeight != nil {
		weight = in.FeedWeight.String()
	}

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_feed_record (batch_id, bag_id, feed_weight, fed_at, operator, remark, created_by)
VALUES (?,?,?,?,?,?,?)`,
		batchID, bagID, weight, fedAt, in.Operator, nullStr(in.Remark), actor.OpenID)
	if err != nil {
		return FeedRecord{}, fmt.Errorf("写投料记录失败: %w", err)
	}
	feedID, _ := res.LastInsertId()

	if _, err := tx.ExecContext(ctx,
		`UPDATE b_bag SET status = ? WHERE id = ? AND status = ?`,
		BagStatusFed, bagID, BagStatusInStock); err != nil {
		return FeedRecord{}, fmt.Errorf("更新袋状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_feed_record", EntityID: feedID, Action: "feed",
		NewValue:    fmt.Sprintf("批 #%d 袋 #%d", batchID, bagID),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return FeedRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeedRecord{}, fmt.Errorf("提交失败: %w", err)
	}

	row, err := scanFeed(s.db.QueryRowContext(ctx, feedSelect+` WHERE f.id = ?`, feedID))
	if err != nil {
		return FeedRecord{}, fmt.Errorf("读取投料记录失败: %w", err)
	}
	return row, nil
}

// parseDateTime 解析常见时间入参（YYYY-MM-DD HH:MM:SS / RFC3339）。
func parseDateTime(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00", time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%w：时间应为 YYYY-MM-DD HH:MM:SS，实际 %q", ErrProdBadInput, v)
}

// CorrectFeed 投料更正（§6-15）：★ 只允许改 feed_weight / remark，必须填 reason，
// 写审计 action='correct'（old_value 记快照）。★ batch_id / bag_id 无修改路径。
func (s *Store) CorrectFeed(ctx context.Context, id int64, in FeedCorrectInput, actor MDActor) (FeedRecord, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return FeedRecord{}, ErrFeedReason
	}
	if in.FeedWeight == nil && in.Remark == nil {
		return FeedRecord{}, fmt.Errorf("%w：没有可更正的字段（只允许 feed_weight / remark）", ErrProdBadInput)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FeedRecord{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	before, err := scanFeed(tx.QueryRowContext(ctx, feedSelect+` WHERE f.id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return FeedRecord{}, fmt.Errorf("%w：投料记录 #%d", ErrProdNotFound, id)
	}
	if err != nil {
		return FeedRecord{}, err
	}

	newWeight := before.FeedWeight
	if in.FeedWeight != nil {
		w := in.FeedWeight.Milli()
		f := float64(w) / 1000
		newWeight = &f
	}
	weightVal := interface{}(nil)
	if newWeight != nil {
		weightVal = milliStr(int64(math.Round(*newWeight * 1000)))
	}
	remarkVal := interface{}(before.Remark)
	if in.Remark != nil {
		remarkVal = nullStr(*in.Remark)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_feed_record SET feed_weight = ?, remark = ? WHERE id = ?`,
		weightVal, remarkVal, id); err != nil {
		return FeedRecord{}, fmt.Errorf("更正投料记录失败: %w", err)
	}
	snapshot := func(f FeedRecord) string {
		w := "NULL"
		if f.FeedWeight != nil {
			w = milliStr(int64(math.Round(*f.FeedWeight * 1000)))
		}
		return fmt.Sprintf("batch_id=%d;bag_id=%d;feed_weight=%s;remark=%s",
			f.BatchID, f.BagID, w, f.Remark)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_feed_record", EntityID: id, Action: "correct",
		OldValue:    snapshot(before),
		NewValue:    fmt.Sprintf("feed_weight=%v;remark=%v", weightVal, remarkVal),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return FeedRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return FeedRecord{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetFeed(ctx, id)
}

// GetFeed 读单条投料记录。
func (s *Store) GetFeed(ctx context.Context, id int64) (FeedRecord, error) {
	f, err := scanFeed(s.db.QueryRowContext(ctx, feedSelect+` WHERE f.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return FeedRecord{}, fmt.Errorf("%w：投料记录 #%d", ErrProdNotFound, id)
	}
	if err != nil {
		return FeedRecord{}, fmt.Errorf("读取投料记录失败: %w", err)
	}
	return f, nil
}

// DeleteFeed 删除投料记录（§6-15）：物理 DELETE + 必填 reason + 审计 action='delete'
// （old_value 记整行快照）+ ★ 同事务把袋 status 回置「在库」。
func (s *Store) DeleteFeed(ctx context.Context, id int64, reason string, actor MDActor) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrFeedReason
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	before, err := scanFeed(tx.QueryRowContext(ctx, feedSelect+` WHERE f.id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w：投料记录 #%d", ErrProdNotFound, id)
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM b_feed_record WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除投料记录失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_bag SET status = ? WHERE id = ?`, BagStatusInStock, before.BagID); err != nil {
		return fmt.Errorf("回置袋状态失败: %w", err)
	}
	w := "NULL"
	if before.FeedWeight != nil {
		w = milliStr(int64(math.Round(*before.FeedWeight * 1000)))
	}
	fedAt := ""
	if before.FedAt != nil {
		fedAt = before.FedAt.Format("2006-01-02 15:04:05")
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_feed_record", EntityID: id, Action: "delete",
		OldValue: fmt.Sprintf("batch_id=%d;bag_id=%d;bag_code=%s;feed_weight=%s;fed_at=%s;operator=%s;remark=%s",
			before.BatchID, before.BagID, before.BagCode, w,
			fedAt, before.Operator, before.Remark),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// ListFeeds 列某生产批的投料明细（正向半步，D5）。
func (s *Store) ListFeeds(ctx context.Context, batchID int64) ([]FeedRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		feedSelect+` WHERE f.batch_id = ? ORDER BY f.id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("查询投料明细失败: %w", err)
	}
	defer rows.Close()
	out := []FeedRecord{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("读取投料明细失败: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GenealogyByBagCode 反向谱系（D5）：袋码 ⇒ 投料记录 ⇒ 去向的生产批（可多条 ⇒ 多对多）。
func (s *Store) GenealogyByBagCode(ctx context.Context, raw string) (GenealogyBag, error) {
	parsed, err := normalizeBagCode(raw)
	if err != nil {
		return GenealogyBag{}, err
	}
	var g GenealogyBag
	var human, status string
	err = s.db.QueryRowContext(ctx,
		`SELECT id, code, COALESCE(code, ''), status, truck_lot_id FROM b_bag WHERE code = ?`,
		parsed.Full).Scan(&g.BagID, &g.BagCode, &human, &status, &g.TruckID)
	if errors.Is(err, sql.ErrNoRows) {
		return GenealogyBag{}, fmt.Errorf("%w：袋码 %s", ErrCodeUnknown, parsed.Full)
	}
	if err != nil {
		return GenealogyBag{}, fmt.Errorf("读取吨袋失败: %w", err)
	}
	g.BagHuman, g.Status = human, status
	_ = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(code, ''), customer_id, material_id FROM b_truck_lot WHERE id = ?`,
		g.TruckID).Scan(&g.TruckCode, &g.CustomerID, &g.MaterialID)

	feeds, err := s.listFeedsByBag(ctx, g.BagID)
	if err != nil {
		return GenealogyBag{}, err
	}
	g.Feeds = feeds
	return g, nil
}

// listFeedsByBag 按袋取投料记录（★ 谱系是多对多：一袋可能有多行）。
func (s *Store) listFeedsByBag(ctx context.Context, bagID int64) ([]FeedRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		feedSelect+` WHERE f.bag_id = ? ORDER BY f.id`, bagID)
	if err != nil {
		return nil, fmt.Errorf("查询袋投料记录失败: %w", err)
	}
	defer rows.Close()
	out := []FeedRecord{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, fmt.Errorf("读取袋投料记录失败: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ===== D3 · 作业段（跨班组）=====

// AddOperation 记一段作业（D3）：seq 由服务端在同事务内取号，uk_op_seq 兜底唯一。
//
// ★ 跨班组就是多条作业段（TC-M6-06）；★ output_weight 是**本作业段**产出，
//
//	不是整批产出（spec/schema.sql:320 列注释）；★ 不校验时间段重叠、不做自动结算。
func (s *Store) AddOperation(ctx context.Context, batchID int64, in OperationInput, actor MDActor) (BatchOperation, error) {
	startAt, err := optionalDateTime(in.StartAt)
	if err != nil {
		return BatchOperation{}, err
	}
	endAt, err := optionalDateTime(in.EndAt)
	if err != nil {
		return BatchOperation{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BatchOperation{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM b_production_batch WHERE id = ?`, batchID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BatchOperation{}, fmt.Errorf("%w：生产批 #%d", ErrProdNotFound, batchID)
		}
		return BatchOperation{}, err
	}
	if status == BatchStatusVoid {
		return BatchOperation{}, fmt.Errorf("%w：生产批 #%d 已作废", ErrProdState, batchID)
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM b_batch_operation WHERE batch_id = ? FOR UPDATE`, batchID).
		Scan(&maxSeq); err != nil {
		return BatchOperation{}, fmt.Errorf("读取作业段序号失败: %w", err)
	}
	next := 1
	if maxSeq.Valid {
		next = int(maxSeq.Int64) + 1
	}
	if next > OpSeqMax {
		return BatchOperation{}, fmt.Errorf("%w：作业段序号已达上限 %d", ErrProdState, OpSeqMax)
	}

	var weight interface{}
	if in.OutputWeight != nil {
		weight = in.OutputWeight.String()
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_batch_operation (batch_id, seq, team_id, operator, start_at, end_at, output_weight, remark, created_by)
VALUES (?,?,?,?,?,?,?,?,?)`,
		batchID, next, nullIntPtr(in.TeamID), nullStr(in.Operator),
		startAt, endAt, weight, nullStr(in.Remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return BatchOperation{}, fmt.Errorf("%w：作业段序号 %d 冲突，请重试", ErrProdSeqBusy, next)
		}
		return BatchOperation{}, fmt.Errorf("记录作业段失败: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_batch_operation", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("批 #%d 段序 %d", batchID, next),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return BatchOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return BatchOperation{}, fmt.Errorf("提交失败: %w", err)
	}

	row, err := scanOp(s.db.QueryRowContext(ctx, `
SELECT id, batch_id, seq, team_id, COALESCE(operator, ''), start_at, end_at,
       output_weight, COALESCE(remark, ''), created_at, created_by
  FROM b_batch_operation WHERE id = ?`, id))
	if err != nil {
		return BatchOperation{}, fmt.Errorf("读取作业段失败: %w", err)
	}
	return row, nil
}

// ListOperations 列某批的作业段（读入口 prod.op.log/READ）。
func (s *Store) ListOperations(ctx context.Context, batchID int64) ([]BatchOperation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, batch_id, seq, team_id, COALESCE(operator, ''), start_at, end_at,
       output_weight, COALESCE(remark, ''), created_at, created_by
  FROM b_batch_operation WHERE batch_id = ? ORDER BY seq`, batchID)
	if err != nil {
		return nil, fmt.Errorf("查询作业段失败: %w", err)
	}
	defer rows.Close()
	out := []BatchOperation{}
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, fmt.Errorf("读取作业段失败: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// optionalDateTime 空串 ⇒ nil（交给 DB 默认 / NULL）。
func optionalDateTime(v string) (interface{}, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	t, err := parseDateTime(v)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func nullIntPtr(p *int64) interface{} {
	if p == nil || *p <= 0 {
		return nil
	}
	return *p
}

// ===== D4 · 成品批 / 成品袋（★ 实际产出只在 b_fg_lot，D23）=====

// CreateFgLot 生成成品批（D4）。
//
//	★★ 客户段 / 物料段必须与来源生产批一致（缺省继承；显式传入但不一致 ⇒ 拒绝，§6-7）；
//	★★ 日期段 = 来源生产批 batch_date（链根日期，§6-8）；★ 序1 = 来源生产批序（§6-9）；
//	★ 序2 = 该生产批内第 n 个成品批（1 起，max 999）。
func (s *Store) CreateFgLot(ctx context.Context, batchID int64, in CreateFgLotInput, actor MDActor) (FgLot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FgLot{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	batch, err := scanBatch(tx.QueryRowContext(ctx, batchSelect+` WHERE id = ? FOR UPDATE`, batchID))
	if errors.Is(err, sql.ErrNoRows) {
		return FgLot{}, fmt.Errorf("%w：生产批 #%d", ErrProdNotFound, batchID)
	}
	if err != nil {
		return FgLot{}, err
	}
	if batch.Status == BatchStatusVoid {
		return FgLot{}, fmt.Errorf("%w：生产批 #%d 已作废", ErrProdState, batchID)
	}

	outMatID := in.OutputMaterialID
	if outMatID == 0 {
		outMatID = batch.PlannedOutputMaterialID
	} else if outMatID != batch.PlannedOutputMaterialID {
		return FgLot{}, fmt.Errorf("%w：output_material_id=%d ≠ 来源生产批的计划产出物料 %d",
			ErrFgMismatch, outMatID, batch.PlannedOutputMaterialID)
	}
	if _, err := lookupMaterialCode(ctx, tx, outMatID, "成品"); err != nil {
		return FgLot{}, err
	}

	batchSeg, err := codec.Parse(batch.Code)
	if err != nil {
		return FgLot{}, fmt.Errorf("解析生产批码失败: %w", err)
	}
	seq1 := batchSeg.Seg.SEQ1
	// ★ 日期段 = 生产批 batch_date（链根日期），不用生成当日 / produced_at（§6-8）
	dateSeg := yymmdd(batch.BatchDate)

	seq2, err := nextFgSeq(ctx, tx, batchID)
	if err != nil {
		return FgLot{}, err
	}
	code, err := codec.Generate(codec.Segments{
		T:        "D",
		BT:       batchSeg.Seg.BT,
		Customer: batchSeg.Seg.Customer,
		Material: batchSeg.Seg.Material,
		Date:     dateSeg,
		SEQ1:     seq1,
		SEQ2:     pad(seq2, 3),
		SEQ3:     codec.Placeholder,
	})
	if err != nil {
		return FgLot{}, fmt.Errorf("生成成品批码失败: %w", err)
	}

	qty := 0
	if in.QtyBag != nil {
		qty = *in.QtyBag
	}
	var netWeight interface{}
	if in.NetWeight != nil {
		netWeight = in.NetWeight.String()
	}
	producedAt, err := optionalDateTime(in.ProducedAt)
	if err != nil {
		return FgLot{}, err
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_fg_lot
  (code, batch_id, customer_id, output_material_id, pack_spec, qty_bag, net_weight,
   produced_at, status, remark, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		code, batchID, batch.CustomerID, outMatID, nullStr(in.PackSpec), qty,
		netWeight, producedAt, FgLotStatusInStock, nullStr(in.Remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return FgLot{}, fmt.Errorf("%w：成品批码 %s 已存在", ErrProdSeqBusy, code)
		}
		return FgLot{}, fmt.Errorf("生成成品批失败: %w", err)
	}
	id, _ := res.LastInsertId()
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_fg_lot", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("%s 来源批 #%d", code, batchID),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return FgLot{}, err
	}
	if err := tx.Commit(); err != nil {
		return FgLot{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetFgLot(ctx, id)
}

// nextFgSeq 取「该生产批内第 n 个成品批」（解析既有 D 码取最大 +1，超 999 报错）。
//
// ★ 在持锁事务内执行；跳号不回收（max+1，已作废行仍计入）。
func nextFgSeq(ctx context.Context, tx *sql.Tx, batchID int64) (int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT code FROM b_fg_lot WHERE batch_id = ? FOR UPDATE`, batchID)
	if err != nil {
		return 0, fmt.Errorf("读取成品批序失败: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return 0, fmt.Errorf("读取成品批序失败: %w", err)
		}
		p, err := codec.Parse(code)
		if err != nil {
			return 0, fmt.Errorf("解析成品批码失败: %w", err)
		}
		n, err := atoiFixed(p.Seg.SEQ2)
		if err != nil {
			return 0, fmt.Errorf("解析成品批序失败: %w", err)
		}
		if n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	next := max + 1
	if next > FgSeqMax {
		return 0, ErrFgSeqOverflow
	}
	return next, nil
}

// GetFgLot 读单个成品批。
func (s *Store) GetFgLot(ctx context.Context, id int64) (FgLot, error) {
	l, err := scanFgLot(s.db.QueryRowContext(ctx, fgLotSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return FgLot{}, fmt.Errorf("%w：成品批 #%d", ErrProdNotFound, id)
	}
	if err != nil {
		return FgLot{}, fmt.Errorf("读取成品批失败: %w", err)
	}
	return l, nil
}

// ListFgLots 列某生产批的成品批（读入口 prod.fg.gen/READ）。
func (s *Store) ListFgLots(ctx context.Context, batchID int64) ([]FgLot, error) {
	rows, err := s.db.QueryContext(ctx,
		fgLotSelect+` WHERE batch_id = ? ORDER BY id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("查询成品批失败: %w", err)
	}
	defer rows.Close()
	out := []FgLot{}
	for rows.Next() {
		l, err := scanFgLot(rows)
		if err != nil {
			return nil, fmt.Errorf("读取成品批失败: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GenerateFgBags 批量生成成品袋码 E（D4）。
//
//	★★ 段位全部继承来源：序1 = 来源生产批序、序2 = 成品批序、序3 = 本成品批内第 n 袋；
//	★ bag_seq 从 1 起连续（第二次生成接着最大序号），uk_fgbag_seq 兜底，超 999 报错；
//	★ net_weight 非空 ⇒ 毫吨整数均分摊算（half-up），否则 weight_allocated 留空且置 0；
//	★ 生成后同事务把 b_fg_lot.qty_bag 更新为**实际生成袋数**（D23：产出只在 b_fg_lot）。
func (s *Store) GenerateFgBags(ctx context.Context, fgLotID int64, count int, actor MDActor) ([]FgBag, error) {
	if count <= 0 {
		return nil, fmt.Errorf("%w：袋数必须大于 0", ErrProdBadInput)
	}
	if count > FgBagSeqMax {
		return nil, fmt.Errorf("%w：袋数 %d 超过上限 %d（成品袋序定宽 3 位）", ErrProdBadInput, count, FgBagSeqMax)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	lot, err := scanFgLot(tx.QueryRowContext(ctx, fgLotSelect+` WHERE id = ? FOR UPDATE`, fgLotID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：成品批 #%d", ErrProdNotFound, fgLotID)
	}
	if err != nil {
		return nil, err
	}
	if lot.Status != FgLotStatusInStock {
		return nil, fmt.Errorf("%w：成品批 #%d 状态「%s」", ErrProdState, fgLotID, lot.Status)
	}
	seg, err := codec.Parse(lot.Code)
	if err != nil {
		return nil, fmt.Errorf("解析成品批码失败: %w", err)
	}

	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(bag_seq) FROM b_fg_bag WHERE fg_lot_id = ? FOR UPDATE`, fgLotID).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("读取成品袋序失败: %w", err)
	}
	start := 1
	if maxSeq.Valid {
		start = int(maxSeq.Int64) + 1
	}
	if start+count-1 > FgBagSeqMax {
		return nil, ErrFgBagSeqOverflow
	}

	var allocMilli int64
	hasWeight := lot.NetWeight != nil
	if hasWeight {
		netMilli := int64(math.Round(*lot.NetWeight * 1000))
		allocMilli = divMilliRound(netMilli, int64(count))
	}

	for i := 0; i < count; i++ {
		seq := start + i
		code, err := codec.Generate(codec.Segments{
			T:        "E",
			BT:       seg.Seg.BT,
			Customer: seg.Seg.Customer,
			Material: seg.Seg.Material,
			Date:     seg.Seg.Date,
			SEQ1:     seg.Seg.SEQ1,
			SEQ2:     seg.Seg.SEQ2,
			SEQ3:     pad(seq, 3),
		})
		if err != nil {
			return nil, fmt.Errorf("生成成品袋码失败: %w", err)
		}
		var weight interface{}
		allocated := 1
		if hasWeight {
			weight = milliStr(allocMilli)
		} else {
			allocated = 0
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO b_fg_bag (code, fg_lot_id, bag_seq, weight_allocated, weight_is_allocated, status, created_by)
VALUES (?,?,?,?,?,?,?)`,
			code, fgLotID, seq, weight, allocated, FgBagStatusInStock, actor.OpenID); err != nil {
			if isDuplicateErr(err) {
				return nil, fmt.Errorf("%w：成品袋码 %s 已存在", ErrProdSeqBusy, code)
			}
			return nil, fmt.Errorf("生成成品袋失败: %w", err)
		}
	}

	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_fg_bag WHERE fg_lot_id = ?`, fgLotID).Scan(&total); err != nil {
		return nil, fmt.Errorf("统计成品袋失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_fg_lot SET qty_bag = ? WHERE id = ?`, total, fgLotID); err != nil {
		return nil, fmt.Errorf("更新成品批袋数失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_fg_lot", EntityID: fgLotID, Action: "bag_gen",
		NewValue:    fmt.Sprintf("%d 个成品袋（累计 %d）", count, total),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return s.ListFgBags(ctx, fgLotID)
}

// ListFgBags 列某成品批的成品袋。
func (s *Store) ListFgBags(ctx context.Context, fgLotID int64) ([]FgBag, error) {
	rows, err := s.db.QueryContext(ctx,
		fgBagSelect+` WHERE fg_lot_id = ? ORDER BY bag_seq`, fgLotID)
	if err != nil {
		return nil, fmt.Errorf("查询成品袋失败: %w", err)
	}
	defer rows.Close()
	out := []FgBag{}
	for rows.Next() {
		b, err := scanFgBag(rows)
		if err != nil {
			return nil, fmt.Errorf("读取成品袋失败: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// PrintFgBags 成品袋打印 / 补打（D4）：★ 写 b_label_print，补打 is_reprint=1 且
// reason 必填非空（沿用 M3 口径 docs/02 §6）；一期只出标签版式数据，不接打印驱动。
func (s *Store) PrintFgBags(ctx context.Context, fgLotID int64, in FgPrintInput, actor MDActor) ([]LabelItem, error) {
	reason := strings.TrimSpace(in.Reason)
	if in.Reprint && reason == "" {
		return nil, ErrReprintReason
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	lot, err := scanFgLot(tx.QueryRowContext(ctx, fgLotSelect+` WHERE id = ? FOR UPDATE`, fgLotID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w：成品批 #%d", ErrProdNotFound, fgLotID)
	}
	if err != nil {
		return nil, err
	}
	if lot.Status == "已作废" {
		return nil, fmt.Errorf("%w：成品批 #%d 已作废", ErrAlreadyVoid, fgLotID)
	}

	q := fgBagSelect + ` WHERE fg_lot_id = ?`
	args := []interface{}{fgLotID}
	if len(in.BagSeqs) > 0 {
		ph := make([]string, 0, len(in.BagSeqs))
		for _, seq := range in.BagSeqs {
			if seq <= 0 {
				return nil, fmt.Errorf("%w：bag_seq 须为正整数", ErrProdBadInput)
			}
			ph = append(ph, "?")
			args = append(args, seq)
		}
		q += ` AND bag_seq IN (` + strings.Join(ph, ",") + `)`
	}
	q += ` ORDER BY bag_seq`
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询成品袋失败: %w", err)
	}
	bags := []FgBag{}
	for rows.Next() {
		b, err := scanFgBag(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		bags = append(bags, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(bags) == 0 {
		return nil, fmt.Errorf("%w：成品批 #%d 没有可打印的袋", ErrProdBadInput, fgLotID)
	}
	if len(in.BagSeqs) > 0 && len(bags) != len(in.BagSeqs) {
		return nil, fmt.Errorf("%w：部分 bag_seq 不存在", ErrProdNotFound)
	}

	out := make([]LabelItem, 0, len(bags))
	for _, b := range bags {
		if b.Status != FgBagStatusInStock {
			return nil, fmt.Errorf("%w：成品袋 %s 状态「%s」", ErrAlreadyVoid, b.Code, b.Status)
		}
		var printed int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM b_label_print WHERE code = ?`, b.Code).Scan(&printed); err != nil {
			return nil, fmt.Errorf("查询打印历史失败: %w", err)
		}
		effective := in.Reprint || printed > 0
		if effective && reason == "" {
			return nil, fmt.Errorf("%w：码 %s 已打印过，再次打印必须走补打并填写原因", ErrReprintReason, b.Code)
		}
		flag := 0
		if effective {
			flag = 1
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_label_print (code, printed_by, is_reprint, reason, created_by)
VALUES (?,?,?,?,?)`, b.Code, actor.Name, flag, nullStr(reason), actor.OpenID)
		if err != nil {
			return nil, fmt.Errorf("写打印记录失败: %w", err)
		}
		pid, _ := res.LastInsertId()
		out = append(out, LabelItem{
			ID: pid, Code: b.Code, Human: b.Human, Kind: "成品吨袋",
			IsReprint: flag, Reason: reason, PrintedBy: actor.Name,
		})
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_fg_lot", EntityID: fgLotID, Action: "print",
		NewValue:    fmt.Sprintf("%d 张（补打 %d 张）", len(out), countReprint(out)),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交失败: %w", err)
	}
	return out, nil
}

// ===== D6 · 返工 =====

// CreateRework 返工（D6）：★ 新建一个生产批（**独立取号**，不得原批加后缀）+ b_rework 关联。
//
//	★ 新批链根日期 = 新批创建日（当日），不是原批日期；
//	★ 前置：原批存在且 status <> 已作废；★ **不强制**原批有不合格成品（放宽读法，§6-16），
//	  以**必填 reason** 留痕替代。
func (s *Store) CreateRework(ctx context.Context, in ReworkInput, actor MDActor) (ProductionBatch, error) {
	reason := strings.TrimSpace(in.Reason)
	if reason == "" {
		return ProductionBatch{}, ErrReworkReason
	}
	src, err := s.GetProductionBatch(ctx, in.SrcBatchID)
	if err != nil {
		return ProductionBatch{}, err
	}
	if src.Status == BatchStatusVoid {
		return ProductionBatch{}, fmt.Errorf("%w：原批 #%d 已作废", ErrReworkSrcState, in.SrcBatchID)
	}

	custID, inMatID, outMatID := src.CustomerID, src.InputMaterialID, src.PlannedOutputMaterialID
	if in.CustomerID > 0 {
		custID = in.CustomerID
	}
	if in.InputMaterialID > 0 {
		inMatID = in.InputMaterialID
	}
	if in.PlannedOutputMaterialID > 0 {
		outMatID = in.PlannedOutputMaterialID
	}
	date := time.Now().Format("2006-01-02")
	custCode, outCode, err := lookupProdMaster(ctx, s.db, custID, inMatID, outMatID)
	if err != nil {
		return ProductionBatch{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ProductionBatch{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()
	lock := batchLockName(custID, outMatID, date)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return ProductionBatch{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ProductionBatch{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	newID, _, err := insertBatchTx(ctx, tx, custID, inMatID, outMatID, date, custCode, outCode,
		"返工："+reason, actor)
	if err != nil {
		return ProductionBatch{}, err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO b_rework (new_batch_id, src_batch_id, reason, created_by)
VALUES (?,?,?,?)`, newID, in.SrcBatchID, reason, actor.OpenID); err != nil {
		return ProductionBatch{}, fmt.Errorf("写返工关联失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_rework", EntityID: newID, Action: "create",
		NewValue:    fmt.Sprintf("原批 #%d → 新批 #%d", in.SrcBatchID, newID),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return ProductionBatch{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProductionBatch{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetProductionBatch(ctx, newID)
}

// ListReworks 查返工关联（读入口 prod.rework/READ）：src_batch_id 为空 ⇒ 全部。
func (s *Store) ListReworks(ctx context.Context, srcBatchID int64) ([]ReworkRow, error) {
	q := `
SELECT r.id, r.new_batch_id, COALESCE(nb.code, ''), r.src_batch_id, COALESCE(sb.code, ''),
       r.reason, r.created_at, r.created_by
  FROM b_rework r
  LEFT JOIN b_production_batch nb ON nb.id = r.new_batch_id
  LEFT JOIN b_production_batch sb ON sb.id = r.src_batch_id`
	args := []interface{}{}
	if srcBatchID > 0 {
		q += ` WHERE r.src_batch_id = ?`
		args = append(args, srcBatchID)
	}
	q += ` ORDER BY r.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询返工关联失败: %w", err)
	}
	defer rows.Close()
	out := []ReworkRow{}
	for rows.Next() {
		var r ReworkRow
		var created sql.NullTime
		if err := rows.Scan(&r.ID, &r.NewBatchID, &r.NewBatchCode, &r.SrcBatchID,
			&r.SrcBatchCode, &r.Reason, &created, &r.CreatedBy); err != nil {
			return nil, fmt.Errorf("读取返工关联失败: %w", err)
		}
		if created.Valid {
			t := created.Time
			r.CreatedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
