package store

// ===== N-029 · APP 首页聚合与收货域读助手（★全部只读）=====
//
// 口径：本文件只提供 SELECT 查询，**不含任何业务写入与状态机逻辑**。
// APP 薄层（httpapi/app.go）做鉴权与形状包装；一切**写路径**一律委托与 Web 端
// 同源的既有 store 方法（ConfirmArrival / ReturnTruck / WeighTruck /
// GenerateBags / PrintLabels），业务逻辑单一真相。
//
// ★ 新增只读聚合的理由（任务包 D1）：仓库原无首页 dashboard 类查询；
//	计数/摘要不得由 Web handler 里复制逻辑 ⇒ 只能落在这里，作为纯查询。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AppAggItem 是首页「进行中作业」某一项的最新一条摘要（只读投影，无业务判断）。
type AppAggItem struct {
	ID     int64  `json:"id"`
	Code   string `json:"code"`
	Title  string `json:"title"`
	Status string `json:"status"`
	At     string `json:"at"`
}

// AppAgg 是首页某一聚合项：进行中计数 + 最新一条摘要（Latest 无数据时为 nil）。
type AppAgg struct {
	Key    string      `json:"key"`
	Count  int         `json:"count"`
	Latest *AppAggItem `json:"latest"`
}

// AppTodos 是首页「今日待办」三计数（口径见 docs/APP-API-A-B.md §A.3）。
type AppTodos struct {
	// AwaitReceive 待收车数＝今日登记且仍「预报」的车次（系统无 ETA，
	// 以 created_at 当日为准 —— 见契约文档口径说明）。
	AwaitReceive int `json:"await_receive"`
	// AwaitLabel 待打码数＝已到货（有车次）但尚未生成袋码的车次。
	AwaitLabel int `json:"await_label"`
	// AwaitLoad 待装车数＝在库成品吨袋中、未被任何未撤销出货单归集的袋数。
	AwaitLoad int `json:"await_load"`
}

// appAggDef 一条聚合项的两条只读 SQL：count（单值）与 latest（最新一条四列投影）。
type appAggDef struct {
	key    string
	count  string
	latest string // SELECT id, code, title, status, at
}

// appAggDefs 聚合定义（key 与 httpapi 侧的标签/权限门一一对应，勿改 key 名）。
var appAggDefs = []appAggDef{
	{"notice_open",
		`SELECT COUNT(*) FROM b_arrival_notice WHERE status = '预报'`,
		`SELECT id, notice_no, CONCAT(IFNULL(plate_no,''), ' / ', IFNULL(driver,'')), status, created_at
		   FROM b_arrival_notice WHERE status = '预报' ORDER BY id DESC LIMIT 1`},
	{"batch_open",
		`SELECT COUNT(*) FROM m_incoming_batch WHERE status = '回货中'`,
		`SELECT id, our_batch_no, customer_batch_no, status, created_at
		   FROM m_incoming_batch WHERE status = '回货中' ORDER BY id DESC LIMIT 1`},
	{"bag_in_stock",
		`SELECT COUNT(*) FROM b_bag WHERE status = '在库'`,
		`SELECT id, code, CONCAT('袋序 ', bag_seq), status, created_at
		   FROM b_bag WHERE status = '在库' ORDER BY id DESC LIMIT 1`},
	{"truck_pending",
		`SELECT COUNT(*) FROM b_truck_lot WHERE status = '待检'`,
		`SELECT t.id, t.code, IFNULL(n.plate_no,''), t.status, t.created_at
		   FROM b_truck_lot t LEFT JOIN b_arrival_notice n ON n.id = t.notice_id
		  WHERE t.status = '待检' ORDER BY t.id DESC LIMIT 1`},
	{"prod_running",
		`SELECT COUNT(*) FROM b_production_batch WHERE status = '进行中'`,
		`SELECT id, code, DATE_FORMAT(batch_date, '%Y-%m-%d'), status, created_at
		   FROM b_production_batch WHERE status = '进行中' ORDER BY id DESC LIMIT 1`},
	{"fg_bag_ready",
		// ★ 「在库/已装吨袋」：成品袋 status='在库' 覆盖「待归集」与「已归集进未出场单」两种
		//	（出场登记才置「已出厂」；撤销单的袋回「在库」⇒ 仍计入）。
		`SELECT COUNT(*) FROM b_fg_bag WHERE status = '在库'`,
		`SELECT id, code, CONCAT('袋序 ', bag_seq), status, created_at
		   FROM b_fg_bag WHERE status = '在库' ORDER BY id DESC LIMIT 1`},
	{"ship_open",
		// ★ 未出厂出货单＝已建单（status='已出厂'）但未做出场登记（ship_at IS NULL）。
		`SELECT COUNT(*) FROM b_shipment WHERE status = '已出厂' AND ship_at IS NULL`,
		`SELECT id, shipment_no, CONCAT(IFNULL(plate_no,''), ' / ', IFNULL(driver,'')),
		        status, created_at
		   FROM b_shipment WHERE status = '已出厂' AND ship_at IS NULL
		  ORDER BY id DESC LIMIT 1`},
}

// AppHomeAggregates 返回全部聚合项（key 顺序固定 = appAggDefs）。
// 可见性过滤（权限门）在 httpapi 层做 —— 权限不属于 store。
func (s *Store) AppHomeAggregates(ctx context.Context) ([]AppAgg, error) {
	out := make([]AppAgg, 0, len(appAggDefs))
	for _, d := range appAggDefs {
		var agg AppAgg
		agg.Key = d.key
		if err := s.db.QueryRowContext(ctx, d.count).Scan(&agg.Count); err != nil {
			return nil, fmt.Errorf("首页聚合计数(%s)失败: %w", d.key, err)
		}
		item, err := s.appAggLatest(ctx, d.key, d.latest)
		if err != nil {
			return nil, err
		}
		agg.Latest = item
		out = append(out, agg)
	}
	return out, nil
}

// appAggLatest 执行 latest SQL；无行 ⇒ (nil, nil)。
func (s *Store) appAggLatest(ctx context.Context, key, q string) (*AppAggItem, error) {
	var it AppAggItem
	var at sql.NullTime
	err := s.db.QueryRowContext(ctx, q).Scan(&it.ID, &it.Code, &it.Title, &it.Status, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("首页聚合最新一条(%s)失败: %w", key, err)
	}
	if at.Valid {
		it.At = at.Time.Format("2006-01-02 15:04:05")
	}
	return &it, nil
}

// AppHomeTodos 一次快照查出「今日待办」三计数。
func (s *Store) AppHomeTodos(ctx context.Context) (AppTodos, error) {
	var t AppTodos
	err := s.db.QueryRowContext(ctx, `
SELECT
  (SELECT COUNT(*) FROM b_arrival_notice
    WHERE status = '预报' AND DATE(created_at) = CURDATE()),
  (SELECT COUNT(*) FROM b_truck_lot
    WHERE bag_count = 0 AND status NOT IN ('已退货', '已作废')),
  (SELECT COUNT(*) FROM b_fg_bag b
    WHERE b.status = '在库'
      AND NOT EXISTS (SELECT 1 FROM b_shipment_item i
                        JOIN b_shipment s ON s.id = i.shipment_id
                       WHERE i.fg_bag_id = b.id AND s.status <> '已撤销'))`).
		Scan(&t.AwaitReceive, &t.AwaitLabel, &t.AwaitLoad)
	if err != nil {
		return AppTodos{}, fmt.Errorf("首页待办计数失败: %w", err)
	}
	return t, nil
}

// TruckByNoticeID 按预报 id 定位车次（APP 退车按 notice_id 入参 ⇒ 需要这一步只读映射）。
// 一预报一车次（ConfirmArrival 语义）；取 id 最大的一条防脏数据重号。
func (s *Store) TruckByNoticeID(ctx context.Context, noticeID int64) (TruckLot, error) {
	t, err := scanTruck(s.db.QueryRowContext(ctx,
		truckSelect+` WHERE t.notice_id = ? ORDER BY t.id DESC LIMIT 1`, noticeID))
	if errors.Is(err, sql.ErrNoRows) {
		return TruckLot{}, fmt.Errorf("%w：预报 #%d 尚无车次（未到货）", ErrRecvNotFound, noticeID)
	}
	return t, err
}

// TareHistory 是「该车历史皮重」参考值（APP 过磅预填；不参与净重计算以外的任何判断）。
type TareHistory struct {
	TruckID    int64   `json:"truck_id"`
	Tare       float64 `json:"tare_weight"`
	ArriveDate string  `json:"arrive_date"`
}

// LastTareFor 取同一辆车**最近一次**有皮重的过磅记录（APP 过磅「皮重取该车历史」）。
//
// ★ 口径（如实声明）：系统原无历史皮重查询机制，本方法为 N-029 新增的**只读**查询；
//
//	匹配顺序＝① 同 vehicle_id（车辆档案关联）② 同车牌（经预报单）；
//	selfTruckID 为当前车次自身（排除自己）。净重仍完全由 WeighTruck 计算（毛 − 皮）。
//
//	无历史 ⇒ (nil, nil) —— 由调用方决定（APP 侧要求人工录入皮重）。
func (s *Store) LastTareFor(ctx context.Context, selfTruckID, vehicleID int64, plateNo string) (*TareHistory, error) {
	cond := ``
	var args []interface{}
	switch {
	case vehicleID > 0:
		cond = `t.vehicle_id = ?`
		args = append(args, vehicleID)
	case plateNo != "":
		cond = `n.plate_no = ?`
		args = append(args, plateNo)
	default:
		return nil, nil // 无车辆档案也无车牌 ⇒ 无从匹配历史
	}
	q := `SELECT t.id, t.tare_weight, DATE_FORMAT(t.arrive_date, '%Y-%m-%d')
	        FROM b_truck_lot t
	        LEFT JOIN b_arrival_notice n ON n.id = t.notice_id
	       WHERE t.tare_weight IS NOT NULL AND t.id <> ? AND ` + cond +
		` ORDER BY t.id DESC LIMIT 1`
	args = append([]interface{}{selfTruckID}, args...)

	var h TareHistory
	var tare float64
	var date string
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&h.TruckID, &tare, &date)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询历史皮重失败: %w", err)
	}
	h.Tare = tare
	h.ArriveDate = date
	return &h, nil
}
