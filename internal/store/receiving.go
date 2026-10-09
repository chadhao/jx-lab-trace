package store

// ===== M3 收货与打码 · 持久化（D2 预报与车序 / D3 到货确认）=====
//
// ★ 车序分配的并发正确性（spec/code-rules.json#rules.concurrent_alloc
//	= must_serialize_in_transaction，任务包 §6-7）：
//	b_arrival_notice 的 idx_notice_seq 是 **KEY 不是 UNIQUE**（spec 如此登记），
//	⇒ DB 侧没有唯一索引兜底，「先查最大值再 +1」在并发下必然重号。
//	本实现用**两道锁**串行化取号，整段读取+插入落在同一事务内：
//	  ① MySQL 命名锁 GET_LOCK（跨会话的显式串行点，对空表同样有效 ——
//	     仅靠 InnoDB gap lock 不够：两个空范围的 gap lock 彼此兼容，
//	     双方都会读到 max=0 并各自插入 01）；
//	  ② 事务内再做一次 SELECT ... ORDER BY seq_no DESC LIMIT 1 FOR UPDATE
//	     （对已有行的当前读）。
//	★ 之所以不改 spec 加唯一索引：任务包 §6-7 明示「若认为需要 DB 级唯一约束 ⇒
//	开议题（不得自改 spec）」—— 本轮按规格实现，取号正确性由自身保证。
//
// ★ 空号跳号不回收：MAX 查询**不过滤 status**，已取消 / 空号的行照样计入最大值；
//	超限（>99）明确报错，绝不自动进位（加宽段位会破坏定长码）。
// ★ 改预报不改车序：更新语句白名单里**没有 seq_no，也没有 arrive_date**
//	（链根日期与车序、已生成的码绑定）。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/codec"
)

// M3 的哨兵错误（httpapi 据此映射状态码）。
var (
	ErrRecvNotFound     = errors.New("对象不存在")
	ErrRecvBadInput     = errors.New("输入不合法")
	ErrSeqOverflow      = errors.New("当日该客户该物料的车序已达上限 99，请人工决策（超限不自动进位）")
	ErrSeqBusy          = errors.New("车序取号繁忙，请重试")
	ErrNoticeState      = errors.New("预报单当前状态不允许该操作")
	ErrTruckState       = errors.New("车次当前状态不允许该操作")
	ErrNotWeighed       = errors.New("尚未过磅（缺净重），不能生成袋码")
	ErrBagsExist        = errors.New("该车次已生成袋码，不能重复生成")
	ErrWeighAfterBags   = errors.New("已生成袋码，不能修改重量")
	ErrBagInUse         = errors.New("该袋已取样 / 已投料，不允许作废")
	ErrAlreadyVoid      = errors.New("该对象已作废")
	ErrVoidReason       = errors.New("作废必须填写原因")
	ErrReprintReason    = errors.New("补打必须填写原因")
	ErrNoReturnDecision = errors.New("未经质检「退货」处置判定，不能退车")
	ErrCodeUnknown      = errors.New("格式正确但系统内不存在")
	ErrCodeUnsupported  = errors.New("该对象类型不在本批（收货与打码）支持范围")
)

// 状态字面量（与 spec/schema.sql 的列注释一致）。
const (
	NoticeStatusForecast = "预报"
	NoticeStatusArrived  = "已到货"
	NoticeStatusCanceled = "已取消"
	NoticeStatusVacant   = "空号"

	TruckStatusPending  = "待检"
	TruckStatusReturned = "已退货"

	BagStatusInStock = "在库"
	BagStatusVoid    = "作废"

	BizTypeCG = "CG" // 客供（受托加工）
	BizTypeZG = "ZG" // 自购 ⇒ 码里客户段填保留值 0000

	// SeqMax 车序上限（spec#sequence_spaces.车序 max=99）。
	SeqMax = 99
	// BagSeqMax 袋序上限（spec#sequence_spaces.袋序 max=999）。
	BagSeqMax = 999
)

// ===== 十进制（吨）小工具 =====
//
// ★ 净重 / 袋重都是 DECIMAL(18,3)，用**毫吨整数**做算术，避免二进制浮点误差；
//	落库与出参再格式化成 3 位小数。袋重摊算的四舍五入按 half-up。

// tons 是以毫吨（1/1000 吨）为单位的重量。
type tons struct {
	milli int64
}

// UnmarshalJSON 接受 `30.5` 或 `"30.5"` 两种形态，最多 3 位小数。
func (t *tons) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" {
		t.milli = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	m, err := parseTonsMilli(s)
	if err != nil {
		return err
	}
	t.milli = m
	return nil
}

// String 返回 3 位小数的吨表示（落库形态）。
func (t tons) String() string { return milliStr(t.milli) }

// Milli 返回毫吨整数。
func (t tons) Milli() int64 { return t.milli }

// IsZero 是否为 0。
func (t tons) IsZero() bool { return t.milli == 0 }

func parseTonsMilli(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%w：重量不能为空", ErrRecvBadInput)
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	whole, frac := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		whole, frac = s[:i], s[i+1:]
	}
	if whole == "" {
		whole = "0"
	}
	var w int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w：%q 不是合法的重量", ErrRecvBadInput, s)
		}
		w = w*10 + int64(c-'0')
	}
	if len(frac) > 3 {
		return 0, fmt.Errorf("%w：重量最多 3 位小数（DECIMAL(18,3)）", ErrRecvBadInput)
	}
	for len(frac) < 3 {
		frac += "0"
	}
	var f int64
	for _, c := range frac {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w：%q 不是合法的重量", ErrRecvBadInput, s)
		}
		f = f*10 + int64(c-'0')
	}
	m := w*1000 + f
	if neg {
		m = -m
	}
	return m, nil
}

func milliStr(m int64) string {
	sign := ""
	if m < 0 {
		sign, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%03d", sign, m/1000, m%1000)
}

// divMilliRound 净重 ÷ 袋数，四舍五入到毫吨（half-up）。
//
// ★ TC-M3-09：30.5t ÷ 30 = 1.01666… ⇒ 1017 毫吨 ⇒ 落库 1.017（列精度 3 位）。
func divMilliRound(netMilli, bags int64) int64 {
	if bags <= 0 {
		return 0
	}
	n := netMilli
	if n < 0 {
		// 负值向下取整方向与 half-up 相反，这里统一按绝对值再还原
		return -divMilliRound(-n, bags)
	}
	return (2*n + bags) / (2 * bags)
}

// ===== 预报单（D2）=====

// NoticeInput 是预报登记的入参。
type NoticeInput struct {
	CustomerID  int64      `json:"customer_id"`
	MaterialID  int64      `json:"material_id"`
	VehicleID   int64      `json:"vehicle_id"`
	PlateNo     string     `json:"plate_no"`
	Driver      string     `json:"driver"`
	Phone       string     `json:"phone"`
	ETA         *time.Time `json:"eta"`
	EstBagCount int        `json:"est_bag_count"`
	BizType     string     `json:"biz_type"`
	// ArriveDate 是链根日期（车序空间的「到货日」，YYYY-MM-DD）。
	// 分配车序时确定，之后**不可改**（改了会让已分配的车序与码错位）。
	ArriveDate string `json:"arrive_date"`
}

// ArrivalNotice 是预报单的读模型。
type ArrivalNotice struct {
	ID           int64      `json:"id"`
	NoticeNo     string     `json:"notice_no"`
	CustomerID   int64      `json:"customer_id"`
	CustomerCode string     `json:"customer_code"`
	CustomerName string     `json:"customer_name"`
	MaterialID   int64      `json:"material_id"`
	MaterialCode string     `json:"material_code"`
	MaterialName string     `json:"material_name"`
	VehicleID    int64      `json:"vehicle_id"`
	PlateNo      string     `json:"plate_no"`
	Driver       string     `json:"driver"`
	Phone        string     `json:"phone"`
	ETA          *time.Time `json:"eta"`
	EstBagCount  int        `json:"est_bag_count"`
	BizType      string     `json:"biz_type"`
	SeqNo        int        `json:"seq_no"`
	ArriveDate   string     `json:"arrive_date"`
	Status       string     `json:"status"`
	CreatedAt    *time.Time `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
}

const noticeSelect = `
SELECT n.id, n.notice_no, n.customer_id, COALESCE(c.code,''), COALESCE(c.name,''),
       n.material_id, COALESCE(m.code,''), COALESCE(m.name,''),
       n.vehicle_id, COALESCE(n.plate_no,''), COALESCE(n.driver,''), COALESCE(n.phone,''),
       n.eta, n.est_bag_count, n.biz_type, n.seq_no, n.arrive_date, n.status,
       n.created_at, n.created_by
FROM b_arrival_notice n
LEFT JOIN m_customer c ON c.id = n.customer_id AND c.is_current = 1
LEFT JOIN m_material m ON m.id = n.material_id AND m.is_current = 1`

func scanNotice(sc interface{ Scan(...interface{}) error }) (ArrivalNotice, error) {
	var n ArrivalNotice
	var eta, arriveDate, createdAt sql.NullTime
	var seqNo, vehicleID, estCount sql.NullInt64
	err := sc.Scan(&n.ID, &n.NoticeNo, &n.CustomerID, &n.CustomerCode, &n.CustomerName,
		&n.MaterialID, &n.MaterialCode, &n.MaterialName,
		&vehicleID, &n.PlateNo, &n.Driver, &n.Phone,
		&eta, &estCount, &n.BizType, &seqNo, &arriveDate, &n.Status,
		&createdAt, &n.CreatedBy)
	if err != nil {
		return ArrivalNotice{}, err
	}
	n.VehicleID = vehicleID.Int64
	n.EstBagCount = int(estCount.Int64)
	if eta.Valid {
		t := eta.Time
		n.ETA = &t
	}
	if createdAt.Valid {
		t := createdAt.Time
		n.CreatedAt = &t
	}
	if arriveDate.Valid {
		n.ArriveDate = arriveDate.Time.Format("2006-01-02")
	}
	if seqNo.Valid {
		n.SeqNo = int(seqNo.Int64)
	}
	return n, nil
}

// ListNotices 列预报单（默认只看未到货的在办单，?all=1 看全部）。
func (s *Store) ListNotices(ctx context.Context, onlyOpen bool) ([]ArrivalNotice, error) {
	q := noticeSelect
	if onlyOpen {
		q += ` WHERE n.status = '预报'`
	}
	q += ` ORDER BY n.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询预报单失败: %w", err)
	}
	defer rows.Close()
	out := []ArrivalNotice{}
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNotice 读单条预报。
func (s *Store) GetNotice(ctx context.Context, id int64) (ArrivalNotice, error) {
	row := s.db.QueryRowContext(ctx, noticeSelect+` WHERE n.id = ?`, id)
	n, err := scanNotice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return ArrivalNotice{}, fmt.Errorf("%w：预报单 #%d", ErrRecvNotFound, id)
	}
	return n, err
}

// CreateNotice 登记预报 + **分配车序**（D2）。
//
// ★ 并发取号：GET_LOCK + 事务内 FOR UPDATE 双重串行（见文件头注释）。
func (s *Store) CreateNotice(ctx context.Context, in NoticeInput, actor MDActor) (ArrivalNotice, error) {
	if err := validateNoticeInput(&in); err != nil {
		return ArrivalNotice{}, err
	}
	if _, _, err := s.lookupCodes(ctx, in.CustomerID, in.MaterialID, in.BizType); err != nil {
		return ArrivalNotice{}, err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ArrivalNotice{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	lock := seqLockName(in.CustomerID, in.MaterialID, in.ArriveDate)
	if err := acquireSeqLock(ctx, conn, lock); err != nil {
		return ArrivalNotice{}, err
	}
	defer releaseSeqLock(conn, lock)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ArrivalNotice{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	seq, err := nextSeq(ctx, tx, in.CustomerID, in.MaterialID, in.ArriveDate)
	if err != nil {
		return ArrivalNotice{}, err
	}

	noticeNo, err := newNoticeNo(in.ArriveDate, seq)
	if err != nil {
		return ArrivalNotice{}, err
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_arrival_notice
  (notice_no, customer_id, material_id, vehicle_id, plate_no, driver, phone, eta,
   est_bag_count, biz_type, seq_no, arrive_date, status, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		noticeNo, in.CustomerID, in.MaterialID, nullInt(in.VehicleID),
		nullStr(in.PlateNo), nullStr(in.Driver), nullStr(in.Phone), in.ETA,
		in.EstBagCount, in.BizType, seq, in.ArriveDate, NoticeStatusForecast, actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return ArrivalNotice{}, fmt.Errorf("%w：预报单号冲突，请重试", ErrSeqBusy)
		}
		return ArrivalNotice{}, fmt.Errorf("登记预报失败: %w", err)
	}
	id, _ := res.LastInsertId()

	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_arrival_notice", EntityID: id, Action: "create",
		NewValue:    fmt.Sprintf("%s 车序%02d %s→%s", noticeNo, seq, in.ArriveDate, NoticeStatusForecast),
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return ArrivalNotice{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArrivalNotice{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetNotice(ctx, id)
}

// NoticePatch 是预报修改的可写字段（★ 白名单里没有 seq_no / arrive_date）。
type NoticePatch struct {
	PlateNo     *string    `json:"plate_no"`
	Driver      *string    `json:"driver"`
	Phone       *string    `json:"phone"`
	ETA         *time.Time `json:"eta"`
	EstBagCount *int       `json:"est_bag_count"`
	CustomerID  *int64     `json:"customer_id"`
	MaterialID  *int64     `json:"material_id"`
	VehicleID   *int64     `json:"vehicle_id"`
	BizType     *string    `json:"biz_type"`
	ArriveDate  *string    `json:"arrive_date"`
}

// UpdateNotice 修改预报字段 —— ★ 改预报不改车序（seq_no 一列都不碰）。
func (s *Store) UpdateNotice(ctx context.Context, id int64, patch NoticePatch, reason string, actor MDActor) (ArrivalNotice, error) {
	if patch.ArriveDate != nil && strings.TrimSpace(*patch.ArriveDate) != "" {
		return ArrivalNotice{}, fmt.Errorf("%w：链根日期（到货日）不可修改 —— 车序与已生成的码都绑定在该日", ErrRecvBadInput)
	}
	if strings.TrimSpace(reason) == "" {
		return ArrivalNotice{}, fmt.Errorf("%w：修改预报必须填写原因", ErrRecvBadInput)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArrivalNotice{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	cur, err := scanNotice(tx.QueryRowContext(ctx, noticeSelect+` WHERE n.id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ArrivalNotice{}, fmt.Errorf("%w：预报单 #%d", ErrRecvNotFound, id)
	}
	if err != nil {
		return ArrivalNotice{}, err
	}
	if cur.Status != NoticeStatusForecast {
		return ArrivalNotice{}, fmt.Errorf("%w：当前状态「%s」，只有「%s」可修改", ErrNoticeState, cur.Status, NoticeStatusForecast)
	}

	sets := []string{}
	args := []interface{}{}
	add := func(col string, v interface{}) {
		sets = append(sets, "`"+col+"` = ?")
		args = append(args, v)
	}
	if patch.PlateNo != nil {
		add("plate_no", nullStr(*patch.PlateNo))
	}
	if patch.Driver != nil {
		add("driver", nullStr(*patch.Driver))
	}
	if patch.Phone != nil {
		add("phone", nullStr(*patch.Phone))
	}
	if patch.ETA != nil {
		add("eta", *patch.ETA)
	}
	if patch.EstBagCount != nil {
		if *patch.EstBagCount < 0 {
			return ArrivalNotice{}, fmt.Errorf("%w：预计袋数不能为负", ErrRecvBadInput)
		}
		add("est_bag_count", *patch.EstBagCount)
	}
	if patch.VehicleID != nil {
		add("vehicle_id", nullInt(*patch.VehicleID))
	}
	if patch.CustomerID != nil || patch.MaterialID != nil || patch.BizType != nil {
		// 客户 / 物料 / 业务类型是码的组成段 ⇒ 改之前必须确认车序空间仍成立。
		// ★ 但 spec 明确「改预报不改车序」⇒ 允许改，车序保持原值。
		newCust := cur.CustomerID
		if patch.CustomerID != nil {
			newCust = *patch.CustomerID
		}
		newMat := cur.MaterialID
		if patch.MaterialID != nil {
			newMat = *patch.MaterialID
		}
		newBiz := cur.BizType
		if patch.BizType != nil {
			newBiz = strings.ToUpper(strings.TrimSpace(*patch.BizType))
		}
		if newBiz != BizTypeCG && newBiz != BizTypeZG {
			return ArrivalNotice{}, fmt.Errorf("%w：biz_type 只允许 CG / ZG", ErrRecvBadInput)
		}
		if _, _, err := s.lookupCodes(ctx, newCust, newMat, newBiz); err != nil {
			return ArrivalNotice{}, err
		}
		if patch.CustomerID != nil {
			add("customer_id", newCust)
		}
		if patch.MaterialID != nil {
			add("material_id", newMat)
		}
		if patch.BizType != nil {
			add("biz_type", newBiz)
		}
	}
	if len(sets) == 0 {
		return ArrivalNotice{}, fmt.Errorf("%w：没有可修改的字段", ErrRecvBadInput)
	}

	args = append(args, id)
	q := "UPDATE b_arrival_notice SET " + strings.Join(sets, ", ") + " WHERE id = ?"
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return ArrivalNotice{}, fmt.Errorf("修改预报失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_arrival_notice", EntityID: id, Action: "update",
		NewValue:    fmt.Sprintf("车序%02d 保持不变；%s", cur.SeqNo, reason),
		Reason:      reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return ArrivalNotice{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArrivalNotice{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetNotice(ctx, id)
}

// CancelNotice 取消预报 / 登记空号 —— ★ 序号作废但**不回收**（行保留，MAX 照常计入）。
func (s *Store) CancelNotice(ctx context.Context, id int64, status, reason string, actor MDActor) (ArrivalNotice, error) {
	if status != NoticeStatusCanceled && status != NoticeStatusVacant {
		return ArrivalNotice{}, fmt.Errorf("%w：status 只允许 %s / %s", ErrRecvBadInput, NoticeStatusCanceled, NoticeStatusVacant)
	}
	if strings.TrimSpace(reason) == "" {
		return ArrivalNotice{}, fmt.Errorf("%w：取消预报必须填写原因", ErrRecvBadInput)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ArrivalNotice{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	cur, err := scanNotice(tx.QueryRowContext(ctx, noticeSelect+` WHERE n.id = ? FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ArrivalNotice{}, fmt.Errorf("%w：预报单 #%d", ErrRecvNotFound, id)
	}
	if err != nil {
		return ArrivalNotice{}, err
	}
	if cur.Status != NoticeStatusForecast {
		return ArrivalNotice{}, fmt.Errorf("%w：当前状态「%s」，只有「%s」可取消", ErrNoticeState, cur.Status, NoticeStatusForecast)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE b_arrival_notice SET status = ? WHERE id = ?`, status, id); err != nil {
		return ArrivalNotice{}, fmt.Errorf("取消预报失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_arrival_notice", EntityID: id, Action: "status",
		Field: "status", OldValue: cur.Status, NewValue: status, Reason: reason,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return ArrivalNotice{}, err
	}
	if err := tx.Commit(); err != nil {
		return ArrivalNotice{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetNotice(ctx, id)
}

// ===== 到货确认（D3）=====

// ArriveInput 是到货确认入参：
//
//	NoticeID > 0 ⇒ 用已有预报确认；
//	Inline != nil ⇒ **无预报直接到货**：现场补录预报并立即确认（允许路径，不是异常）。
type ArriveInput struct {
	NoticeID int64        `json:"notice_id"`
	Inline   *NoticeInput `json:"notice"`
	ArriveAt *time.Time   `json:"arrive_at"`
	Remark   string       `json:"remark"`
}

// TruckLot 是车次的读模型。
type TruckLot struct {
	ID           int64      `json:"id"`
	Code         string     `json:"code"`
	Human        string     `json:"human"`
	NoticeID     int64      `json:"notice_id"`
	NoticeNo     string     `json:"notice_no"`
	CustomerID   int64      `json:"customer_id"`
	CustomerCode string     `json:"customer_code"`
	CustomerName string     `json:"customer_name"`
	MaterialID   int64      `json:"material_id"`
	MaterialCode string     `json:"material_code"`
	MaterialName string     `json:"material_name"`
	VehicleID    int64      `json:"vehicle_id"`
	PlateNo      string     `json:"plate_no"`
	Driver       string     `json:"driver"`
	Phone        string     `json:"phone"`
	BizType      string     `json:"biz_type"`
	ArriveDate   string     `json:"arrive_date"`
	ArriveAt     *time.Time `json:"arrive_at"`
	SeqNo        int        `json:"seq_no"`
	GrossWeight  *float64   `json:"gross_weight"`
	TareWeight   *float64   `json:"tare_weight"`
	NetWeight    *float64   `json:"net_weight"`
	BagCount     int        `json:"bag_count"`
	Status       string     `json:"status"`
	Remark       string     `json:"remark"`
	Operator     string     `json:"operator"`
	CreatedAt    *time.Time `json:"created_at"`
	CreatedBy    string     `json:"created_by"`
}

const truckSelect = `
SELECT t.id, t.code, t.notice_id, COALESCE(n.notice_no,''),
       t.customer_id, COALESCE(c.code,''), COALESCE(c.name,''),
       t.material_id, COALESCE(m.code,''), COALESCE(m.name,''),
       t.vehicle_id, COALESCE(n.plate_no,''), COALESCE(t.driver,''), COALESCE(t.phone,''),
       t.biz_type, t.arrive_date, t.arrive_at,
       COALESCE(n.seq_no, 0),
       t.gross_weight, t.tare_weight, t.net_weight, t.bag_count, t.status,
       COALESCE(t.remark,''), t.operator, t.created_at, t.created_by
FROM b_truck_lot t
LEFT JOIN b_arrival_notice n ON n.id = t.notice_id
LEFT JOIN m_customer c ON c.id = t.customer_id AND c.is_current = 1
LEFT JOIN m_material m ON m.id = t.material_id AND m.is_current = 1`

func scanTruck(sc interface{ Scan(...interface{}) error }) (TruckLot, error) {
	var t TruckLot
	var arriveDate sql.NullTime
	var arriveAt, createdAt sql.NullTime
	var gross, tare, net sql.NullFloat64
	var noticeID, vehicleID sql.NullInt64
	err := sc.Scan(&t.ID, &t.Code, &noticeID, &t.NoticeNo,
		&t.CustomerID, &t.CustomerCode, &t.CustomerName,
		&t.MaterialID, &t.MaterialCode, &t.MaterialName,
		&vehicleID, &t.PlateNo, &t.Driver, &t.Phone,
		&t.BizType, &arriveDate, &arriveAt,
		&t.SeqNo, &gross, &tare, &net, &t.BagCount, &t.Status,
		&t.Remark, &t.Operator, &createdAt, &t.CreatedBy)
	if err != nil {
		return TruckLot{}, err
	}
	t.NoticeID = noticeID.Int64
	t.VehicleID = vehicleID.Int64
	if arriveDate.Valid {
		t.ArriveDate = arriveDate.Time.Format("2006-01-02")
	}
	if arriveAt.Valid {
		v := arriveAt.Time
		t.ArriveAt = &v
	}
	if createdAt.Valid {
		v := createdAt.Time
		t.CreatedAt = &v
	}
	if gross.Valid {
		v := gross.Float64
		t.GrossWeight = &v
	}
	if tare.Valid {
		v := tare.Float64
		t.TareWeight = &v
	}
	if net.Valid {
		v := net.Float64
		t.NetWeight = &v
	}
	if p, err := codec.ToHuman(t.Code); err == nil {
		t.Human = p
	}
	return t, nil
}

// ConfirmArrival 到货确认：生成车次 + 车码 A（generation.A = 到货确认时）。
//
// ★ 支持无预报直接到货：ArriveInput.Inline 非空时，在**同一事务**里
//
//	先补录预报（照常分配车序）再确认。
func (s *Store) ConfirmArrival(ctx context.Context, in ArriveInput, actor MDActor) (TruckLot, error) {
	if in.NoticeID <= 0 && in.Inline == nil {
		return TruckLot{}, fmt.Errorf("%w：需要 notice_id（已有预报）或 notice（无预报直接到货）", ErrRecvBadInput)
	}

	var conn *sql.Conn
	var err error
	conn, err = s.db.Conn(ctx)
	if err != nil {
		return TruckLot{}, fmt.Errorf("获取连接失败: %w", err)
	}
	defer conn.Close()

	// 无预报直接到货 ⇒ 要取车序 ⇒ 同样要拿车序锁
	var lock string
	if in.Inline != nil {
		if err := validateNoticeInput(in.Inline); err != nil {
			return TruckLot{}, err
		}
		lock = seqLockName(in.Inline.CustomerID, in.Inline.MaterialID, in.Inline.ArriveDate)
		if err := acquireSeqLock(ctx, conn, lock); err != nil {
			return TruckLot{}, err
		}
		defer releaseSeqLock(conn, lock)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return TruckLot{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	noticeID := in.NoticeID
	if in.Inline != nil {
		seq, err := nextSeq(ctx, tx, in.Inline.CustomerID, in.Inline.MaterialID, in.Inline.ArriveDate)
		if err != nil {
			return TruckLot{}, err
		}
		noticeNo, err := newNoticeNo(in.Inline.ArriveDate, seq)
		if err != nil {
			return TruckLot{}, err
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO b_arrival_notice
  (notice_no, customer_id, material_id, vehicle_id, plate_no, driver, phone, eta,
   est_bag_count, biz_type, seq_no, arrive_date, status, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			noticeNo, in.Inline.CustomerID, in.Inline.MaterialID, nullInt(in.Inline.VehicleID),
			nullStr(in.Inline.PlateNo), nullStr(in.Inline.Driver), nullStr(in.Inline.Phone),
			in.Inline.ETA, in.Inline.EstBagCount, in.Inline.BizType, seq,
			in.Inline.ArriveDate, NoticeStatusForecast, actor.OpenID)
		if err != nil {
			return TruckLot{}, fmt.Errorf("补录预报失败: %w", err)
		}
		noticeID, _ = res.LastInsertId()
	}

	// 读预报（FOR UPDATE：并发下同一张预报只能确认一次）
	n, err := scanNotice(tx.QueryRowContext(ctx, noticeSelect+` WHERE n.id = ? FOR UPDATE`, noticeID))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：预报单 #%d", ErrRecvNotFound, noticeID)
	}
	if err != nil {
		return TruckLot{}, err
	}
	if n.Status != NoticeStatusForecast {
		return TruckLot{}, fmt.Errorf("%w：预报单 %s 当前状态「%s」，不能重复到货确认",
			ErrNoticeState, n.NoticeNo, n.Status)
	}

	custCode, matCode, err := lookupCodesTx(ctx, tx, n.CustomerID, n.MaterialID, n.BizType)
	if err != nil {
		return TruckLot{}, err
	}

	// ★ 车码 A：DATE = 链根日期（到货日），SEQ1 = 车序，SEQ2/SEQ3 = 000
	code, err := codec.Generate(codec.Segments{
		T:        "A",
		BT:       n.BizType,
		Customer: custCode,
		Material: matCode,
		Date:     yymmdd(n.ArriveDate),
		SEQ1:     pad(n.SeqNo, 2),
		SEQ2:     codec.Placeholder,
		SEQ3:     codec.Placeholder,
	})
	if err != nil {
		return TruckLot{}, fmt.Errorf("生成车码失败: %w", err)
	}

	arriveAt := time.Now()
	if in.ArriveAt != nil && !in.ArriveAt.IsZero() {
		arriveAt = *in.ArriveAt
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO b_truck_lot
  (code, notice_id, customer_id, material_id, vehicle_id, driver, phone, biz_type,
   arrive_date, arrive_at, status, remark, operator, created_by)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		code, noticeID, n.CustomerID, n.MaterialID, nullInt(n.VehicleID),
		nullStr(n.Driver), nullStr(n.Phone), n.BizType,
		n.ArriveDate, arriveAt, TruckStatusPending, nullStr(in.Remark), actor.Name, actor.OpenID)
	if err != nil {
		if isDuplicateErr(err) {
			return TruckLot{}, fmt.Errorf("%w：车码 %s 已存在", ErrRecvBadInput, code)
		}
		return TruckLot{}, fmt.Errorf("生成车次失败: %w", err)
	}
	truckID, _ := res.LastInsertId()

	if _, err := tx.ExecContext(ctx,
		`UPDATE b_arrival_notice SET status = ? WHERE id = ?`, NoticeStatusArrived, noticeID); err != nil {
		return TruckLot{}, fmt.Errorf("更新预报状态失败: %w", err)
	}
	if err := appendAuditTx(ctx, tx, audit.Entry{
		Entity: "b_truck_lot", EntityID: truckID, Action: "arrive_confirm",
		NewValue:    code,
		ActorOpenID: actor.OpenID, ActorName: actor.Name, ActorRole: actor.Role, IP: actor.IP,
	}); err != nil {
		return TruckLot{}, err
	}
	if err := tx.Commit(); err != nil {
		return TruckLot{}, fmt.Errorf("提交失败: %w", err)
	}
	return s.GetTruck(ctx, truckID)
}

// GetTruck 读单条车次。
func (s *Store) GetTruck(ctx context.Context, id int64) (TruckLot, error) {
	t, err := scanTruck(s.db.QueryRowContext(ctx, truckSelect+` WHERE t.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：车次 #%d", ErrRecvNotFound, id)
	}
	return t, err
}

// ListTrucks 列车次（?status= 过滤）。
func (s *Store) ListTrucks(ctx context.Context, status string) ([]TruckLot, error) {
	q := truckSelect
	if strings.TrimSpace(status) != "" {
		q += ` WHERE t.status = '` + strings.TrimSpace(status) + `'`
	}
	q += ` ORDER BY t.id DESC LIMIT 500`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("查询车次失败: %w", err)
	}
	defer rows.Close()
	out := []TruckLot{}
	for rows.Next() {
		t, err := scanTruck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ===== 取号与校验的内部工具 =====

func validateNoticeInput(in *NoticeInput) error {
	in.BizType = strings.ToUpper(strings.TrimSpace(in.BizType))
	if in.BizType == "" {
		in.BizType = BizTypeCG
	}
	if in.BizType != BizTypeCG && in.BizType != BizTypeZG {
		return fmt.Errorf("%w：biz_type 只允许 CG / ZG", ErrRecvBadInput)
	}
	if in.BizType == BizTypeCG && in.CustomerID <= 0 {
		return fmt.Errorf("%w：客供（CG）必须选择客户", ErrRecvBadInput)
	}
	if in.BizType == BizTypeZG && in.CustomerID < 0 {
		return fmt.Errorf("%w：customer_id 不能为负", ErrRecvBadInput)
	}
	if in.MaterialID <= 0 {
		return fmt.Errorf("%w：必须选择原料物料", ErrRecvBadInput)
	}
	if in.EstBagCount < 0 {
		return fmt.Errorf("%w：预计袋数不能为负", ErrRecvBadInput)
	}
	if _, err := time.Parse("2006-01-02", in.ArriveDate); err != nil {
		return fmt.Errorf("%w：arrive_date 必须是 YYYY-MM-DD（链根日期 = 车序空间的到货日）", ErrRecvBadInput)
	}
	return nil
}

// lookupCodes 校验主数据存在且**启用**（is_current=1 且 status=启用），返回 4 位编号。
func (s *Store) lookupCodes(ctx context.Context, customerID, materialID int64, bizType string) (custCode, matCode string, err error) {
	return lookupCodesCtx(ctx, s.db, customerID, materialID, bizType)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

func lookupCodesCtx(ctx context.Context, q queryer, customerID, materialID int64, bizType string) (string, string, error) {
	cust := "0000" // ★ 自购料（ZG）客户段填保留值 0000（spec#segments.CUSTOMER）
	if bizType != BizTypeZG {
		if customerID <= 0 {
			return "", "", fmt.Errorf("%w：客供（CG）必须选择客户", ErrRecvBadInput)
		}
		var code, status string
		var current int
		err := q.QueryRowContext(ctx,
			`SELECT code, status, is_current FROM m_customer WHERE id = ?`, customerID).
			Scan(&code, &status, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", fmt.Errorf("%w：客户 #%d", ErrRecvNotFound, customerID)
		}
		if err != nil {
			return "", "", fmt.Errorf("查询客户失败: %w", err)
		}
		if current != 1 || status != "启用" {
			return "", "", fmt.Errorf("%w：客户 #%d 已停用或不是当前版本", ErrRecvBadInput, customerID)
		}
		cust = code
	}
	var mcode, mstatus string
	var mcurrent int
	err := q.QueryRowContext(ctx,
		`SELECT code, status, is_current FROM m_material WHERE id = ? AND kind = '原料'`, materialID).
		Scan(&mcode, &mstatus, &mcurrent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w：原料物料 #%d", ErrRecvNotFound, materialID)
	}
	if err != nil {
		return "", "", fmt.Errorf("查询物料失败: %w", err)
	}
	if mcurrent != 1 || mstatus != "启用" {
		return "", "", fmt.Errorf("%w：物料 #%d 已停用或不是当前版本", ErrRecvBadInput, materialID)
	}
	return cust, mcode, nil
}

func lookupCodesTx(ctx context.Context, tx *sql.Tx, customerID, materialID int64, bizType string) (string, string, error) {
	return lookupCodesCtx(ctx, tx, customerID, materialID, bizType)
}

// nextSeq 取下一个车序（★ 必须在持锁的事务内调用）。
func nextSeq(ctx context.Context, tx *sql.Tx, customerID, materialID int64, arriveDate string) (int, error) {
	var max sql.NullInt64
	// 当前读：锁住该「客户+物料+到货日」范围内已有的最大行
	err := tx.QueryRowContext(ctx, `
SELECT seq_no FROM b_arrival_notice
WHERE customer_id = ? AND material_id = ? AND arrive_date = ?
ORDER BY seq_no DESC LIMIT 1 FOR UPDATE`,
		customerID, materialID, arriveDate).Scan(&max)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("读取车序失败: %w", err)
	}
	next := 1
	if max.Valid {
		next = int(max.Int64) + 1
	}
	if next > SeqMax {
		return 0, ErrSeqOverflow
	}
	return next, nil
}

// seqLockName 生成车序空间对应的 MySQL 命名锁名（≤64 字符）。
func seqLockName(customerID, materialID int64, arriveDate string) string {
	return fmt.Sprintf("jxlab.seq.%d.%d.%s", customerID, materialID, arriveDate)
}

// acquireSeqLock 在**本会话**上取命名锁（超时 ⇒ 明确报错，不静默重试）。
func acquireSeqLock(ctx context.Context, conn *sql.Conn, name string) error {
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 10)`, name).Scan(&got); err != nil {
		return fmt.Errorf("取车序锁失败: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return ErrSeqBusy
	}
	return nil
}

func releaseSeqLock(conn *sql.Conn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `SELECT RELEASE_LOCK(?)`, name)
}

// newNoticeNo 生成预报单号：YB + YYMMDD + 车序 + 6 位随机后缀（避免跨客户撞号）。
func newNoticeNo(arriveDate string, seq int) (string, error) {
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成预报单号失败: %w", err)
	}
	return fmt.Sprintf("YB%s%02d%s", strings.ReplaceAll(arriveDate, "-", "")[2:], seq, hex.EncodeToString(buf)), nil
}

// yymmdd 把 YYYY-MM-DD 转成码里的 6 位日期段。
func yymmdd(date string) string {
	d := strings.ReplaceAll(date, "-", "")
	if len(d) != 8 {
		return ""
	}
	return d[2:]
}

// pad 定宽补零。
func pad(n, width int) string {
	s := fmt.Sprintf("%d", n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

func nullStr(v string) interface{} {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullInt(v int64) interface{} {
	if v <= 0 {
		return nil
	}
	return v
}
