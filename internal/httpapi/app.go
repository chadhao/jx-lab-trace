package httpapi

// ===== N-029 · APP 业务 API 批次 A+B（/api/app/* 薄层）=====
//
// ★★ 架构红线（任务包 §6-1）：本文件的 handler 只做
//	「鉴权（路由守卫）＋ 入参校验 ＋ 调 store ＋ 形状包装」，业务逻辑与 Web 端
//	**同源**（同一 store 方法）：
//
//	GET  /recv/notices        → store.ListNotices      （同 Web GET  /api/recv/notices）
//	POST /recv/arrive-confirm → store.ConfirmArrival   （同 Web POST /api/recv/arrivals：27 位车次码 / 链根日期机制一字不改）
//	POST /recv/return         → store.ReturnTruck      （同 Web POST /api/recv/trucks/:id/return）
//	POST /recv/weigh          → store.WeighTruck       （同 Web POST /api/recv/trucks/:id/weigh：净重＝毛−皮，实测口径不变）
//	POST /recv/bag-gen        → store.GenerateBags     （同 Web POST /api/recv/trucks/:id/bags：袋序/摊算/吨袋码由 store 分配）
//	POST /label/reprint       → store.PrintLabels      （同 Web POST /api/recv/labels/reprint）
//	GET  /home                → store.AppHomeAggregates / AppHomeTodos / ListAuditByActor（★只读聚合）
//
// ★ 路由前缀 /api/app/*（与 Web 端 /api/* 并列；**不复用 Web 路由** ⇒ 字段形状不耦合）。
// ★ 不新增权限点：六动作逐点挂既有 recv.* 权限点（与 Web 端同点同级）；
//	home 是跨域只读聚合 ⇒ 登录必达（RequireLogin），**按域**判 READ 可见性。
// ★ 结构化拦截（D0）：无预报 / 无车辆档案 / 无权限 三类 + 其余 4xx，
//	统一 {"error","reason","owner","contact"}（映射表 appOwners；Web 端错误结构不变）。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// ===== 结构化拦截（D0）=====

// appOwners —— owner / contact 映射表（★ 与 docs/APP-API-A-B.md §E 逐行同步）。
const (
	ownerSession   = "本人 / 系统管理员（登录会话）"
	contactSession = "在飞书内重新打开本应用登录；仍失败联系系统管理员"
	ownerPerm      = "系统管理员（权限配置）"
	contactPerm    = "飞书联系系统管理员，请其在 Web「后台 › 权限矩阵」按角色调整"
	ownerNotice    = "收货文员（预报登记）"
	contactNotice  = "飞书联系收货文员在 Web「收货 › 预报登记」补录预报后再操作"
	ownerVehicle   = "主数据维护（车辆档案）"
	contactVehicle = "飞书联系主数据维护在 Web「主数据 › 车辆档案」补建档案"
	ownerRecv      = "收货文员（收货与打码）"
	contactRecv    = "飞书联系收货文员按页面提示处理"
)

// appBlock 构造结构化拦截（★ 四字段固定，jsonErrorHandler 的 map 通道原样透传）。
func appBlock(status int, errText, reason, owner, contact string) error {
	return echo.NewHTTPError(status, map[string]interface{}{
		"error":   errText,
		"reason":  reason,
		"owner":   owner,
		"contact": contact,
	})
}

// isAppBlock 判定 error message 是否已是四字段结构（避免二次包装）。
func isAppBlock(msg interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	_, hasOwner := m["owner"]
	_, hasReason := m["reason"]
	return hasOwner && hasReason
}

// appIntercept 挂在 /api/app 组**最外层**：把守卫抛出的 401/403 改写成 D0 结构化拦截。
// ★ Web 路由不挂本中间件 ⇒ Web 端错误结构（{error} / {error,point,need,hint}）零改动。
func appIntercept() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			he, ok := err.(*echo.HTTPError)
			if !ok {
				return err
			}
			switch he.Code {
			case http.StatusUnauthorized:
				return appBlock(http.StatusUnauthorized, "未登录",
					"会话已过期或尚未登录，请重新登录后再试", ownerSession, contactSession)
			case http.StatusForbidden:
				if isAppBlock(he.Message) {
					return err
				}
				return appBlock(http.StatusForbidden, "无权限",
					"当前账号在该动作上没有权限（角色未授予该权限点，或级别不足；deny by default）",
					ownerPerm, contactPerm)
			default:
				return err
			}
		}
	}
}

// appReasonOf 给 Web 侧已判定的状态码配一句 APP 端「人话原因」。
func appReasonOf(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "入参不合法：请检查必填字段、取值格式与数量上限"
	case http.StatusNotFound:
		return "对象不存在：编号可能已作废、被删除或从未登记；请回列表刷新后重试"
	case http.StatusConflict:
		return "当前状态不允许该动作：请按流程顺序推进（预报 → 到货 → 过磅 → 打码 → 检测）"
	default:
		return "操作被拒绝：请按页面提示处理"
	}
}

// appErr 复用 **Web 侧 recvErr 的错误分类**（同一真相）再包成结构化四字段；
// 分类不到的（5xx / 未知）原样上交统一处理器（5xx 不外泄内部细节）。
func appErr(err error, owner, contact string) error {
	mapped := recvErr(err)
	he, ok := mapped.(*echo.HTTPError)
	if !ok {
		return mapped
	}
	text := fmt.Sprint(he.Message)
	if nested, isMap := he.Message.(map[string]interface{}); isMap {
		if s, hasErr := nested["error"].(string); hasErr {
			text = s
		}
	}
	return appBlock(he.Code, text, appReasonOf(he.Code), owner, contact)
}

// appReadBody 读原始请求体（入参校验属薄层职责）。
func appReadBody(c echo.Context) ([]byte, error) {
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, appBlock(http.StatusBadRequest, "读取请求体失败",
			"请求体读取失败，请检查网络后重试", ownerRecv, contactRecv)
	}
	return raw, nil
}

// appDecodeBody 读取并 JSON 校验请求体（空体 ⇒ 零值；不合法 ⇒ 结构化 400）。
func appDecodeBody(c echo.Context, out interface{}, owner, contact string) error {
	raw, err := appReadBody(c)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return appBlock(http.StatusBadRequest, "请求体不是合法 JSON",
			"入参必须是合法 JSON 对象（Content-Type: application/json）", owner, contact)
	}
	return nil
}

// ===== 路由装配 =====

// mountApp 装配 /api/app/* 的受保护入口（★ 集中声明）。
//
// ★★ 每条路由直接写出守卫调用（scripts/check_perm_registry.py 判据③：
//
//	实参必须是 permission.XXX 常量，传变量 ⇒ 假红）。
func (s *Server) mountApp(e *echo.Echo) {
	g := e.Group("/api/app", appIntercept())

	// —— 批次 A：首页聚合（跨域只读；登录必达，可见性按域判 READ ⇒ 不挂单一权限点）——
	g.GET("/home", s.handleAppHome, access.RequireLogin())

	// —— 批次 B：收货域六动作（权限点与 Web 端同点同级）——
	g.GET("/recv/notices", s.handleAppNotices,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelRead))
	g.POST("/recv/arrive-confirm", s.handleAppArriveConfirm,
		access.RequirePerm(s.Store, permission.RecvArriveConfirm, access.LevelAll))
	g.POST("/recv/return", s.handleAppReturn,
		access.RequirePerm(s.Store, permission.RecvReturn, access.LevelAll))
	g.POST("/recv/weigh", s.handleAppWeigh,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelAll))
	g.POST("/recv/bag-gen", s.handleAppBagGen,
		access.RequirePerm(s.Store, permission.RecvBagGen, access.LevelAll))
	g.POST("/label/reprint", s.handleAppLabelReprint,
		access.RequirePerm(s.Store, permission.RecvLabelReprint, access.LevelAll))
}

// ===== 批次 A · GET /api/app/home =====

type appMeta struct {
	label  string
	domain string
}

// appAggOrder / appTodoOrder：响应里各聚合项的固定顺序（形状稳定、可逐条断言）。
var appAggOrder = []string{
	"notice_open", "batch_open", "bag_in_stock",
	"truck_pending", "prod_running", "fg_bag_ready", "ship_open",
}

var appTodoOrder = []string{"await_receive", "await_label", "await_load"}

// appAggMeta / appTodoMeta：key →（中文标签 / 所属域）。
var appAggMeta = map[string]appMeta{
	"notice_open":   {"预报中车次", "recv"},
	"batch_open":    {"回货中批次", "recv"},
	"bag_in_stock":  {"在库吨袋", "recv"},
	"truck_pending": {"待检车次", "insp"},
	"prod_running":  {"进行中生产批", "prod"},
	"fg_bag_ready":  {"在库/已装吨袋", "ship"},
	"ship_open":     {"未出厂出货单", "ship"},
}

var appTodoMeta = map[string]appMeta{
	"await_receive": {"待收车数", "recv"},
	"await_label":   {"待打码数", "recv"},
	"await_load":    {"待装车数", "ship"},
}

// appAggGate / appTodoGate：key → 现场权限点（READ 级即可见；★ 不新增权限点）。
var appAggGate = map[string]permission.Code{
	"notice_open":   permission.RecvNoticeCreate,
	"batch_open":    permission.RecvNoticeCreate,
	"bag_in_stock":  permission.RecvBagGen,
	"truck_pending": permission.InspTaskView,
	"prod_running":  permission.ProdBatchCreate,
	"fg_bag_ready":  permission.ShipLoadScan,
	"ship_open":     permission.ShipLoadScan,
}

var appTodoGate = map[string]permission.Code{
	"await_receive": permission.RecvNoticeCreate,
	"await_label":   permission.RecvBagGen,
	"await_load":    permission.ShipLoadScan,
}

type appHomeItem struct {
	Key    string            `json:"key"`
	Label  string            `json:"label"`
	Domain string            `json:"domain"`
	Count  int               `json:"count"`
	Latest *store.AppAggItem `json:"latest"`
}

type appHomeTodo struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Domain string `json:"domain"`
	Count  int    `json:"count"`
}

type appRecentItem struct {
	ID          int64  `json:"id"`
	At          string `json:"at"`
	Entity      string `json:"entity"`
	EntityID    int64  `json:"entity_id"`
	Action      string `json:"action"`
	Field       string `json:"field"`
	NewValue    string `json:"new_value"`
	Reason      string `json:"reason"`
	ActorOpenID string `json:"actor_open_id"`
	Actor       string `json:"actor"`
}

// appVisible 判定登录人在某现场权限点上是否有 READ 级（首页按域过滤用）。
func (s *Server) appVisible(ctx context.Context, openID string, point permission.Code) (bool, error) {
	lv, err := s.Store.LevelsFor(ctx, openID, point)
	if err != nil {
		return false, err
	}
	return lv.Allows(access.LevelRead), nil
}

// handleAppHome 首页聚合：进行中作业 + 今日待办 + 最近作业（全部只读）。
func (s *Server) handleAppHome(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil { // RequireLogin 已挡；保留为薄层防御（避免 nil 解引用）
		return appBlock(http.StatusUnauthorized, "未登录",
			"会话已过期或尚未登录，请重新登录后再试", ownerSession, contactSession)
	}
	ctx := c.Request().Context()

	aggs, err := s.Store.AppHomeAggregates(ctx)
	if err != nil {
		return err
	}
	todos, err := s.Store.AppHomeTodos(ctx)
	if err != nil {
		return err
	}
	recent, err := s.Store.ListAuditByActor(ctx, p.OpenID, 10)
	if err != nil {
		return err
	}

	inProgress := make([]appHomeItem, 0, len(appAggOrder))
	domainSet := map[string]bool{}
	for _, agg := range aggs {
		point, ok := appAggGate[agg.Key]
		meta := appAggMeta[agg.Key]
		if !ok || meta.label == "" {
			continue
		}
		visible, err := s.appVisible(ctx, p.OpenID, point)
		if err != nil {
			return err
		}
		if !visible {
			continue
		}
		domainSet[meta.domain] = true
		inProgress = append(inProgress, appHomeItem{
			Key: agg.Key, Label: meta.label, Domain: meta.domain,
			Count: agg.Count, Latest: agg.Latest,
		})
	}

	todoCount := map[string]int{
		"await_receive": todos.AwaitReceive,
		"await_label":   todos.AwaitLabel,
		"await_load":    todos.AwaitLoad,
	}
	todoList := make([]appHomeTodo, 0, len(appTodoOrder))
	for _, key := range appTodoOrder {
		point, ok := appTodoGate[key]
		meta := appTodoMeta[key]
		if !ok || meta.label == "" {
			continue
		}
		visible, err := s.appVisible(ctx, p.OpenID, point)
		if err != nil {
			return err
		}
		if !visible {
			continue
		}
		domainSet[meta.domain] = true
		todoList = append(todoList, appHomeTodo{
			Key: key, Label: meta.label, Domain: meta.domain, Count: todoCount[key],
		})
	}

	domains := make([]string, 0, len(domainSet))
	for d := range domainSet {
		domains = append(domains, d)
	}
	sort.Strings(domains)

	recentItems := make([]appRecentItem, 0, len(recent))
	for _, r := range recent {
		recentItems = append(recentItems, appRecentItem{
			ID: r.ID, At: r.At, Entity: r.Entity, EntityID: r.EntityID,
			Action: r.Action, Field: r.Field, NewValue: r.NewValue,
			Reason: r.Reason, ActorOpenID: r.ActorOpenID, Actor: r.ActorName,
		})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"me": map[string]interface{}{
			"open_id": p.OpenID, "name": p.Name, "roles": p.AllRoles(),
		},
		"domains":       domains,
		"in_progress":   inProgress,
		"todos":         todoList,
		"recent":        recentItems,
		"recent_source": "s_audit_log（actor=登录人，按 id 倒序取 10 条）",
		"generated_at":  time.Now().Format(time.RFC3339),
	})
}

// ===== 批次 B · 收货域六动作 =====

// appNotice 是 APP 端预报行（字段形状独立于 Web 端 ArrivalNotice 的 JSON）。
type appNotice struct {
	NoticeID        int64  `json:"notice_id"`
	NoticeNo        string `json:"notice_no"`
	BatchID         int64  `json:"batch_id"`
	OurBatchNo      string `json:"our_batch_no"`
	CustomerBatchNo string `json:"customer_batch_no"`
	CustomerName    string `json:"customer_name"`
	MaterialName    string `json:"material_name"`
	SeqNo           int    `json:"seq_no"`
	PlateNo         string `json:"plate_no"`
	Driver          string `json:"driver"`
	Phone           string `json:"phone"`
	Status          string `json:"status"`
	ArriveDate      string `json:"arrive_date"`
	Legacy          bool   `json:"legacy"`
}

// handleAppNotices 预报列表：mode 缺省=全部；open=预报中；pending=L2（批次预报，排除旧版）。
// 与 Web 同源：store.ListNotices 的 mode 语义一字不差。
func (s *Server) handleAppNotices(c echo.Context) error {
	mode := ""
	switch c.QueryParam("mode") {
	case "open":
		mode = "open"
	case "pending":
		mode = "pending"
	}
	rows, err := s.Store.ListNotices(c.Request().Context(), mode)
	if err != nil {
		return appErr(err, ownerNotice, contactNotice)
	}
	notices := make([]appNotice, 0, len(rows))
	for _, n := range rows {
		notices = append(notices, appNotice{
			NoticeID: n.ID, NoticeNo: n.NoticeNo, BatchID: n.BatchID,
			OurBatchNo: n.OurBatchNo, CustomerBatchNo: n.CustomerBatchNo,
			CustomerName: n.CustomerName, MaterialName: n.MaterialName,
			SeqNo: n.SeqNo, PlateNo: n.PlateNo, Driver: n.Driver, Phone: n.Phone,
			Status: n.Status, ArriveDate: n.ArriveDate, Legacy: n.Legacy,
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(notices), "notices": notices,
	})
}

// handleAppArriveConfirm 到货确认：入参 notice_id；与 Web /api/recv/arrivals 同一 store 调用链。
func (s *Server) handleAppArriveConfirm(c echo.Context) error {
	var in struct {
		NoticeID int64  `json:"notice_id"`
		Remark   string `json:"remark"`
	}
	if err := appDecodeBody(c, &in, ownerNotice, contactNotice); err != nil {
		return err
	}
	if in.NoticeID <= 0 {
		return appBlock(http.StatusBadRequest, "缺少 notice_id",
			"入参必须带预报单 id（APP 端到货确认只走已有预报，不做无预报直达到货）",
			ownerNotice, contactNotice)
	}
	ctx := c.Request().Context()

	notice, err := s.Store.GetNotice(ctx, in.NoticeID)
	if errors.Is(err, store.ErrRecvNotFound) {
		return appBlock(http.StatusNotFound, "无预报",
			"该预报单不存在或已取消，无法确认到货", ownerNotice, contactNotice)
	}
	if err != nil {
		return appErr(err, ownerNotice, contactNotice)
	}
	// D0「无车辆档案」拦截：预报引用了车辆档案 id，但主数据中没有该行。
	// ★ 只读前置校验，仅在 APP 端生效；Web 端行为不变（本就不校验车辆档案）。
	if notice.VehicleID > 0 {
		if _, err := s.Store.MDGet(ctx, "vehicles", notice.VehicleID); err != nil {
			if errors.Is(err, store.ErrMDRowNotFound) {
				return appBlock(http.StatusNotFound, "无车辆档案",
					"预报引用的车辆档案在主数据中不存在，无法生成车次码", ownerVehicle, contactVehicle)
			}
			return appErr(err, ownerVehicle, contactVehicle)
		}
	}

	row, err := s.Store.ConfirmArrival(ctx,
		store.ArriveInput{NoticeID: in.NoticeID, Remark: in.Remark}, mdActor(c))
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"confirmed":    true,
		"truck_lot_id": row.ID,
		"truck_code":   row.Code,
		"truck_human":  row.Human,
		"notice_id":    row.NoticeID,
		"arrive_date":  row.ArriveDate,
		"seq_no":       row.SeqNo,
		"plate_no":     row.PlateNo,
		"status":       row.Status,
	})
}

// handleAppReturn 退车：入参 notice_id（APP 契约）⇒ 先只读定位车次，再委托同一 store 方法。
func (s *Server) handleAppReturn(c echo.Context) error {
	var in struct {
		NoticeID int64  `json:"notice_id"`
		Reason   string `json:"reason"`
	}
	if err := appDecodeBody(c, &in, ownerRecv, contactRecv); err != nil {
		return err
	}
	if in.NoticeID <= 0 {
		return appBlock(http.StatusBadRequest, "缺少 notice_id",
			"入参必须带预报单 id（APP 端退车按预报定位车次）", ownerNotice, contactNotice)
	}
	ctx := c.Request().Context()

	if _, err := s.Store.GetNotice(ctx, in.NoticeID); err != nil {
		if errors.Is(err, store.ErrRecvNotFound) {
			return appBlock(http.StatusNotFound, "无预报",
				"该预报单不存在或已取消，无法退车", ownerNotice, contactNotice)
		}
		return appErr(err, ownerNotice, contactNotice)
	}
	truck, err := s.Store.TruckByNoticeID(ctx, in.NoticeID)
	if errors.Is(err, store.ErrRecvNotFound) {
		return appBlock(http.StatusConflict, err.Error(),
			"该预报尚未到货（还没有车次），无法退车", ownerRecv, contactRecv)
	}
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}

	row, err := s.Store.ReturnTruck(ctx, truck.ID, in.Reason, mdActor(c))
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"returned":     true,
		"truck_lot_id": row.ID,
		"truck_code":   row.Code,
		"notice_id":    row.NoticeID,
		"status":       row.Status,
		"bag_count":    row.BagCount,
	})
}

// handleAppWeigh 过磅：毛重必填；皮重可省略 ⇒ 取该车历史（store.LastTareFor，只读）。
// 净重＝毛−皮，仍由 store.WeighTruck 计算（实测口径不变）。
func (s *Server) handleAppWeigh(c echo.Context) error {
	raw, err := appReadBody(c)
	if err != nil {
		return err
	}
	var in struct {
		TruckLotID int64 `json:"truck_lot_id"`
		store.WeighInput
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return appBlock(http.StatusBadRequest, "请求体不是合法 JSON",
				"入参必须是合法 JSON 对象（Content-Type: application/json）", ownerRecv, contactRecv)
		}
	}
	if in.TruckLotID <= 0 {
		return appBlock(http.StatusBadRequest, "缺少 truck_lot_id",
			"入参必须带车次 id（truck_lot_id）", ownerRecv, contactRecv)
	}

	var probe map[string]json.RawMessage
	_ = json.Unmarshal(raw, &probe)
	_, hasTare := probe["tare_weight"]

	ctx := c.Request().Context()
	tareSource := "input"
	var tareFrom *store.TareHistory
	if !hasTare {
		// ★ 皮重取该车历史（任务包 D2）：历史皮重查询是 N-029 新增**只读**查询，
		//	匹配同车辆档案 / 同车牌的最近一次过磅；无历史 ⇒ 要求人工录入，不猜值。
		truck, err := s.Store.GetTruck(ctx, in.TruckLotID)
		if err != nil {
			return appErr(err, ownerRecv, contactRecv)
		}
		hist, err := s.Store.LastTareFor(ctx, truck.ID, truck.VehicleID, truck.PlateNo)
		if err != nil {
			return err
		}
		if hist == nil {
			return appBlock(http.StatusBadRequest, "缺少 tare_weight",
				"该车无历史皮重可用（系统不自动估重），请人工录入皮重后再过磅",
				ownerRecv, contactRecv)
		}
		payload, _ := json.Marshal(map[string]float64{"tare_weight": hist.Tare})
		if err := json.Unmarshal(payload, &in.WeighInput); err != nil {
			return err
		}
		tareSource = "history"
		tareFrom = hist
	}

	row, err := s.Store.WeighTruck(ctx, in.TruckLotID, in.WeighInput, mdActor(c))
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"weighed":      true,
		"truck_lot_id": row.ID,
		"truck_code":   row.Code,
		"gross_weight": row.GrossWeight,
		"tare_weight":  row.TareWeight,
		"net_weight":   row.NetWeight,
		"tare_source":  tareSource,
		"tare_history": tareFrom,
		"status":       row.Status,
	})
}

// appBag 是 APP 端袋行投影。
type appBag struct {
	BagSeq            int      `json:"bag_seq"`
	Code              string   `json:"code"`
	Human             string   `json:"human"`
	WeightAllocated   *float64 `json:"weight_allocated"`
	WeightIsAllocated int      `json:"weight_is_allocated"`
	Status            string   `json:"status"`
}

// handleAppBagGen 建袋打码：入参 truck_lot_id + count；袋序/摊算重量/吨袋码全部由 store 分配。
func (s *Server) handleAppBagGen(c echo.Context) error {
	var in struct {
		TruckLotID int64 `json:"truck_lot_id"`
		Count      int   `json:"count"`
	}
	if err := appDecodeBody(c, &in, ownerRecv, contactRecv); err != nil {
		return err
	}
	if in.TruckLotID <= 0 {
		return appBlock(http.StatusBadRequest, "缺少 truck_lot_id",
			"入参必须带车次 id（truck_lot_id）", ownerRecv, contactRecv)
	}
	if in.Count <= 0 {
		return appBlock(http.StatusBadRequest, "缺少 count",
			"入参必须带袋数 count（1~999，由后端分配袋序）", ownerRecv, contactRecv)
	}
	rows, err := s.Store.GenerateBags(c.Request().Context(), in.TruckLotID, in.Count, mdActor(c))
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}
	bags := make([]appBag, 0, len(rows))
	for _, b := range rows {
		bags = append(bags, appBag{
			BagSeq: b.BagSeq, Code: b.Code, Human: b.Human,
			WeightAllocated: b.WeightAllocated, WeightIsAllocated: b.WeightIsAllocated,
			Status: b.Status,
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"generated":    len(bags),
		"truck_lot_id": in.TruckLotID,
		"bags":         bags,
	})
}

// appLabel 是 APP 端打印记录投影。
type appLabel struct {
	ID        int64  `json:"label_id"`
	Code      string `json:"code"`
	Human     string `json:"human"`
	Kind      string `json:"kind"`
	IsReprint int    `json:"is_reprint"`
	Reason    string `json:"reason"`
	PrintedBy string `json:"printed_by"`
}

// handleAppLabelReprint 标签补打：入参 bag_code（必填）+ reason（补打必填，store 校验）。
func (s *Server) handleAppLabelReprint(c echo.Context) error {
	var in struct {
		BagCode string `json:"bag_code"`
		Reason  string `json:"reason"`
	}
	if err := appDecodeBody(c, &in, ownerRecv, contactRecv); err != nil {
		return err
	}
	code := strings.TrimSpace(in.BagCode)
	if code == "" {
		return appBlock(http.StatusBadRequest, "缺少 bag_code",
			"入参必须带袋号 bag_code（27 位吨袋码）", ownerRecv, contactRecv)
	}
	items, err := s.Store.PrintLabels(c.Request().Context(),
		store.LabelPrintInput{
			Codes:   []string{code},
			Reprint: true,
			Reason:  strings.TrimSpace(in.Reason),
		}, mdActor(c))
	if err != nil {
		return appErr(err, ownerRecv, contactRecv)
	}
	labels := make([]appLabel, 0, len(items))
	ids := make([]string, 0, len(items))
	for _, it := range items {
		labels = append(labels, appLabel{
			ID: it.ID, Code: it.Code, Human: it.Human, Kind: it.Kind,
			IsReprint: it.IsReprint, Reason: it.Reason, PrintedBy: it.PrintedBy,
		})
		ids = append(ids, fmt.Sprint(it.ID))
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count":    len(labels),
		"labels":   labels,
		"page_url": "/api/recv/labels/page?ids=" + strings.Join(ids, ","),
	})
}
