package httpapi

// ===== M8 追溯 · 接口（D4 正向 / D5 反向 / D6 批次档案）=====
//
// ★★ 3 个 trace.* 权限点全部在本包的**路由表**（mountTrace）里消费；
//	全部读入口 ⇒ access.LevelRead（A14）。
// ★★ 本模块零写业务表（A17）：handler 只调 store 的只读方法，不落任何审计。
// ★ 未命中 ≠ 报错（§6-12 / A10）：正向查无流向由 store 层返回 200 + has_flow:false，
//	本层不把它映射成 404/500。

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

// mountTrace 装配 /api/trace/* 的受保护入口（★ 集中声明，实参须为权限点常量）。
func (s *Server) mountTrace(e *echo.Echo) {
	g := e.Group("/api/trace")

	g.GET("/forward", s.handleTraceForward,
		access.RequirePerm(s.Store, permission.TraceForward, access.LevelRead))
	g.GET("/backward", s.handleTraceBackward,
		access.RequirePerm(s.Store, permission.TraceBackward, access.LevelRead))
	g.GET("/batch/:batchId", s.handleTraceBatchArchive,
		access.RequirePerm(s.Store, permission.TraceBatchView, access.LevelRead))
}

// ---- D4 正向追溯 ----

func (s *Server) handleTraceForward(c echo.Context) error {
	truckLotID := int64(0)
	if v := strings.TrimSpace(c.QueryParam("truck_lot_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "truck_lot_id 须为非负整数")
		}
		truckLotID = n
	}
	bagCode := strings.TrimSpace(c.QueryParam("bag_code"))
	if truckLotID == 0 && bagCode == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "truck_lot_id / bag_code 至少一个")
	}
	row, err := s.Store.TraceForward(c.Request().Context(), truckLotID, bagCode)
	if err != nil {
		return traceErr(err)
	}
	return c.JSON(http.StatusOK, row)
}

// ---- D5 反向追溯 ----

func (s *Server) handleTraceBackward(c echo.Context) error {
	fgLotID := int64(0)
	if v := strings.TrimSpace(c.QueryParam("fg_lot_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "fg_lot_id 须为非负整数")
		}
		fgLotID = n
	}
	fgCode := strings.TrimSpace(c.QueryParam("fg_code"))
	if fgLotID == 0 && fgCode == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "fg_lot_id / fg_code 至少一个")
	}
	row, err := s.Store.TraceBackward(c.Request().Context(), fgLotID, fgCode)
	if err != nil {
		return traceErr(err)
	}
	return c.JSON(http.StatusOK, row)
}

// ---- D6 批次档案 ----

func (s *Server) handleTraceBatchArchive(c echo.Context) error {
	batchID, err := strconv.ParseInt(c.Param("batchId"), 10, 64)
	if err != nil || batchID <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "batchId 须为正整数")
	}
	row, err := s.Store.GetBatchArchive(c.Request().Context(), batchID)
	if err != nil {
		return traceErr(err)
	}
	return c.JSON(http.StatusOK, row)
}

// ---- 错误映射 ----
//
//	404：生产批 / 成品批不存在；400：输入不合法（含 codec 错误）。
//	★ 查无流向**不在这里** —— 它是 200 + has_flow:false（store 层已表达）。
func traceErr(err error) error {
	switch {
	case errors.Is(err, store.ErrProdNotFound),
		errors.Is(err, store.ErrCodeUnknown),
		errors.Is(err, store.ErrCodeUnsupported):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrProdBadInput),
		errors.Is(err, codec.ErrNotOurs),
		errors.Is(err, codec.ErrChecksum),
		errors.Is(err, codec.ErrBadInput),
		errors.Is(err, codec.ErrVersion):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
