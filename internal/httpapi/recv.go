package httpapi

// ===== M3 收货与打码 · 接口（D2~D6）=====
//
// ★★ 8 个 recv.* 权限点全部在本包的**路由表**（mountReceiving）里，以
//	access 守卫 ＋ 权限点常量（permission.RecvXxx）的形式消费，
//	禁裸写权限点字符串（TC-M0-08 门禁判据③）。
//
// 权限点 → 入口：
//	recv.notice.create    预报登记（POST）· 预报列表（GET，READ 级）
//	recv.notice.edit      预报修改（PUT）· 取消 / 空号（PATCH）
//	recv.arrive.confirm   到货确认（含无预报直接到货）
//	recv.weigh            过磅录入（POST）· 车次 / 袋查询（GET，READ 级）
//	recv.bag.gen          袋数录入 + 批量生成袋码（POST）· 袋作废（POST）
//	recv.label.print      标签打印（POST）· 版式页 / 二维码 / 打印历史（GET，READ 级）
//	recv.label.reprint    标签补打（POST，必填原因）
//	recv.return           退车执行登记（POST）

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// mountReceiving 装配 /api/recv/* 的受保护入口（★ 集中声明）。
//
// ★★ 每条路由都**直接写出守卫调用**（不封装成传变量的 helper）：
//
//	scripts/check_perm_registry.py 判据③ 要求实参能静态核验成已登记的权限点常量，
//	传变量会被判「无法静态核验」⇒ 假红。宁可写长，不可写活。
//
// ★ N-023 权限（不新增权限点，沿用收货域）：
//
//	批次预填/列表/查询 = 预报域动作 ⇒ recv.notice.create（ALL / READ）
//	批次关闭 = 预报域管理动作（决定该批次能否继续预报）⇒ recv.notice.edit（ALL）
//	★ GET 列表一律 READ 级（N-021 教训：recv.notice.create 自带 READ 级别，
//	  与既有 GET /notices 同点同级 —— 动作型权限点不以 ALL 级守 GET 列表）。
func (s *Server) mountReceiving(e *echo.Echo) {
	g := e.Group("/api/recv")

	// —— D2 预报登记与车序分配 ——
	g.GET("/notices", s.handleListNotices,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelRead))
	g.POST("/notices", s.handleCreateNotice,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelAll))
	g.PUT("/notices/:id", s.handleUpdateNotice,
		access.RequirePerm(s.Store, permission.RecvNoticeEdit, access.LevelAll))
	g.PATCH("/notices/:id/status", s.handleNoticeStatus,
		access.RequirePerm(s.Store, permission.RecvNoticeEdit, access.LevelAll))

	// —— N-023 来料批次（D2）——
	g.POST("/batches", s.handleCreateIncomingBatch,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelAll))
	g.GET("/batches", s.handleListIncomingBatches,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelRead))
	g.GET("/batches/all", s.handleSearchIncomingBatches,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelRead))
	g.POST("/batches/:id/close", s.handleCloseIncomingBatch,
		access.RequirePerm(s.Store, permission.RecvNoticeEdit, access.LevelAll))
	g.GET("/search", s.handleSearch,
		access.RequirePerm(s.Store, permission.RecvNoticeCreate, access.LevelRead))

	// —— D3 到货确认 ——
	g.POST("/arrivals", s.handleConfirmArrival,
		access.RequirePerm(s.Store, permission.RecvArriveConfirm, access.LevelAll))

	// —— D4 过磅与批量袋码 ——
	g.GET("/trucks", s.handleListTrucks,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelRead))
	g.GET("/trucks/:id", s.handleGetTruck,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelRead))
	g.GET("/trucks/:id/bags", s.handleListBags,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelRead))
	g.POST("/trucks/:id/weigh", s.handleWeighTruck,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelAll))
	g.POST("/trucks/:id/bags", s.handleGenBags,
		access.RequirePerm(s.Store, permission.RecvBagGen, access.LevelAll))

	// —— D5 标签打印与补打 ——
	g.POST("/labels/print", s.handlePrintLabels,
		access.RequirePerm(s.Store, permission.RecvLabelPrint, access.LevelAll))
	g.POST("/labels/reprint", s.handleReprintLabels,
		access.RequirePerm(s.Store, permission.RecvLabelReprint, access.LevelAll))
	g.GET("/labels/page", s.handleLabelPage,
		access.RequirePerm(s.Store, permission.RecvLabelPrint, access.LevelRead))
	g.GET("/labels/qr", s.handleLabelQR,
		access.RequirePerm(s.Store, permission.RecvLabelPrint, access.LevelRead))
	g.GET("/labels/history", s.handleLabelHistory,
		access.RequirePerm(s.Store, permission.RecvLabelPrint, access.LevelRead))

	// —— D6 袋作废与退车 ——
	g.POST("/bags/:id/void", s.handleVoidBag,
		access.RequirePerm(s.Store, permission.RecvBagGen, access.LevelAll))
	g.POST("/trucks/:id/return", s.handleReturnTruck,
		access.RequirePerm(s.Store, permission.RecvReturn, access.LevelAll))

	// —— 扫码定位（区分「码非法」与「系统内不存在」）——
	g.GET("/scan", s.handleScan,
		access.RequirePerm(s.Store, permission.RecvWeigh, access.LevelRead))
}

// ---- D2 预报 ----

// noticeForbiddenKeys —— N-023 已删除/由批次带出的预报输入（D2：**不再接受**：
// 显式 400 明确报错，而非静默忽略 —— 静默忽略会让旧客户端以为参数生效了）。
var noticeForbiddenKeys = map[string]string{
	"eta":           "预计到货时间（eta）字段已删除（N-023 系统无预估）",
	"est_bag_count": "预计袋数字段已删除（N-023 系统无预估，一切以扫码实际为准）",
	"arrive_date":   "到货日由到货确认时刻写入，车次预报不再接受该输入",
	"customer_id":   "客户由批次带出（车次预报只选批次）",
	"material_id":   "产品由批次带出（车次预报只选批次）",
	"biz_type":      "业务类型由批次带出（批次必含客户 ⇒ 客供）",
}

// decodeNoticeInput 读取预报类请求体：顶层与嵌套 "notice"（/arrivals 内联补录）
// 键逐一拒绝禁止字段 ⇒ 400 明确文案；空体 = 零值；其余合法 JSON 解码到 out。
func decodeNoticeInput(c echo.Context, out interface{}) error {
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "读取请求体失败")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	reject := func(m map[string]json.RawMessage) error {
		// 按固定顺序报错（map 遍历无序 ⇒ 排序后取首个命中，文案稳定可断言）
		for _, k := range noticeForbiddenKeysOrder {
			if _, ok := m[k]; ok {
				return echo.NewHTTPError(http.StatusBadRequest,
					noticeForbiddenKeys[k]+"（字段 "+k+"）")
			}
		}
		return nil
	}
	if err := reject(top); err != nil {
		return err
	}
	if nested, ok := top["notice"]; ok && string(nested) != "null" {
		var nm map[string]json.RawMessage
		if err := json.Unmarshal(nested, &nm); err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
		}
		if err := reject(nm); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	return nil
}

var noticeForbiddenKeysOrder = []string{
	"eta", "est_bag_count", "arrive_date", "customer_id", "material_id", "biz_type",
}

// handleListNotices 预报列表：?open=1 预报中（含旧版）；?pending=1 = L2
// （★ batch_id 非空 且 预报中 —— 旧版预报排除；含所属批次号）。
func (s *Server) handleListNotices(c echo.Context) error {
	mode := ""
	switch {
	case c.QueryParam("pending") == "1":
		mode = "pending"
	case c.QueryParam("open") == "1":
		mode = "open"
	}
	rows, err := s.Store.ListNotices(c.Request().Context(), mode)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

func (s *Server) handleCreateNotice(c echo.Context) error {
	var in store.NoticeInput
	if err := decodeNoticeInput(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateNotice(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": true, "row": row,
	})
}

func (s *Server) handleUpdateNotice(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Reason string `json:"reason"`
		store.NoticePatch
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.UpdateNotice(c.Request().Context(), id, body.NoticePatch,
		body.Reason, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"changed": true, "row": row,
	})
}

func (s *Server) handleNoticeStatus(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.CancelNotice(c.Request().Context(), id, body.Status,
		body.Reason, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"changed": true, "row": row,
	})
}

// ---- N-023 来料批次 ----

// handleCreateIncomingBatch 预填批次。三元组重复 ⇒ 400 且响应体含已有批次（供跳转）。
func (s *Server) handleCreateIncomingBatch(c echo.Context) error {
	var in store.BatchInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateBatch(c.Request().Context(), in, mdActor(c))
	if err != nil {
		var dup *store.BatchDuplicateError
		if errors.As(err, &dup) {
			return echo.NewHTTPError(http.StatusBadRequest, map[string]interface{}{
				"error":    err.Error(),
				"existing": dup.Existing,
			})
		}
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": true, "row": row,
	})
}

// handleListIncomingBatches L1：未关闭批次列表（?status=回货中 含每批次统计）。
func (s *Server) handleListIncomingBatches(c echo.Context) error {
	rows, err := s.Store.ListBatches(c.Request().Context(), c.QueryParam("status"))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// handleSearchIncomingBatches L3 批次侧：多条件筛选。
func (s *Server) handleSearchIncomingBatches(c echo.Context) error {
	f := store.BatchFilter{
		CustomerID:      qInt64(c, "customer_id"),
		MaterialID:      qInt64(c, "material_id"),
		CustomerBatchNo: c.QueryParam("customer_batch_no"),
		OurBatchNo:      c.QueryParam("our_batch_no"),
		Status:          c.QueryParam("status"),
		Limit:           queryInt(c, "limit"),
	}
	rows, err := s.Store.ListBatchesAll(c.Request().Context(), f)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// handleCloseIncomingBatch 手工关闭批次（幂等）。
func (s *Server) handleCloseIncomingBatch(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.CloseBatch(c.Request().Context(), id, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"closed": true, "row": row,
	})
}

// handleSearch L3 统一查询：批次 + 车次预报（含旧版预报标注「旧版」）。
//
//	筛选：customer_id / material_id / customer_batch_no / our_batch_no /
//	batch_status / notice_status / plate / legacy(1=只看旧版, 0=只看批次预报, 缺省=全部)
func (s *Server) handleSearch(c echo.Context) error {
	bf := store.BatchFilter{
		CustomerID:      qInt64(c, "customer_id"),
		MaterialID:      qInt64(c, "material_id"),
		CustomerBatchNo: c.QueryParam("customer_batch_no"),
		OurBatchNo:      c.QueryParam("our_batch_no"),
		Status:          c.QueryParam("batch_status"),
		Limit:           queryInt(c, "limit"),
	}
	batches, err := s.Store.ListBatchesAll(c.Request().Context(), bf)
	if err != nil {
		return recvErr(err)
	}
	nf := store.NoticeFilter{
		CustomerID:      bf.CustomerID,
		MaterialID:      bf.MaterialID,
		CustomerBatchNo: bf.CustomerBatchNo,
		OurBatchNo:      bf.OurBatchNo,
		Plate:           c.QueryParam("plate"),
		Status:          c.QueryParam("notice_status"),
		Limit:           bf.Limit,
	}
	switch c.QueryParam("legacy") {
	case "1":
		nf.LegacyOnly = true
	case "0":
		nf.BatchOnly = true
	}
	notices, err := s.Store.SearchNotices(c.Request().Context(), nf)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"batch_count": len(batches), "batches": batches,
		"notice_count": len(notices), "notices": notices,
	})
}

// qInt64 包装 md.go 的 queryInt64：解析失败/负数 ⇒ 0（筛选语义「不限」）。
func qInt64(c echo.Context, key string) int64 {
	v, err := queryInt64(c, key)
	if err != nil {
		return 0
	}
	return v
}

// queryInt 解析查询串整数（失败/缺省 = 0）。
func queryInt(c echo.Context, key string) int {
	v, _ := strconv.Atoi(c.QueryParam(key))
	if v < 0 {
		return 0
	}
	return v
}

// ---- D3 到货确认（支持无预报直接到货）----

func (s *Server) handleConfirmArrival(c echo.Context) error {
	var in store.ArriveInput
	if err := decodeNoticeInput(c, &in); err != nil {
		return err
	}
	row, err := s.Store.ConfirmArrival(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"confirmed": true, "row": row,
	})
}

// ---- D4 过磅与袋码 ----

func (s *Server) handleListTrucks(c echo.Context) error {
	rows, err := s.Store.ListTrucks(c.Request().Context(), strings.TrimSpace(c.QueryParam("status")))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

func (s *Server) handleGetTruck(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.GetTruck(c.Request().Context(), id)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": row})
}

func (s *Server) handleListBags(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListBags(c.Request().Context(), id)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

func (s *Server) handleWeighTruck(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.WeighInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.WeighTruck(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"weighed": true, "row": row,
	})
}

func (s *Server) handleGenBags(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Count int `json:"count"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	rows, err := s.Store.GenerateBags(c.Request().Context(), id, body.Count, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"generated": len(rows), "rows": rows,
	})
}

// ---- D5 标签打印 / 补打 ----

func (s *Server) handlePrintLabels(c echo.Context) error {
	return s.printLabels(c, false)
}

func (s *Server) handleReprintLabels(c echo.Context) error {
	return s.printLabels(c, true)
}

// printLabels 落打印留痕并返回**标签版式页地址**（服务端生成页面 → 浏览器打印）。
func (s *Server) printLabels(c echo.Context, forceReprint bool) error {
	var body struct {
		Codes  []string `json:"codes"`
		Reason string   `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	in := store.LabelPrintInput{Codes: body.Codes, Reason: body.Reason}
	if forceReprint {
		in.Reprint = true
	}
	items, err := s.Store.PrintLabels(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, strconv.FormatInt(it.ID, 10))
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count":    len(items),
		"labels":   items,
		"page_url": "/api/recv/labels/page?ids=" + strings.Join(ids, ","),
	})
}

func (s *Server) handleLabelHistory(c echo.Context) error {
	code := strings.ToUpper(strings.TrimSpace(c.QueryParam("code")))
	if code == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 code")
	}
	rows, err := s.Store.ListLabelPrints(c.Request().Context(), code)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- D6 袋作废与退车 ----

func (s *Server) handleVoidBag(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.VoidBag(c.Request().Context(), id, body.Reason, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"voided": true, "row": row,
	})
}

func (s *Server) handleReturnTruck(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.ReturnTruck(c.Request().Context(), id, body.Reason, mdActor(c))
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"returned": true, "row": row,
	})
}

// ---- 扫码定位 ----

func (s *Server) handleScan(c echo.Context) error {
	code := strings.TrimSpace(c.QueryParam("code"))
	if code == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 code")
	}
	res, err := s.Store.ScanCode(c.Request().Context(), code)
	if err != nil {
		return recvErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"found": true, "result": res})
}

// ---- 错误映射 ----
//
// ★ 三种「不存在」必须分开：
//
//	码本身非法（codec.ErrNotOurs / ErrChecksum）⇒ 400（提示原文）；
//	格式正确但系统内不存在（ErrCodeUnknown）⇒ 404；
//	业务规则拒绝（已投料不许作废 / 无退货判定不许退车 …）⇒ 409。
func recvErr(err error) error {
	switch {
	case errors.Is(err, store.ErrRecvNotFound),
		errors.Is(err, store.ErrCodeUnknown),
		errors.Is(err, store.ErrCodeUnsupported):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrSeqOverflow),
		errors.Is(err, store.ErrSeqBusy),
		errors.Is(err, store.ErrNoticeState),
		errors.Is(err, store.ErrTruckState),
		errors.Is(err, store.ErrNotWeighed),
		errors.Is(err, store.ErrBagsExist),
		errors.Is(err, store.ErrWeighAfterBags),
		errors.Is(err, store.ErrBagInUse),
		errors.Is(err, store.ErrAlreadyVoid),
		errors.Is(err, store.ErrNoReturnDecision),
		errors.Is(err, store.ErrTruckFed): // ★ 批 6 联动：已有投料袋不得退车（§6-24）
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrRecvBadInput),
		errors.Is(err, store.ErrVoidReason),
		errors.Is(err, store.ErrReprintReason),
		errors.Is(err, store.ErrBatchClosed), // N-023：关闭后预报新车次 ⇒ 400
		errors.Is(err, store.ErrBatchDuplicate),
		errors.Is(err, codec.ErrNotOurs),
		errors.Is(err, codec.ErrChecksum),
		errors.Is(err, codec.ErrBadInput),
		errors.Is(err, codec.ErrVersion):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
