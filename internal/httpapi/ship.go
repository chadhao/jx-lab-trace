package httpapi

// ===== M7 出货 · 接口（D1~D3）=====
//
// ★★ 4 个 ship.* 权限点全部在本包的**路由表**（mountShip）里，以 access 守卫
//	＋ 权限点常量（permission.ShipXxx）的形式消费，禁裸写权限点字符串。
//
// 权限点 → 入口（★ 级别逐点核对，A14 / §6-8）：
//	ship.load.scan    建单归集 / 追加扫码（POST，ALL）· 列表 / 详情（GET，READ）
//	ship.out.register 出场登记（POST，ALL）
//	ship.void.init    撤销发起（POST，★ LevelInit）· 撤销留痕查询（GET，READ）
//	ship.void.approve 撤销审批（POST，★ LevelApprove；该点 levels 无 READ ⇒ 不挂读入口）
//
// ★ 撤销留痕读入口挂 ship.void.init 的 LevelRead（该点 levels 含 READ）——
//	不得挂 ship.void.approve（["APPROVE","NONE"]，谁都读不到）。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/codec"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// mountShip 装配 /api/ship/* 的受保护入口（★ 集中声明，实参须为权限点常量）。
func (s *Server) mountShip(e *echo.Echo) {
	g := e.Group("/api/ship")

	// —— D1 装车归集（ship.load.scan：写 ALL / 读 READ）——
	g.POST("/shipments", s.handleCreateShipment,
		access.RequirePerm(s.Store, permission.ShipLoadScan, access.LevelAll))
	g.GET("/shipments", s.handleListShipments,
		access.RequirePerm(s.Store, permission.ShipLoadScan, access.LevelRead))
	g.GET("/shipments/:id", s.handleGetShipment,
		access.RequirePerm(s.Store, permission.ShipLoadScan, access.LevelRead))
	g.POST("/shipments/:id/items", s.handleAddShipmentItem,
		access.RequirePerm(s.Store, permission.ShipLoadScan, access.LevelAll))

	// —— D2 出场登记（ship.out.register：写 ALL）——
	g.POST("/shipments/:id/depart", s.handleDepartShipment,
		access.RequirePerm(s.Store, permission.ShipOutRegister, access.LevelAll))

	// —— D3 撤销（发起 LevelInit / 审批 LevelApprove；留痕读挂 init 点 READ）——
	g.POST("/shipments/:id/void", s.handleInitShipVoid,
		access.RequirePerm(s.Store, permission.ShipVoidInit, access.LevelInit))
	g.POST("/shipments/:id/void/approve", s.handleApproveShipVoid,
		access.RequirePerm(s.Store, permission.ShipVoidApprove, access.LevelApprove))
	g.GET("/shipments/:id/void-records", s.handleShipVoidRecords,
		access.RequirePerm(s.Store, permission.ShipVoidInit, access.LevelRead))

	// —— 前端按权限显隐用的权限摘要（含 M7 四点 + M8 三点；登录即可）——
	g.GET("/perm-summary", s.handleShipPermSummary, access.RequireLogin())
}

// ---- D1 装车归集 ----

func (s *Server) handleCreateShipment(c echo.Context) error {
	var in store.CreateShipmentInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateShipment(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

func (s *Server) handleAddShipmentItem(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.AddShipmentItemInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.AddShipmentItem(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"added": true, "row": row})
}

func (s *Server) handleListShipments(c echo.Context) error {
	customerID := int64(0)
	if v := strings.TrimSpace(c.QueryParam("customer_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "customer_id 须为非负整数")
		}
		customerID = n
	}
	rows, err := s.Store.ListShipments(c.Request().Context(),
		strings.TrimSpace(c.QueryParam("status")), customerID)
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleGetShipment(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.GetShipment(c.Request().Context(), id)
	if err != nil {
		return shipErr(err)
	}
	items, err := s.Store.ListShipmentItems(c.Request().Context(), id)
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": row, "items": items})
}

// ---- D2 出场登记 ----

func (s *Server) handleDepartShipment(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.DepartInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.DepartShipment(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"departed": true, "row": row})
}

// ---- D3 撤销发起 / 审批 / 留痕 ----

func (s *Server) handleInitShipVoid(c echo.Context) error {
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
	row, err := s.Store.InitShipVoid(c.Request().Context(), id, body.Reason, mdActor(c))
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"initiated": true, "row": row})
}

func (s *Server) handleApproveShipVoid(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.ApproveShipVoid(c.Request().Context(), id, mdActor(c))
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"approved": true, "row": row})
}

func (s *Server) handleShipVoidRecords(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ShipVoidRecords(c.Request().Context(), id)
	if err != nil {
		return shipErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

// ---- 权限摘要（M7 四点 + M8 三点，前端按权限显隐动作按钮）----

func (s *Server) handleShipPermSummary(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	out := map[string]string{}
	for _, code := range []permission.Code{
		permission.ShipLoadScan,
		permission.ShipOutRegister,
		permission.ShipVoidInit,
		permission.ShipVoidApprove,
		permission.TraceForward,
		permission.TraceBackward,
		permission.TraceBatchView,
	} {
		lv, err := s.Store.LevelsFor(c.Request().Context(), p.OpenID, code)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, "权限查询失败")
		}
		if !lv.Granted() {
			out[code.String()] = "NONE"
			continue
		}
		joined := "NONE"
		if len(lv) > 0 {
			joined = strings.Join(lv, ",")
		}
		out[code.String()] = joined
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"points": out})
}

// ---- 错误映射 ----
//
//	404：出货单 / 袋码不存在；409：业务状态冲突（已归集 / 已登记 / 未发起 / 超限 …）；
//	403：撤销发起人自审；400：输入不合法（含 codec 格式与校验位错误）。
func shipErr(err error) error {
	switch {
	case errors.Is(err, store.ErrShipNotFound),
		errors.Is(err, store.ErrCodeUnknown),
		errors.Is(err, store.ErrCodeUnsupported):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrShipState),
		errors.Is(err, store.ErrShipSeqBusy),
		errors.Is(err, store.ErrShipSeqOverflow),
		errors.Is(err, store.ErrShipBagCollected),
		errors.Is(err, store.ErrShipNoItems),
		errors.Is(err, store.ErrShipDeparted),
		errors.Is(err, store.ErrShipVoidNotInit),
		errors.Is(err, store.ErrShipVoidPending),
		errors.Is(err, store.ErrSeqBusy):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrShipVoidSelf):
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrShipBadInput),
		errors.Is(err, store.ErrShipCrossCustomer),
		errors.Is(err, codec.ErrNotOurs),
		errors.Is(err, codec.ErrChecksum),
		errors.Is(err, codec.ErrBadInput),
		errors.Is(err, codec.ErrVersion):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
