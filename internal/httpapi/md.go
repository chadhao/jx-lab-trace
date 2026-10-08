package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// ===== M1 主数据接口 =====
//
// 路由（每类主数据 6 条，守卫见 server.go 的 mdGuards）：
//
//	GET    /api/md/{entity}              列表（?history=1 列全部版本 / ?status= / ?q= / ?limit=）
//	POST   /api/md/{entity}              新增
//	GET    /api/md/{entity}/:id          单行
//	GET    /api/md/{entity}/:id/history  版本链（含已失效版本）
//	PUT    /api/md/{entity}/:id          修改业务字段 ⇒ **作废 + 新增**，reason 必填
//	PATCH  /api/md/{entity}/:id/status   启用 / 停用（status 原地流转，不产生新版本）

func (s *Server) handleMDList(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
		limit := 0
		if v := strings.TrimSpace(c.QueryParam("limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		rows, err := s.Store.MDList(c.Request().Context(), entity, store.MDOpts{
			IncludeHistory: c.QueryParam("history") == "1",
			Status:         strings.TrimSpace(c.QueryParam("status")),
			Search:         strings.TrimSpace(c.QueryParam("q")),
			Limit:          limit,
		})
		if err != nil {
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{
			"entity": entity, "count": len(rows), "rows": orEmptyMD(rows),
		})
	}
}

func (s *Server) handleMDGet(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		row, err := s.Store.MDGet(c.Request().Context(), entity, id)
		if err != nil {
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{"entity": entity, "row": row})
	}
}

func (s *Server) handleMDHistory(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		chain, err := s.Store.MDHistory(c.Request().Context(), entity, id)
		if err != nil {
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{
			"entity": entity, "count": len(chain), "chain": orEmptyMD(chain),
		})
	}
}

func (s *Server) handleMDCreate(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
		values, _, err := decodeMDBody(c)
		if err != nil {
			return err
		}
		row, err := s.Store.MDCreate(c.Request().Context(), entity, values, mdActor(c))
		if err != nil {
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{
			"entity": entity, "created": true, "row": row,
		})
	}
}

func (s *Server) handleMDUpdate(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
		id, err := pathID(c)
		if err != nil {
			return err
		}
		values, reason, err := decodeMDBody(c)
		if err != nil {
			return err
		}
		row, err := s.Store.MDUpdate(c.Request().Context(), entity, id, values, reason, mdActor(c))
		if err != nil {
			if errors.Is(err, store.ErrMDNoChange) {
				return c.JSON(http.StatusOK, map[string]interface{}{
					"entity": entity, "changed": false, "reason": "内容未变化，未产生新版本",
				})
			}
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{
			"entity": entity, "changed": true, "row": row,
		})
	}
}

func (s *Server) handleMDStatus(entity string) echo.HandlerFunc {
	return func(c echo.Context) error {
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
		row, err := s.Store.MDSetStatus(c.Request().Context(), entity, id,
			body.Status, body.Reason, mdActor(c))
		if err != nil {
			if errors.Is(err, store.ErrMDNoChange) {
				return c.JSON(http.StatusOK, map[string]interface{}{
					"entity": entity, "changed": false, "status": body.Status,
				})
			}
			return mdErr(err)
		}
		return c.JSON(http.StatusOK, map[string]interface{}{
			"entity": entity, "changed": true, "row": row,
		})
	}
}

// decodeMDBody 解析主数据写请求：{"values": {...}, "reason": "..."}，
// 兼容顶层平铺（除 reason 外都是字段）。
// handleResolveLimit 判定限解析入口（D3 / A4）：
//
//	GET /api/limits/resolve?test_item_id=&customer_id=&material_id=
//
// 返回命中的判定限与它的 rank（3=精确 2=客户专属 1=物料专属 0=通用），
// 让「最具体者优先」在接口上可直接观测。未配置 ⇒ resolved=false（合法答案，不是错误）。
func (s *Server) handleResolveLimit(c echo.Context) error {
	testItemID, err := queryInt64(c, "test_item_id")
	if err != nil {
		return err
	}
	customerID, err := queryInt64(c, "customer_id")
	if err != nil {
		return err
	}
	materialID, err := queryInt64(c, "material_id")
	if err != nil {
		return err
	}
	row, ok, err := s.Store.LookupLimit(c.Request().Context(), testItemID, customerID, materialID)
	if err != nil {
		return mdErr(err)
	}
	if !ok {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"resolved": false, "test_item_id": testItemID,
			"customer_id": customerID, "material_id": materialID,
			"hint": "未配置任何可用判定限（含通用默认）",
		})
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"resolved": true, "limit": row,
		"rank": store.LimitRank(row.CustomerID, row.MaterialID, customerID, materialID),
		"scope": map[string]interface{}{
			"customer_id": row.CustomerID, "material_id": row.MaterialID,
		},
	})
}

func queryInt64(c echo.Context, name string) (int64, error) {
	raw := strings.TrimSpace(c.QueryParam(name))
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0, echo.NewHTTPError(http.StatusBadRequest, name+" 必须是非负整数")
	}
	return v, nil
}

func decodeMDBody(c echo.Context) (map[string]interface{}, string, error) {
	var body struct {
		Values map[string]interface{} `json:"values"`
		Reason string                 `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return nil, "", err
	}
	if body.Values != nil {
		return body.Values, body.Reason, nil
	}
	return nil, "", echo.NewHTTPError(http.StatusBadRequest, "缺少 values 对象")
}

func pathID(c echo.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, echo.NewHTTPError(http.StatusBadRequest, "id 必须是正整数")
	}
	return id, nil
}

func decodeBody(c echo.Context, out interface{}) error {
	if c.Request().Body == nil {
		return nil
	}
	dec := json.NewDecoder(c.Request().Body)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest, "请求体不是合法 JSON")
	}
	return nil
}

// mdErr 把 store 的哨兵错误映射成 HTTP 状态码（4xx 带原文，5xx 由统一错误处理器打码）。
func mdErr(err error) error {
	switch {
	case errors.Is(err, store.ErrMDEntityNotFound),
		errors.Is(err, store.ErrMDRowNotFound):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrMDDuplicate),
		errors.Is(err, store.ErrMDStale),
		errors.Is(err, store.ErrBuiltinRole),
		errors.Is(err, store.ErrDeadlockGuard),
		errors.Is(err, store.ErrLastAdmin),
		errors.Is(err, store.ErrOccupied),
		errors.Is(err, store.ErrBadLevel),
		errors.Is(err, store.ErrRoleNotFound),
		errors.Is(err, store.ErrRoleCodeBad),
		errors.Is(err, store.ErrRoleNameEmpty),
		errors.Is(err, store.ErrMatrixEmpty),
		errors.Is(err, store.ErrPointNotFound):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrMDBadInput),
		errors.Is(err, store.ErrMDNeedReason):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}

func orEmptyMD(rows []store.MDRow) []store.MDRow {
	if rows == nil {
		return []store.MDRow{}
	}
	return rows
}
