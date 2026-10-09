package httpapi

// ===== M4 取样与留样 · 接口（D1~D6）=====
//
// ★★ 5 个 sample.* 权限点全部在本包的**路由表**（mountSampling）里，以
//	access 守卫 ＋ 权限点常量（permission.SampleXxx）的形式消费，
//	禁裸写权限点字符串（check_perm_registry 判据③）。
//
// 权限点 → 入口：
//	sample.take                   扫码取样（POST）· 建取样组（POST）· 样品/组/目标查询（GET，READ 级）
//	sample.retain.in              留样入库（POST）· 留样记录与到期检索（GET，READ 级）
//	sample.retain.lend            借出 / 归还（POST）· 借还记录（GET，READ 级）
//	sample.retain.destroy.init    销毁发起（POST，**INIT 级**）
//	sample.retain.destroy.approve 销毁审批（POST，**APPROVE 级** —— 不得用 ALL，
//	                                否则 qc 的 NONE 会被语义绕过；见任务包 §6-11 / A12）

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

// mountSampling 装配 /api/sample/* 的受保护入口（★ 集中声明）。
//
// ★ 每条路由都直接写出守卫调用（不封装成传变量的 helper）——
//
//	check_perm_registry 判据③ 要求实参能静态核验成已登记的权限点常量。
func (s *Server) mountSampling(e *echo.Echo) {
	g := e.Group("/api/sample")

	// —— D1/D2 扫码取样 + D3 取样组（sample.take）——
	g.POST("/take", s.handleTakeSample,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelAll))
	g.GET("/samples", s.handleListSamples,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelRead))
	g.POST("/groups", s.handleCreateGroup,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelAll))
	g.GET("/groups", s.handleListGroups,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelRead))
	g.GET("/groups/:id/members", s.handleGroupMembers,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelRead))
	g.GET("/targets", s.handleListTargets,
		access.RequirePerm(s.Store, permission.SampleTake, access.LevelRead))

	// —— D4 留样入库 + 到期检索（sample.retain.in）——
	g.POST("/retention", s.handleRetain,
		access.RequirePerm(s.Store, permission.SampleRetainIn, access.LevelAll))
	g.GET("/retention", s.handleListRetention,
		access.RequirePerm(s.Store, permission.SampleRetainIn, access.LevelRead))

	// —— D5 借还（sample.retain.lend）——
	g.POST("/lend", s.handleLend,
		access.RequirePerm(s.Store, permission.SampleRetainLend, access.LevelAll))
	g.POST("/lend/:id/return", s.handleReturnLend,
		access.RequirePerm(s.Store, permission.SampleRetainLend, access.LevelAll))
	g.GET("/lend", s.handleListLends,
		access.RequirePerm(s.Store, permission.SampleRetainLend, access.LevelRead))

	// —— D6 销毁：发起 ≠ 审批（两个权限点、两个级别）——
	g.POST("/destroy/init", s.handleDestroyInit,
		access.RequirePerm(s.Store, permission.SampleRetainDestroyInit, access.LevelInit))
	g.POST("/destroy/approve", s.handleDestroyApprove,
		access.RequirePerm(s.Store, permission.SampleRetainDestroyApprove, access.LevelApprove))
	g.GET("/destroy", s.handleListDestroys,
		access.RequirePerm(s.Store, permission.SampleRetainDestroyInit, access.LevelRead))

	// —— 前端按权限显隐用的「我的取样权限摘要」（登录即可，无新增权限点）——
	g.GET("/perm-summary", s.handlePermSummary, access.RequireLogin())
}

// ---- D1/D2 扫码取样 ----

func (s *Server) handleTakeSample(c echo.Context) error {
	var in store.TakeInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	res, err := s.Store.TakeSample(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": len(res.Samples), "parent": res.Parent,
		"code": res.Code, "human": res.Human, "samples": res.Samples,
	})
}

func (s *Server) handleListSamples(c echo.Context) error {
	f := store.SampleFilter{
		Role:       strings.TrimSpace(c.QueryParam("role")),
		TargetType: strings.TrimSpace(c.QueryParam("target_type")),
	}
	if v := strings.TrimSpace(c.QueryParam("target_id")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "target_id 须为整数")
		}
		f.TargetID = id
	}
	// group=none ⇒ 只看未并入的份样（建组下拉用）
	if c.QueryParam("group") == "none" {
		none := int64(-1)
		f.GroupID = &none
	}
	rows, err := s.Store.ListSamples(c.Request().Context(), f)
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- D3 取样组 ----

func (s *Server) handleCreateGroup(c echo.Context) error {
	var in store.GroupInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	grp, err := s.Store.CreateSampleGroup(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": true, "row": grp,
	})
}

func (s *Server) handleListGroups(c echo.Context) error {
	rows, err := s.Store.ListSampleGroups(c.Request().Context(),
		strings.TrimSpace(c.QueryParam("target_type")), 0)
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

func (s *Server) handleGroupMembers(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	grp, err := s.Store.GetSampleGroup(c.Request().Context(), id)
	if err != nil {
		return sampleErr(err)
	}
	members, err := s.Store.ListGroupMembers(c.Request().Context(), id)
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"group": grp, "count": len(members), "members": members,
	})
}

func (s *Server) handleListTargets(c echo.Context) error {
	targetType := strings.TrimSpace(c.QueryParam("target_type"))
	rows, err := s.Store.ListSampleTargets(c.Request().Context(), targetType)
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- D4 留样入库 ----

func (s *Server) handleRetain(c echo.Context) error {
	var in store.RetainInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.RetainSample(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"stored": true, "row": row,
	})
}

func (s *Server) handleListRetention(c echo.Context) error {
	rows, err := s.Store.ListRetention(c.Request().Context(),
		strings.TrimSpace(c.QueryParam("due_before")),
		c.QueryParam("include_destroyed") == "1")
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
		"defaults": s.Store.RetentionDefaults(),
	})
}

// ---- D5 借还 ----

func (s *Server) handleLend(c echo.Context) error {
	var in store.LendInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.LendSample(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"lent": true, "row": row,
	})
}

func (s *Server) handleReturnLend(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.ReturnLend(c.Request().Context(), id, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"returned": true, "row": row,
	})
}

func (s *Server) handleListLends(c echo.Context) error {
	rows, err := s.Store.ListLends(c.Request().Context(), c.QueryParam("open") == "1")
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- D6 销毁 ----

func (s *Server) handleDestroyInit(c echo.Context) error {
	var in store.DestroyInitInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.InitDestroy(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"initiated": true, "row": row,
	})
}

func (s *Server) handleDestroyApprove(c echo.Context) error {
	var body struct {
		SampleID   int64  `json:"sample_id"`
		ApprovedBy string `json:"approved_by"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.ApproveDestroy(c.Request().Context(), body.SampleID,
		body.ApprovedBy, mdActor(c))
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"approved": true, "row": row,
	})
}

func (s *Server) handleListDestroys(c echo.Context) error {
	rows, err := s.Store.ListDestroys(c.Request().Context(), c.QueryParam("pending") == "1")
	if err != nil {
		return sampleErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- 我的取样权限摘要（前端按权限显隐发起/审批按钮）----

func (s *Server) handlePermSummary(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	out := map[string]string{}
	for _, code := range []permission.Code{
		permission.SampleTake,
		permission.SampleRetainIn,
		permission.SampleRetainLend,
		permission.SampleRetainDestroyInit,
		permission.SampleRetainDestroyApprove,
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
// ★ 与批 3 同口径：码本身非法 ⇒ 400；格式正确但系统内不存在 ⇒ 404；
//
//	业务状态冲突（作废/序号超限/已销毁/未发起…）⇒ 409。
func sampleErr(err error) error {
	switch {
	case errors.Is(err, store.ErrCodeUnknown),
		errors.Is(err, store.ErrSampleNotFound),
		errors.Is(err, store.ErrSampleTargetUnknown):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrTargetVoided),
		errors.Is(err, store.ErrSampleSeqOverflow),
		errors.Is(err, store.ErrSampleState),
		errors.Is(err, store.ErrRetentionExists),
		errors.Is(err, store.ErrLendState),
		errors.Is(err, store.ErrDestroyNotInit),
		errors.Is(err, store.ErrDestroyAlready),
		errors.Is(err, store.ErrSeqBusy):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrSampleBadInput),
		errors.Is(err, store.ErrSampleTargetUnsupported),
		errors.Is(err, store.ErrDestroyNeedApprover),
		errors.Is(err, codec.ErrNotOurs),
		errors.Is(err, codec.ErrChecksum),
		errors.Is(err, codec.ErrBadInput),
		errors.Is(err, codec.ErrVersion):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
