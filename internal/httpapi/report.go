package httpapi

// ===== M9 报告分享 · 接口（D1~D4）=====
//
// ★★ 4 个权限点全部在本包路由表（mountReport / mountRpt）里以 access 守卫
//	＋ 权限点常量消费（判据②③），禁裸写权限点字符串。
//
// 权限点 → 入口（★ 级别逐点核对，A14 / §6-7）：
//	report.generate       生成 / 刷新（POST，★ LevelAll）
//	report.share.manage   设有效期 / 撤销 / 过期清扫 / 访问日志同步（POST，★ LevelAll）
//	rpt.view              ★ 报告列表 / 详情 / 访问日志（GET，★ LevelRead ——
//	                      本批唯一含 READ 的点；report.generate 与
//	                      report.share.manage 的 levels 均为 ["ALL","NONE"] 无 READ）
//	rpt.export            导出 CSV（POST，★ LevelAll）
//
// ★ 撤销**单步生效**（§6-5）：report.share.manage 一个点 ⇒ 没有发起 / 审批两段式。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// mountReport 装配 /api/report/* 的受保护入口（★ 集中声明，实参须为权限点常量）。
func (s *Server) mountReport(e *echo.Echo) {
	g := e.Group("/api/report")

	// —— D1/D2 生成与刷新（report.generate：ALL）——
	g.POST("/generate", s.handleGenerateReport,
		access.RequirePerm(s.Store, permission.ReportGenerate, access.LevelAll))
	g.POST("/:id/refresh", s.handleRefreshReport,
		access.RequirePerm(s.Store, permission.ReportGenerate, access.LevelAll))

	// —— D3 生命周期（report.share.manage：ALL；撤销单步生效）——
	g.POST("/:id/expires", s.handleSetReportExpires,
		access.RequirePerm(s.Store, permission.ReportShareManage, access.LevelAll))
	g.POST("/:id/revoke", s.handleRevokeReport,
		access.RequirePerm(s.Store, permission.ReportShareManage, access.LevelAll))
	g.POST("/expire/sweep", s.handleSweepReports,
		access.RequirePerm(s.Store, permission.ReportShareManage, access.LevelAll))

	// —— D4 访问日志同步（report.share.manage：ALL）——
	g.POST("/access/sync", s.handleSyncAccess,
		access.RequirePerm(s.Store, permission.ReportShareManage, access.LevelAll))

	// —— 读入口 ★ 全部挂 rpt.view 的 LevelRead（§6-7）——
	g.GET("/list", s.handleListReports,
		access.RequirePerm(s.Store, permission.RptView, access.LevelRead))
	g.GET("/:id", s.handleGetReport,
		access.RequirePerm(s.Store, permission.RptView, access.LevelRead))
	g.GET("/:id/access", s.handleReportAccess,
		access.RequirePerm(s.Store, permission.RptView, access.LevelRead))

	// —— 前端按权限显隐用的权限摘要（4 点；登录即可）——
	g.GET("/perm-summary", s.handleReportPermSummary, access.RequireLogin())
}

// ---- D1 生成 ----

func (s *Server) handleGenerateReport(c echo.Context) error {
	var in store.GenerateReportInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.GenerateReport(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

// ---- D2 刷新（新 token 新链接）----

func (s *Server) handleRefreshReport(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, superseded, err := s.Store.RefreshReport(c.Request().Context(), id, mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"refreshed": true, "row": row, "superseded_report_no": superseded,
	})
}

// ---- D3 生命周期 ----

func (s *Server) handleSetReportExpires(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.SetReportExpires(c.Request().Context(), id, body.ExpiresAt, mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"updated": true, "row": row})
}

func (s *Server) handleRevokeReport(c echo.Context) error {
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
	row, err := s.Store.RevokeReport(c.Request().Context(), id, body.Reason, mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"revoked": true, "row": row})
}

func (s *Server) handleSweepReports(c echo.Context) error {
	swept, nos, err := s.Store.SweepExpiredReports(c.Request().Context(), mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"swept": swept, "report_nos": nos})
}

// ---- D4 访问日志同步 ----

func (s *Server) handleSyncAccess(c echo.Context) error {
	res, err := s.Store.SyncReportAccess(c.Request().Context(), mdActor(c))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, res)
}

// ---- 读入口（rpt.view / LevelRead）----

func (s *Server) handleListReports(c echo.Context) error {
	rows, err := s.Store.ListReports(c.Request().Context(), c.QueryParam("status"))
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

func (s *Server) handleGetReport(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.GetReport(c.Request().Context(), id)
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": row})
}

func (s *Server) handleReportAccess(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListReportAccess(c.Request().Context(), id)
	if err != nil {
		return reportErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"count": len(rows), "rows": rows})
}

// ---- 权限摘要（M9 四点，前端按权限显隐动作按钮）----

func (s *Server) handleReportPermSummary(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	out := map[string]string{}
	for _, code := range []permission.Code{
		permission.ReportGenerate,
		permission.ReportShareManage,
		permission.RptView,
		permission.RptExport,
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
//	404：报告不存在；409：状态冲突 / 取号超限；400：输入与禁用词；其余 ⇒ 500。
func reportErr(err error) error {
	switch {
	case errors.Is(err, store.ErrReportNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrReportState),
		errors.Is(err, store.ErrReportSeqBusy),
		errors.Is(err, store.ErrReportSeqOverflow):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrReportBadInput),
		errors.Is(err, store.ErrReportForbidden):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}

// parseRptFilters 从 query 解析通用筛选（非法值 ⇒ 400）。
func parseRptFilters(c echo.Context) (store.RptFilters, error) {
	f := store.RptFilters{
		From:   strings.TrimSpace(c.QueryParam("from")),
		To:     strings.TrimSpace(c.QueryParam("to")),
		Bucket: strings.TrimSpace(c.QueryParam("bucket")),
	}
	if v := strings.TrimSpace(c.QueryParam("customer_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return f, echo.NewHTTPError(http.StatusBadRequest, "customer_id 须为非负整数")
		}
		f.CustomerID = n
	}
	if err := store.ValidateRptFilters(f); err != nil {
		return f, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return f, nil
}
