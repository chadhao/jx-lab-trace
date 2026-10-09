package httpapi

// ===== M10 报表 · 接口（D5）=====
//
// ★★ 只读：本文件**零 SQL 写**（A13 结构化机检的扫描对象之一）——
//	查询走 store.RunRpt（SELECT-only），唯一写是导出审计（store.AppendAudit，
//	落 s_audit_log，不在被扫描的 rpt*.go 里）。
// ★★ 空数据 ≠ 0：返回体固定带 has_data 与 note，两态文案不同（A15）。
// ★ 级别：查报表 GET ⇒ rpt.view 的 LevelRead；导出 POST ⇒ rpt.export 的 LevelAll。

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/audit"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// mountRpt 装配 /api/rpt/* 的受保护入口（★ 集中声明，实参须为权限点常量）。
func (s *Server) mountRpt(e *echo.Echo) {
	g := e.Group("/api/rpt")
	g.GET("/:name", s.handleRunRpt,
		access.RequirePerm(s.Store, permission.RptView, access.LevelRead))
	g.POST("/export", s.handleExportRpt,
		access.RequirePerm(s.Store, permission.RptExport, access.LevelAll))
}

// handleRunRpt 执行一张报表（一期 5 张）。
func (s *Server) handleRunRpt(c echo.Context) error {
	f, err := parseRptFilters(c)
	if err != nil {
		return err
	}
	res, err := s.Store.RunRpt(c.Request().Context(), c.Param("name"), f)
	if err != nil {
		return rptErr(err)
	}
	return c.JSON(http.StatusOK, res)
}

// exportBody 是导出入参。
type exportBody struct {
	Name    string           `json:"name"`
	Filters store.RptFilters `json:"filters"`
}

// handleExportRpt 导出 CSV（★ 只写 s_audit_log，不写任何业务表）。
func (s *Server) handleExportRpt(c echo.Context) error {
	var in exportBody
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	if err := store.ValidateRptFilters(in.Filters); err != nil {
		return rptErr(err)
	}
	res, err := s.Store.RunRpt(c.Request().Context(), in.Name, in.Filters)
	if err != nil {
		return rptErr(err)
	}

	filterJSON, _ := json.Marshal(in.Filters)
	if err := s.Store.AppendAudit(c.Request().Context(), audit.Entry{
		Entity: "rpt." + in.Name, Action: "rpt_export", NewValue: string(filterJSON),
		ActorOpenID: mdActor(c).OpenID, ActorName: mdActor(c).Name,
		ActorRole: mdActor(c).Role, IP: mdActor(c).IP,
	}); err != nil {
		return err
	}

	data := buildRptCSV(res)
	return c.Blob(http.StatusOK, "text/csv; charset=utf-8", data)
}

// buildRptCSV 把报表结果序列化为**带 UTF-8 BOM** 的 CSV（便于 Excel 直接打开）。
// ★ 列序 = Columns，行内容与屏幕一致（TC-M10-04）；nil ⇒ 空串。
func buildRptCSV(res store.RptResult) []byte {
	var sb strings.Builder
	sb.WriteString(string([]byte{0xEF, 0xBB, 0xBF})) // UTF-8 BOM
	w := csv.NewWriter(&sb)

	header := make([]string, 0, len(res.Columns))
	for _, col := range res.Columns {
		header = append(header, col.Label)
	}
	_ = w.Write(header)

	for _, row := range res.Rows {
		rec := make([]string, 0, len(res.Columns))
		for _, col := range res.Columns {
			rec = append(rec, csvCell(row[col.Key]))
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return []byte(sb.String())
}

// csvCell 单元格序列化：nil ⇒ 空串；其余走 JSON 数字 / 字符串原样
// （float64 1.017 ⇒ "1.017"，与屏幕展示一致）。
func csvCell(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// rptErr 报表错误映射（404 未知报表 / 400 参数不合法）。
func rptErr(err error) error {
	switch {
	case errors.Is(err, store.ErrRptUnknown):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrRptBadInput):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
