package httpapi

// ===== M6 生产与谱系 · 接口（D1~D6）=====
//
// ★★ 6 个 prod.* 权限点全部在本包的**路由表**（mountProd）里，以 access 守卫
//	＋ 权限点常量（permission.ProdXxx）的形式消费，禁裸写权限点字符串。
//
// 权限点 → 入口（★ 级别逐点核对，A12）：
//	prod.batch.create  建批（POST，ALL）· 批列表 / 详情（GET，READ）
//	prod.feed.scan     投料扫码（POST，ALL）· 投料明细 / 谱系反查（GET，READ）
//	prod.feed.correct   投料更正（PATCH）/ 删除（DELETE），均 ALL
//	prod.op.log        作业段新增（POST，ALL）· 列表（GET，READ）
//	prod.fg.gen        成品批 / 成品袋生成与打印（POST，ALL）· 列表（GET，READ）
//	prod.rework        返工发起（POST，★ LevelInit —— 不是 LevelAll，否则 production
//	                   的 INIT 被挡）· 返工关联查询（GET，READ）
//
// ★ 不设返工审批入口：返工不在 spec#constraints.approval_scope 的四类内（§6-16）。

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

// mountProd 装配 /api/prod/* 的受保护入口（★ 集中声明）。
//
// ★ 每条路由都直接写出守卫调用（不封装成传变量的 helper）——
//
//	check_perm_registry 判据③ 要求实参能静态核验成已登记的权限点常量。
func (s *Server) mountProd(e *echo.Echo) {
	g := e.Group("/api/prod")

	// —— D1 建生产批（prod.batch.create：写 ALL / 读 READ）——
	g.GET("/batches", s.handleListBatches,
		access.RequirePerm(s.Store, permission.ProdBatchCreate, access.LevelRead))
	g.POST("/batches", s.handleCreateBatch,
		access.RequirePerm(s.Store, permission.ProdBatchCreate, access.LevelAll))
	g.GET("/batches/:id", s.handleGetBatch,
		access.RequirePerm(s.Store, permission.ProdBatchCreate, access.LevelRead))

	// —— D2 投料扫码（谱系承重墙）+ 更正 / 删除 ——
	g.GET("/batches/:id/feeds", s.handleListFeeds,
		access.RequirePerm(s.Store, permission.ProdFeedScan, access.LevelRead))
	g.POST("/batches/:id/feeds", s.handleFeedScan,
		access.RequirePerm(s.Store, permission.ProdFeedScan, access.LevelAll))
	g.PATCH("/feeds/:id", s.handleCorrectFeed,
		access.RequirePerm(s.Store, permission.ProdFeedCorrect, access.LevelAll))
	g.DELETE("/feeds/:id", s.handleDeleteFeed,
		access.RequirePerm(s.Store, permission.ProdFeedCorrect, access.LevelAll))

	// —— D5 谱系反查（读，投料与谱系 ⇒ prod.feed.scan/READ）——
	g.GET("/genealogy/bags/:code", s.handleGenealogy,
		access.RequirePerm(s.Store, permission.ProdFeedScan, access.LevelRead))

	// —— D3 作业段（prod.op.log）——
	g.GET("/batches/:id/operations", s.handleListOperations,
		access.RequirePerm(s.Store, permission.ProdOpLog, access.LevelRead))
	g.POST("/batches/:id/operations", s.handleAddOperation,
		access.RequirePerm(s.Store, permission.ProdOpLog, access.LevelAll))

	// —— D4 成品批 / 成品袋生成与打码（prod.fg.gen）——
	g.GET("/batches/:id/fg-lots", s.handleListFgLots,
		access.RequirePerm(s.Store, permission.ProdFgGen, access.LevelRead))
	g.POST("/batches/:id/fg-lots", s.handleCreateFgLot,
		access.RequirePerm(s.Store, permission.ProdFgGen, access.LevelAll))
	g.GET("/fg-lots/:id/bags", s.handleListFgBags,
		access.RequirePerm(s.Store, permission.ProdFgGen, access.LevelRead))
	g.POST("/fg-lots/:id/bags", s.handleGenFgBags,
		access.RequirePerm(s.Store, permission.ProdFgGen, access.LevelAll))
	g.POST("/fg-lots/:id/print", s.handlePrintFgBags,
		access.RequirePerm(s.Store, permission.ProdFgGen, access.LevelAll))

	// —— D6 返工（prod.rework：发起 ★ LevelInit / 读 READ）——
	g.POST("/rework", s.handleCreateRework,
		access.RequirePerm(s.Store, permission.ProdRework, access.LevelInit))
	g.GET("/rework", s.handleListReworks,
		access.RequirePerm(s.Store, permission.ProdRework, access.LevelRead))

	// —— 前端按权限显隐用的「我的生产权限摘要」（登录即可，无新增权限点）——
	g.GET("/perm-summary", s.handleProdPermSummary, access.RequireLogin())
}

// ---- D1 生产批 ----

func (s *Server) handleListBatches(c echo.Context) error {
	rows, err := s.Store.ListProductionBatches(c.Request().Context(),
		strings.TrimSpace(c.QueryParam("status")))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleCreateBatch(c echo.Context) error {
	var in store.CreateBatchInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateProductionBatch(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

func (s *Server) handleGetBatch(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.GetProductionBatch(c.Request().Context(), id)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": row})
}

// ---- D2 投料 ----

func (s *Server) handleListFeeds(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListFeeds(c.Request().Context(), id)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleFeedScan(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.FeedInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.FeedScan(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"fed": true, "row": row})
}

func (s *Server) handleCorrectFeed(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.FeedCorrectInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CorrectFeed(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"corrected": true, "row": row})
}

func (s *Server) handleDeleteFeed(c echo.Context) error {
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
	if err := s.Store.DeleteFeed(c.Request().Context(), id, body.Reason, mdActor(c)); err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"deleted": true})
}

// ---- D5 谱系 ----

func (s *Server) handleGenealogy(c echo.Context) error {
	code := strings.TrimSpace(c.Param("code"))
	if code == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "缺少袋码")
	}
	row, err := s.Store.GenealogyByBagCode(c.Request().Context(), code)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"found": true, "row": row})
}

// ---- D3 作业段 ----

func (s *Server) handleListOperations(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListOperations(c.Request().Context(), id)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleAddOperation(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.OperationInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.AddOperation(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

// ---- D4 成品批 / 成品袋 ----

func (s *Server) handleListFgLots(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListFgLots(c.Request().Context(), id)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleCreateFgLot(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.CreateFgLotInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateFgLot(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

func (s *Server) handleListFgBags(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListFgBags(c.Request().Context(), id)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleGenFgBags(c echo.Context) error {
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
	rows, err := s.Store.GenerateFgBags(c.Request().Context(), id, body.Count, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handlePrintFgBags(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var in store.FgPrintInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	rows, err := s.Store.PrintFgBags(c.Request().Context(), id, in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

// ---- D6 返工 ----

func (s *Server) handleCreateRework(c echo.Context) error {
	var in store.ReworkInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateRework(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

func (s *Server) handleListReworks(c echo.Context) error {
	src := int64(0)
	if v := strings.TrimSpace(c.QueryParam("src_batch_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return echo.NewHTTPError(http.StatusBadRequest, "src_batch_id 须为非负整数")
		}
		src = n
	}
	rows, err := s.Store.ListReworks(c.Request().Context(), src)
	if err != nil {
		return prodErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

// ---- 我的生产权限摘要（前端按权限显隐动作按钮）----

func (s *Server) handleProdPermSummary(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	out := map[string]string{}
	for _, code := range []permission.Code{
		permission.ProdBatchCreate,
		permission.ProdFeedScan,
		permission.ProdFeedCorrect,
		permission.ProdOpLog,
		permission.ProdFgGen,
		permission.ProdRework,
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
//	404：对象 / 码不存在；409：业务状态冲突（已投料 / 未出结论 / 超限 / 已作废 …）；
//	400：输入不合法（含 codec 的格式与校验位错误）。
func prodErr(err error) error {
	switch {
	case errors.Is(err, store.ErrProdNotFound),
		errors.Is(err, store.ErrCodeUnknown),
		errors.Is(err, store.ErrCodeUnsupported):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrProdState),
		errors.Is(err, store.ErrProdSeqBusy),
		errors.Is(err, store.ErrBatchSeqOverflow),
		errors.Is(err, store.ErrFgSeqOverflow),
		errors.Is(err, store.ErrFgBagSeqOverflow),
		errors.Is(err, store.ErrFeedNotReleased),
		errors.Is(err, store.ErrFeedDup),
		errors.Is(err, store.ErrAlreadyVoid),
		errors.Is(err, store.ErrReworkSrcState),
		errors.Is(err, store.ErrTruckFed),
		errors.Is(err, store.ErrReprintReason):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrProdBadInput),
		errors.Is(err, store.ErrFeedReason),
		errors.Is(err, store.ErrFgMismatch),
		errors.Is(err, store.ErrReworkReason),
		errors.Is(err, store.ErrRecvBadInput),
		errors.Is(err, codec.ErrNotOurs),
		errors.Is(err, codec.ErrChecksum),
		errors.Is(err, codec.ErrBadInput),
		errors.Is(err, codec.ErrVersion):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
