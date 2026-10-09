package store

// ===== M7 出货 · 持久化（D1 装车归集 / D2 出场登记 / D3 撤销发起·审批）=====
//
// ★★ 跨单唯一由应用层三段合取落实（任务包 §6-2 / A3 最高优先判据）：
//	uk_ship_bag(shipment_id, fg_bag_id) 只保证**单内**不重复，不是跨单全局唯一
//	⇒ 判据 = ① 袋行 SELECT … FOR UPDATE；② b_fg_bag.status='在库'；
//	③ NOT EXISTS（该袋在 status<>'已撤销' 的出货单中存在明细）。★ 不改 schema。
// ★ 袋 status 只在本批两处变（§6-3）：出场登记 ⇒ 已出厂；撤销审批 ⇒ 回退 在库。
//	归集本身不动袋状态（占用由第 ③ 段表达）。
// ★ shipment_no 非追踪码（§6-1）：CH + YYMMDD + '-' + 当日 3 位序号，取号 =
//	GET_LOCK 命名锁 + 同事务前缀取最大 +1（禁「先查 max 再 +1」），超 999 明确报错。
// ★★ 撤销留痕落 s_audit_log（§6-4）：b_shipment 无 void 列（冻结件）⇒
//	init 只写审计不改状态、approve 才推进状态；发起人 ≠ 审批人（403）。
// ★ 建单即「已出厂」，是否出场登记用 ship_at IS NULL 表达（§6-6）；
//	不写 b_fg_lot.status（§6-7）；明细行撤销后保留、不物理删除（§6-21）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// M7 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrShipNotFound      = errors.New("出货单不存在")
	ErrShipBadInput      = errors.New("出货输入不合法")
	ErrShipState         = errors.New("出货单当前状态不允许该操作")
	ErrShipSeqBusy       = errors.New("出货单号取号繁忙，请重试")
	ErrShipSeqOverflow   = errors.New("当日出货单号序号已达上限 999，请人工决策（超限不自动进位）")
	ErrShipBagCollected  = errors.New("该成品袋已被出货单归集")
	ErrShipCrossCustomer = errors.New("跨客户装车须拆单")
	ErrShipNoItems       = errors.New("出货单无明细，不得出场登记")
	ErrShipDeparted      = errors.New("该出货单已登记出场，不得重复登记")
	ErrShipVoidNotInit   = errors.New("该出货单尚未发起撤销")
	ErrShipVoidSelf      = errors.New("撤销发起人不得自行审批")
	ErrShipVoidPending   = errors.New("该出货单已有待审批的撤销发起，请勿重复发起")
)

// 状态字面量（★ 逐字取自 spec/schema.sql 列注释的中文原词，§6-6）。
const (
	ShipStatusOut  = "已出厂"
	ShipStatusVoid = "已撤销"

	FgBagStatusOut = "已出厂"

	// ShipSeqMax 出货单号当日序号上限（3 位，不自动进位）。
	ShipSeqMax = 999
)

// ===== 读模型 =====

// Shipment 是出货单的读模型（★ 出场登记与否由 ship_at 是否为 NULL 表达）。
type Shipment struct {
	ID           int64      `json:"id"`
	ShipmentNo   string     `json:"shipment_no"`
	CustomerID   int64      `json:"customer_id"`
	CustomerName string     `json:"customer_name"`
	VehicleID    *int64     `json:"vehicle_id"`
	PlateNo      string     `json:"plate_no"`
	Driver       string     `json:"driver"`
	ShipAt       *time.Time `json:"ship_at"`
	Operator     string     `json:"operator"`
	Status       string     `json:"status"`
	Remark       string     `json:"remark"`
	CreatedAt    *time.Time `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
}

// ShipmentItem 是出货明细（逐袋扫码归集的行）。
type ShipmentItem struct {
	ID         int64      `json:"id"`
	ShipmentID int64      `json:"shipment_id"`
	FgBagID    int64      `json:"fg_bag_id"`
	BagCode    string     `json:"bag_code"`
	BagHuman   string     `json:"bag_human"`
	BagStatus  string     `json:"bag_status"`
	FgLotID    int64      `json:"fg_lot_id"`
	FgLotCode  string     `json:"fg_lot_code"`
	CustomerID int64      `json:"customer_id"`
	CreatedAt  *time.Time `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
}

// ShipVoidRecord 是撤销的发起 / 审批留痕（读自 s_audit_log）。
type ShipVoidRecord struct {
	ID          int64      `json:"id"`
	Action      string     `json:"action"`
	ActorOpenID string     `json:"actor_open_id"`
	ActorName   string     `json:"actor_name"`
	Reason      string     `json:"reason"`
	At          *time.Time `json:"at"`
}

// ===== 入参 =====

// CreateShipmentInput 是建单 + 批量归集的入参（bag_codes 至少 1 个）。
type CreateShipmentInput struct {
	CustomerID int64    `json:"customer_id"` // 缺省 0 ⇒ 由首个袋推定
	BagCodes   []string `json:"bag_codes"`
	Remark     string   `json:"remark"`
}

// AddShipmentItemInput 是向已有单追加扫码的入参。
type AddShipmentItemInput struct {
	BagCode string `json:"bag_code"`
}

// DepartInput 是出场登记入参（plate_no / driver 必填非空）。
type DepartInput struct {
	VehicleID *int64 `json:"vehicle_id"`
	PlateNo   string `json:"plate_no"`
	Driver    string `json:"driver"`
	ShipAt    string `json:"ship_at"` // 可空 ⇒ 服务端当前时刻
	Operator  string `json:"operator"`
}

// ===== 行扫描 =====

const shipmentSelect = `
SELECT s.id, s.shipment_no, s.customer_id, COALESCE(c.name, ''),
       s.vehicle_id, COALESCE(s.plate_no, ''), COALESCE(s.driver, ''),
       s.ship_at, COALESCE(s.operator, ''), s.status, COALESCE(s.remark, ''),
       s.created_at, s.created_by
  FROM b_shipment s
  LEFT JOIN m_customer c ON c.id = s.customer_id`

func scanShipment(sc interface{ Scan(...interface{}) error }) (Shipment, error) {
	var s Shipment
	var vehicleID sql.NullInt64
	var shipAt sql.NullTime
	var created sql.NullTime
	err := sc.Scan(&s.ID, &s.ShipmentNo, &s.CustomerID, &s.CustomerName,
		&vehicleID, &s.PlateNo, &s.Driver, &shipAt, &s.Operator,
		&s.Status, &s.Remark, &created, &s.CreatedBy)
	if err != nil {
		return Shipment{}, err
	}
	if vehicleID.Valid {
		v := vehicleID.Int64
		s.VehicleID = &v
	}
	if shipAt.Valid {
		t := shipAt.Time
		s.ShipAt = &t
	}
	if created.Valid {
		t := created.Time
		s.CreatedAt = &t
	}
	return s, nil
}

const shipmentItemSelect = `
SELECT i.id, i.shipment_id, i.fg_bag_id, b.code, b.status,
       b.fg_lot_id, l.code, l.customer_id, i.created_at, i.created_by
  FROM b_shipment_item i
  JOIN b_fg_bag b ON b.id = i.fg_bag_id
  JOIN b_fg_lot l ON l.id = b.fg_lot_id`

func scanShipmentItem(sc interface{ Scan(...interface{}) error }) (ShipmentItem, error) {
	var it ShipmentItem
	var created sql.NullTime
	err := sc.Scan(&it.ID, &it.ShipmentID, &it.FgBagID, &it.BagCode, &it.BagStatus,
		&it.FgLotID, &it.FgLotCode, &it.CustomerID, &created, &it.CreatedBy)
	if err != nil {
		return ShipmentItem{}, err
	}
	it.BagHuman, _ = codec.ToHuman(it.BagCode)
	if created.Valid {
		t := created.Time
		it.CreatedAt = &t
	}
	return it, nil
}

// ===== 码解析（★ 复用 codec，要求 T='E' 成品袋码）=====

// normalizeFgBagCode 解析成品袋码（裸串或人读行）；非 E 类 ⇒ 400。
func normalizeFgBagCode(raw string) (codec.Parsed, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return codec.Parsed{}, fmt.Errorf("%w：bag_code 不能为空", ErrShipBadInput)
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
	if p.Seg.T != "E" {
		return codec.Parsed{}, fmt.Errorf("%w：装车只扫成品袋码（E），实际 %s", ErrShipBadInput, p.Seg.T)
	}
	return p, nil
}

// ===== 取号（GET_LOCK + 同事务，§6-1）=====

// shipSeqLockName 出货单号空间的命名锁名（≤64 字符）。
func shipSeqLockName(now time.Time) string {
	return "jxlab.ship." + now.Format("060102")
}

// nextShipNo 在**持命名锁的事务内**取当日出货单号（超 999 明确报错，不进位）。
func nextShipNo(ctx context.Context, tx *sql.Tx, now time.Time) (string, error) {
	prefix := fmt.Sprintf("CH%s-", now.Format("060102"))
	rows, err := tx.QueryContext(ctx,
		`SELECT shipment_no FROM b_shipment WHERE shipment_no LIKE ?`, prefix+"%")
	if err != nil {
		return "", fmt.Errorf("读取出货单号失败: %w", err)
	}
	defer rows.Close()
	max := 0
	for rows.Next() {
		var no string
		if err := rows.Scan(&no); err != nil {
			return "", fmt.Errorf("读取出货单号失败: %w", err)
		}
		n, err := parseShipSeq(strings.TrimPrefix(no, prefix))
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
	if next > ShipSeqMax {
		return "", ErrShipSeqOverflow
	}
	return fmt.Sprintf("%s%03d", prefix, next), nil
}

func parseShipSeq(s string) (int, error) {
	if len(s) == 0 {
		return 0, fmt.Errorf("空序号")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("非数字序段 %q", s)
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// ===== 每袋归集校验（5 项中的 ②③④；① 由 normalizeFgBagCode 承担，⑤ 在调用方查单状态）=====

// resolveShipmentBagTx 锁袋并做跨单唯一 / 状态 / 所属客户校验（§6-2 三段合取）。
// 返回袋所属客户 id（供跨客户校验）。
func resolveShipmentBagTx(ctx context.Context, tx *sql.Tx, fullCode string) (bagID, fgLotID, custID int64, err error) {
	var bagStatus string
	err = tx.QueryRowContext(ctx, `
SELECT b.id, b.fg_lot_id, b.status, l.customer_id
  FROM b_fg_bag b
  JOIN b_fg_lot l ON l.id = b.fg_lot_id
 WHERE b.code = ? FOR UPDATE`, fullCode).
		Scan(&bagID, &fgLotID, &bagStatus, &custID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("%w：袋码 %s", ErrCodeUnknown, fullCode)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("读取成品袋失败: %w", err)
	}
	// ② 袋状态守卫（袋行已 FOR UPDATE）
	switch bagStatus {
	case FgBagStatusInStock:
		// 可归集
	case FgBagStatusOut:
		return 0, 0, 0, fmt.Errorf("%w：袋 #%d 已出厂", ErrShipState, bagID)
	case BagStatusVoid:
		return 0, 0, 0, fmt.Errorf("%w：袋 #%d 已作废", ErrShipState, bagID)
	default:
		return 0, 0, 0, fmt.Errorf("%w：袋 #%d 当前状态「%s」", ErrShipState, bagID, bagStatus)
	}
	// ③ ★★ 跨单唯一：在未撤销的出货单中不得已有明细（uk_ship_bag 只管单内）
	var shipped sql.NullString
	err = tx.QueryRowContext(ctx, `
SELECT s.shipment_no
  FROM b_shipment_item i
  JOIN b_shipment s ON s.id = i.shipment_id
 WHERE i.fg_bag_id = ? AND s.status <> ?
 LIMIT 1`, bagID, ShipStatusVoid).Scan(&shipped)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("检查归集状态失败: %w", err)
	}
	if shipped.Valid {
		return 0, 0, 0, fmt.Errorf("%w：出货单 %s", ErrShipBagCollected, shipped.String)
	}
	return bagID, fgLotID, custID, nil
}

// ===== D1 · 建单 + 逐袋归集（同一事务）=====

// CreateShipment 建出货单并归集 bag_codes（至少 1 个）。
//
// ★ 取号 = 同一 conn 上 GET_LOCK + 同一事务（CreateProductionBatch 同型）；
//
//	customer_id 缺省由首个袋推定，须与其余袋一致（跨客户 ⇒ 400 拆单）。
func (s *Store) CreateShipment(ctx context.Context, in CreateShipmentInput, actor MDActor) (Shipment, error) {
	codes := make([]string, 0, len(in.BagCodes))
	seen := map[string]bool{}
	for _, raw := range in.BagCodes {
		c := strings.TrimSpace(raw)
		if c == "" {
			continue
		}
		key := strings.ToUpper(c)
		if strings.Contains(c, "-") {
			if f, err := codec.FromHuman(c); err == nil {
				key = f
			}
		}
		if seen[key] {
			return Shipment{}, fmt.Errorf("%w：袋码 %s 在本次提交中重复", ErrShipBadInput, c)
		}
		seen[key] = true
		codes = append(codes, c)
	}
	if len(codes) == 0 {
		return Shipment{}, fmt.Errorf("%w：bag_codes 至少 1 个", ErrShipBadInput)
	}

	now := time.Now()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Shipment{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := shipSeqLockName(now)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return Shipment{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Shipment{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	shipNo, err := nextShipNo(ctx, tx, now)
	if err != nil {
		return Shipment{}, err
	}

	customerID := in.CustomerID
	for i, raw := range codes {
		p, perr := normalizeFgBagCode(raw)
		if perr != nil {
			return Shipment{}, perr
		}
		_, _, lotCust, rerr := resolveShipmentBagTx(ctx, tx, p.Full)
		if rerr != nil {
			return Shipment{}, rerr
		}
		if i == 0 && customerID == 0 {
			customerID = lotCust
		}
		// ④ 跨客户必须拆单（§6-5）
		if lotCust != customerID {
			return Shipment{}, fmt.Errorf("%w：袋 %s 属客户 #%d，本单客户 #%d",
				ErrShipCrossCustomer, p.Full, lotCust, customerID)
		}
	}
	if customerID == 0 {
		return Shipment{}, fmt.Errorf("%w：无法推定客户", ErrShipBadInput)
	}

	res, err := tx.ExecContext(ctx, `
INSERT INTO b_shipment (shipment_no, customer_id, status, remark, created_by)
VALUES (?,?,?,?,?)`,
		shipNo, customerID, ShipStatusOut, nullStr(in.Remark), actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return Shipment{}, fmt.Errorf("%w：出货单号 %s 已存在", ErrShipSeqBusy, shipNo)
		}
		return Shipment{}, fmt.Errorf("建出货单失败: %w", err)
	}
	shipID, _ := res.LastInsertId()

	for _, raw := range codes {
		p, perr := normalizeFgBagCode(raw)
		if perr != nil {
			return Shipment{}, perr
		}
		var bagID int64
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM b_fg_bag WHERE code = ?`, p.Full).Scan(&bagID); err != nil {
			return Shipment{}, fmt.Errorf("读取成品袋失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO b_shipment_item (shipment_id, fg_bag_id, created_by) VALUES (?,?,?)`,
			shipID, bagID, actor.OpenID); err != nil {
			if isDuplicateErr(err) {
				return Shipment{}, fmt.Errorf("%w：出货单 %s", ErrShipBagCollected, shipNo)
			}
			return Shipment{}, fmt.Errorf("写出货明细失败: %w", err)
		}
	}

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_shipment", EntityID: shipID, Action: "create",
		NewValue:    fmt.Sprintf("%s 归集 %d 袋", shipNo, len(codes)),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: strings.TrimSpace(in.Remark),
	}); err != nil {
		return Shipment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Shipment{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetShipment(ctx, shipID)
}

// AddShipmentItem 向同一张单追加扫码（同一套 5 项校验，§6-2）。
func (s *Store) AddShipmentItem(ctx context.Context, shipmentID int64, in AddShipmentItemInput, actor MDActor) (Shipment, error) {
	p, err := normalizeFgBagCode(in.BagCode)
	if err != nil {
		return Shipment{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Shipment{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var customerID int64
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT customer_id, status FROM b_shipment WHERE id = ? FOR UPDATE`, shipmentID).
		Scan(&customerID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：#%d", ErrShipNotFound, shipmentID)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("读取出货单失败: %w", err)
	}
	// ⑤ 本单 status='已出厂'（已撤销的单不得再扫码）
	if status != ShipStatusOut {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d 当前「%s」", ErrShipState, shipmentID, status)
	}

	bagID, _, lotCust, err := resolveShipmentBagTx(ctx, tx, p.Full)
	if err != nil {
		return Shipment{}, err
	}
	if lotCust != customerID {
		return Shipment{}, fmt.Errorf("%w：袋 %s 属客户 #%d，本单客户 #%d",
			ErrShipCrossCustomer, p.Full, lotCust, customerID)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO b_shipment_item (shipment_id, fg_bag_id, created_by) VALUES (?,?,?)`,
		shipmentID, bagID, actor.OpenID); err != nil {
		if isDuplicateErr(err) {
			return Shipment{}, fmt.Errorf("%w：出货单 #%d", ErrShipBagCollected, shipmentID)
		}
		return Shipment{}, fmt.Errorf("写出货明细失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Shipment{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetShipment(ctx, shipmentID)
}

// ===== D2 · 出场登记 =====

// DepartShipment 出场登记：plate_no / driver 必填；已登记 ⇒ 409 不覆盖；
// 空单 ⇒ 409。同事务把该单所有明细袋置「已出厂」。★ 不动 b_fg_lot.status（§6-7）。
func (s *Store) DepartShipment(ctx context.Context, shipmentID int64, in DepartInput, actor MDActor) (Shipment, error) {
	plateNo := strings.TrimSpace(in.PlateNo)
	driver := strings.TrimSpace(in.Driver)
	if plateNo == "" {
		return Shipment{}, fmt.Errorf("%w：plate_no（车牌）必填", ErrShipBadInput)
	}
	if driver == "" {
		return Shipment{}, fmt.Errorf("%w：driver（司机）必填", ErrShipBadInput)
	}
	shipAt := time.Now()
	if strings.TrimSpace(in.ShipAt) != "" {
		t, err := parseDateTime(in.ShipAt)
		if err != nil {
			return Shipment{}, err
		}
		shipAt = t
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Shipment{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status string
	var registeredAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT status, ship_at FROM b_shipment WHERE id = ? FOR UPDATE`, shipmentID).
		Scan(&status, &registeredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：#%d", ErrShipNotFound, shipmentID)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("读取出货单失败: %w", err)
	}
	if status != ShipStatusOut {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d 当前「%s」", ErrShipState, shipmentID, status)
	}
	if registeredAt.Valid {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d", ErrShipDeparted, shipmentID)
	}
	var itemCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM b_shipment_item WHERE shipment_id = ?`, shipmentID).
		Scan(&itemCount); err != nil {
		return Shipment{}, fmt.Errorf("统计出货明细失败: %w", err)
	}
	if itemCount == 0 {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d", ErrShipNoItems, shipmentID)
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE b_shipment
   SET vehicle_id = ?, plate_no = ?, driver = ?, ship_at = ?, operator = ?
 WHERE id = ?`,
		nullIntPtr(in.VehicleID), plateNo, driver, shipAt, nullStr(in.Operator),
		shipmentID); err != nil {
		return Shipment{}, fmt.Errorf("更新出货单失败: %w", err)
	}
	// 同事务：该单所有明细袋 → 已出厂
	if _, err := tx.ExecContext(ctx, `
UPDATE b_fg_bag SET status = ?
 WHERE id IN (SELECT fg_bag_id FROM b_shipment_item WHERE shipment_id = ?)`,
		FgBagStatusOut, shipmentID); err != nil {
		return Shipment{}, fmt.Errorf("更新成品袋状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_shipment", EntityID: shipmentID, Action: "depart",
		NewValue:    fmt.Sprintf("%s %s 司机 %s %s", plateNo, shipAt.Format("2006-01-02 15:04:05"), driver, status),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return Shipment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Shipment{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetShipment(ctx, shipmentID)
}

// ===== D3 · 撤销（发起 ≠ 审批；留痕落 s_audit_log）=====

// InitShipVoid 撤销**发起**：reason 必填；**不改状态、不动袋**；
// 同一单已有未审批的 init ⇒ 409。审计 action='ship_void_init'。
func (s *Store) InitShipVoid(ctx context.Context, shipmentID int64, reason string, actor MDActor) (Shipment, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Shipment{}, fmt.Errorf("%w：reason（撤销原因）必填", ErrShipBadInput)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Shipment{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status, shipNo string
	err = tx.QueryRowContext(ctx,
		`SELECT status, shipment_no FROM b_shipment WHERE id = ? FOR UPDATE`, shipmentID).
		Scan(&status, &shipNo)
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：#%d", ErrShipNotFound, shipmentID)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("读取出货单失败: %w", err)
	}
	if status != ShipStatusOut {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d 当前「%s」", ErrShipState, shipmentID, status)
	}

	// 幂等：已有 init（尚未审批 —— 审批必推进状态，前面已拦已撤销）⇒ 409
	var inits int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM s_audit_log
 WHERE entity = 'b_shipment' AND entity_id = ? AND action = 'ship_void_init'`,
		shipmentID).Scan(&inits); err != nil {
		return Shipment{}, fmt.Errorf("查询撤销发起失败: %w", err)
	}
	if inits > 0 {
		return Shipment{}, fmt.Errorf("%w：出货单 %s", ErrShipVoidPending, shipNo)
	}

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_shipment", EntityID: shipmentID, Action: "ship_void_init",
		NewValue:    shipNo + " 发起撤销",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: reason,
	}); err != nil {
		return Shipment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Shipment{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetShipment(ctx, shipmentID)
}

// ApproveShipVoid 撤销**审批**：未发起 ⇒ 409；发起人 == 审批人 ⇒ 403（TC-M7-05）。
// 生效（同事务）：单置「已撤销」＋ 明细袋中仅「已出厂」的回退「在库」（作废袋不动）；
// 明细行保留（§6-21）。审计 action='ship_void_approve'。
func (s *Store) ApproveShipVoid(ctx context.Context, shipmentID int64, actor MDActor) (Shipment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Shipment{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var status, shipNo string
	err = tx.QueryRowContext(ctx,
		`SELECT status, shipment_no FROM b_shipment WHERE id = ? FOR UPDATE`, shipmentID).
		Scan(&status, &shipNo)
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：#%d", ErrShipNotFound, shipmentID)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("读取出货单失败: %w", err)
	}
	if status != ShipStatusOut {
		return Shipment{}, fmt.Errorf("%w：出货单 #%d 当前「%s」", ErrShipState, shipmentID, status)
	}

	var initBy, initReason string
	err = tx.QueryRowContext(ctx, `
SELECT COALESCE(actor_open_id, ''), COALESCE(reason, '')
  FROM s_audit_log
 WHERE entity = 'b_shipment' AND entity_id = ? AND action = 'ship_void_init'
 ORDER BY id ASC LIMIT 1`, shipmentID).Scan(&initBy, &initReason)
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：出货单 %s", ErrShipVoidNotInit, shipNo)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("查询撤销发起失败: %w", err)
	}
	// ★★ TC-M7-05：审批人 ≠ 发起人
	if initBy != "" && initBy == actor.OpenID {
		return Shipment{}, fmt.Errorf("%w：出货单 %s", ErrShipVoidSelf, shipNo)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE b_shipment SET status = ? WHERE id = ?`, ShipStatusVoid, shipmentID); err != nil {
		return Shipment{}, fmt.Errorf("更新出货单状态失败: %w", err)
	}
	// 袋回退：仅「已出厂」→「在库」；作废袋不动；明细行保留
	if _, err := tx.ExecContext(ctx, `
UPDATE b_fg_bag SET status = ?
 WHERE status = ?
   AND id IN (SELECT fg_bag_id FROM b_shipment_item WHERE shipment_id = ?)`,
		FgBagStatusInStock, FgBagStatusOut, shipmentID); err != nil {
		return Shipment{}, fmt.Errorf("回退成品袋状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_shipment", EntityID: shipmentID, Action: "ship_void_approve",
		NewValue:    shipNo + " 撤销生效",
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
		Reason: initReason,
	}); err != nil {
		return Shipment{}, err
	}
	if err := tx.Commit(); err != nil {
		return Shipment{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetShipment(ctx, shipmentID)
}

// ===== 读 =====

// GetShipment 读单张出货单。
func (s *Store) GetShipment(ctx context.Context, id int64) (Shipment, error) {
	sh, err := scanShipment(s.db.QueryRowContext(ctx, shipmentSelect+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Shipment{}, fmt.Errorf("%w：#%d", ErrShipNotFound, id)
	}
	if err != nil {
		return Shipment{}, fmt.Errorf("读取出货单失败: %w", err)
	}
	return sh, nil
}

// ListShipments 列出货单（status / customer_id 可作过滤；读入口 ship.load.scan/READ）。
func (s *Store) ListShipments(ctx context.Context, status string, customerID int64) ([]Shipment, error) {
	q := shipmentSelect
	args := []interface{}{}
	where := []string{}
	if strings.TrimSpace(status) != "" {
		where = append(where, `s.status = ?`)
		args = append(args, strings.TrimSpace(status))
	}
	if customerID > 0 {
		where = append(where, `s.customer_id = ?`)
		args = append(args, customerID)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	q += ` ORDER BY s.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询出货单失败: %w", err)
	}
	defer rows.Close()
	out := []Shipment{}
	for rows.Next() {
		sh, err := scanShipment(rows)
		if err != nil {
			return nil, fmt.Errorf("读取出货单失败: %w", err)
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// ListShipmentItems 列某单的明细袋码。
func (s *Store) ListShipmentItems(ctx context.Context, shipmentID int64) ([]ShipmentItem, error) {
	rows, err := s.db.QueryContext(ctx,
		shipmentItemSelect+` WHERE i.shipment_id = ? ORDER BY i.id ASC`, shipmentID)
	if err != nil {
		return nil, fmt.Errorf("查询出货明细失败: %w", err)
	}
	defer rows.Close()
	out := []ShipmentItem{}
	for rows.Next() {
		it, err := scanShipmentItem(rows)
		if err != nil {
			return nil, fmt.Errorf("读取出货明细失败: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ShipVoidRecords 读该单的撤销发起 / 审批留痕（s_audit_log）。
func (s *Store) ShipVoidRecords(ctx context.Context, shipmentID int64) ([]ShipVoidRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, action, COALESCE(actor_open_id, ''), COALESCE(actor_name, ''),
       COALESCE(reason, ''), at
  FROM s_audit_log
 WHERE entity = 'b_shipment' AND entity_id = ?
   AND action IN ('ship_void_init', 'ship_void_approve')
 ORDER BY id ASC`, shipmentID)
	if err != nil {
		return nil, fmt.Errorf("查询撤销留痕失败: %w", err)
	}
	defer rows.Close()
	out := []ShipVoidRecord{}
	for rows.Next() {
		var r ShipVoidRecord
		var at sql.NullTime
		if err := rows.Scan(&r.ID, &r.Action, &r.ActorOpenID, &r.ActorName, &r.Reason, &at); err != nil {
			return nil, fmt.Errorf("读取撤销留痕失败: %w", err)
		}
		if at.Valid {
			t := at.Time
			r.At = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
