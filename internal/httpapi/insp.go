package httpapi

// ===== M5 检测 · 接口（D1~D7）=====
//
// ★★ 11 个 insp.* 权限点全部在本包的**路由表**（mountInsp）里，以
//	access 守卫 ＋ 权限点常量（permission.InspXxx）的形式消费，
//	禁裸写权限点字符串（check_perm_registry 判据③）。
//
// 权限点 → 入口（级别逐点核对，任务包 §6-12 / A15）：
//	insp.task.view              任务列表 / 单据与清单读取 / 放行态查询（READ）
//	insp.scope.edit             建单 · 加项 · 删项 · 复检（ALL）
//	insp.result.entry           结果录入（ALL）
//	insp.result.correct         修正（ALL）
//	insp.file.upload            附件上传（ALL）
//	insp.conclusion             出结论（ALL）
//	insp.disposition            处置（ALL）
//	insp.concession.qc_sign     让步质检签署（ALL —— 不是 INIT/APPROVE）
//	insp.concession.dept_sign   让步使用部门签署（ALL）
//	insp.urgent.release.init    紧急放行发起（★ INIT 级）
//	insp.urgent.release.approve 紧急放行审批（★ APPROVE 级 —— 绝不能用 ALL）

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
	"github.com/chadhao/jx-lab-trace/internal/permission"
	"github.com/chadhao/jx-lab-trace/internal/store"
	"github.com/labstack/echo/v4"
)

// defaultAttachMaxMB 与 config 缺省一致：cfg 未注入时的兜底（测试 env 直构 Config）。
const defaultAttachMaxMB = 20

// mountInsp 装配 /api/insp/* 的受保护入口（★ 集中声明）。
//
// ★ 每条路由都直接写出守卫调用（不封装成传变量的 helper）——
//
//	check_perm_registry 判据③ 要求实参能静态核验成已登记的权限点常量。
func (s *Server) mountInsp(e *echo.Echo) {
	g := e.Group("/api/insp")

	// —— D1 任务列表（insp.task.view，READ）——
	g.GET("/tasks", s.handleListInspTasks,
		access.RequirePerm(s.Store, permission.InspTaskView, access.LevelRead))

	// —— D2 建单 / 清单（insp.scope.edit，ALL）+ 读取（insp.task.view，READ）——
	g.POST("", s.handleCreateInsp,
		access.RequirePerm(s.Store, permission.InspScopeEdit, access.LevelAll))
	g.GET("/:id", s.handleGetInsp,
		access.RequirePerm(s.Store, permission.InspTaskView, access.LevelRead))
	g.GET("/:id/items", s.handleListItems,
		access.RequirePerm(s.Store, permission.InspTaskView, access.LevelRead))
	g.POST("/:id/items", s.handleAddItems,
		access.RequirePerm(s.Store, permission.InspScopeEdit, access.LevelAll))
	g.DELETE("/:id/items/:itemId", s.handleDeleteItem,
		access.RequirePerm(s.Store, permission.InspScopeEdit, access.LevelAll))

	// —— D3 结果三态录入（insp.result.entry，ALL）——
	g.PATCH("/:id/items/:itemId", s.handleRecordResult,
		access.RequirePerm(s.Store, permission.InspResultEntry, access.LevelAll))

	// —— D4 附件（insp.file.upload，ALL）——
	g.POST("/:id/files", s.handleUploadFile,
		access.RequirePerm(s.Store, permission.InspFileUpload, access.LevelAll))
	g.GET("/:id/files", s.handleListFiles,
		access.RequirePerm(s.Store, permission.InspTaskView, access.LevelRead))

	// —— D5 出结论 / 处置 / 让步双签（四个点、双签两个入口）——
	g.POST("/:id/conclusion", s.handleConclusion,
		access.RequirePerm(s.Store, permission.InspConclusion, access.LevelAll))
	g.POST("/:id/disposition", s.handleDisposition,
		access.RequirePerm(s.Store, permission.InspDisposition, access.LevelAll))
	g.POST("/:id/concession/qc-sign", s.handleQcSign,
		access.RequirePerm(s.Store, permission.InspConcessionQcSign, access.LevelAll))
	g.POST("/:id/concession/dept-sign", s.handleDeptSign,
		access.RequirePerm(s.Store, permission.InspConcessionDeptSign, access.LevelAll))

	// —— D6 紧急放行：发起 ≠ 审批（两个权限点、两个级别）——
	g.POST("/urgent-release/init", s.handleUrgentInit,
		access.RequirePerm(s.Store, permission.InspUrgentReleaseInit, access.LevelInit))
	g.POST("/urgent-release/approve", s.handleUrgentApprove,
		access.RequirePerm(s.Store, permission.InspUrgentReleaseApprove, access.LevelApprove))
	g.GET("/urgent-release", s.handleUrgentGet,
		access.RequirePerm(s.Store, permission.InspTaskView, access.LevelRead))

	// —— D7 复检（insp.scope.edit）+ 修正（insp.result.correct）——
	g.POST("/:id/recheck", s.handleRecheck,
		access.RequirePerm(s.Store, permission.InspScopeEdit, access.LevelAll))
	g.POST("/:id/correct", s.handleCorrect,
		access.RequirePerm(s.Store, permission.InspResultCorrect, access.LevelAll))

	// —— 前端按权限显隐用的「我的检测权限摘要」（登录即可，无新增权限点）——
	g.GET("/perm-summary", s.handleInspPermSummary, access.RequireLogin())
}

// ---- D1 任务列表 ----

func (s *Server) handleListInspTasks(c echo.Context) error {
	rows, err := s.Store.ListInspTasks(c.Request().Context(),
		strings.TrimSpace(c.QueryParam("state")))
	if err != nil {
		return inspErr(err)
	}
	if rows == nil {
		rows = []store.InspTaskRow{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

// ---- D2 建单 / 清单 ----

func (s *Server) handleCreateInsp(c echo.Context) error {
	var in store.CreateInspInput
	if err := decodeBody(c, &in); err != nil {
		return err
	}
	row, err := s.Store.CreateInspection(c.Request().Context(), in, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"created": true, "row": row,
	})
}

func (s *Server) handleGetInsp(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.GetInspection(c.Request().Context(), id)
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": row})
}

func (s *Server) handleListItems(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListInspectionItems(c.Request().Context(), id)
	if err != nil {
		return inspErr(err)
	}
	todo := 0
	for _, r := range rows {
		if r.State == store.InspStateTodo {
			todo++
		}
	}
	if rows == nil {
		rows = []store.InspResultRow{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "todo": todo, "rows": rows,
	})
}

func (s *Server) handleAddItems(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		ItemIDs []int64 `json:"item_ids"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	rows, err := s.Store.AddInspectionItems(c.Request().Context(), id, body.ItemIDs, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows,
	})
}

func (s *Server) handleDeleteItem(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	itemID, err := strconv.ParseInt(c.Param("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "itemId 必须是正整数")
	}
	if err := s.Store.DeleteInspectionItem(c.Request().Context(), id, itemID, mdActor(c)); err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"deleted": true})
}

// ---- D3 结果录入 ----

func (s *Server) handleRecordResult(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	itemID, err := strconv.ParseInt(c.Param("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "itemId 必须是正整数")
	}
	var body struct {
		State     string   `json:"state"`
		ValueNum  *float64 `json:"value_num"`
		ValueText string   `json:"value_text"`
		Unit      string   `json:"unit"`
		Remark    string   `json:"remark"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.RecordInspectionResult(c.Request().Context(), store.RecordInspInput{
		InspectionID: id, ItemID: itemID,
		State: body.State, ValueNum: body.ValueNum,
		ValueText: body.ValueText, Unit: body.Unit, Remark: body.Remark,
	}, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"saved": true, "row": row})
}

// ---- D4 附件 ----

// attachMaxBytes 当前生效的单文件上限字节数（cfg 未注入 ⇒ 20MB 兜底）。
func (s *Server) attachMaxBytes() int64 {
	mb := defaultAttachMaxMB
	if s.Cfg != nil && s.Cfg.AttachMaxMB > 0 {
		mb = s.Cfg.AttachMaxMB
	}
	return int64(mb) << 20
}

func (s *Server) handleUploadFile(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	if _, err := s.Store.GetInspection(c.Request().Context(), id); err != nil {
		return inspErr(err)
	}

	maxBytes := s.attachMaxBytes()
	// ★ 超限先拒后写：Content-Length 预检 + MaxBytesReader（读完整文件前拒绝，不留半截文件）。
	//	预留 4KB 给 multipart 框架开销（边界/头部），文件本体大小另按 written 精确复核。
	const attachSlack = 4096
	roundLimit := maxBytes + attachSlack
	if c.Request().ContentLength > roundLimit {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("附件超过单文件上限 %dMB", maxBytes>>20))
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, roundLimit)

	file, header, err := c.Request().FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "request body too large") {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("附件超过单文件上限 %dMB", maxBytes>>20))
		}
		return echo.NewHTTPError(http.StatusBadRequest, "缺少 multipart 字段 file："+err.Error())
	}
	defer file.Close()

	attachDir := "/srv/jx-lab-trace/attachments"
	if s.Cfg != nil && strings.TrimSpace(s.Cfg.AttachDir) != "" {
		attachDir = s.Cfg.AttachDir
	}
	if err := os.MkdirAll(attachDir, 0o755); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "附件目录创建失败")
	}

	// ★ 库中只存路径：落盘名带检测单 id + 时间戳，避免重名覆盖与路径穿越
	safeName := sanitizeFileName(header.Filename)
	diskName := fmt.Sprintf("insp%d_%d_%s", id, time.Now().UnixNano(), safeName)
	diskPath := filepath.Join(attachDir, diskName)

	dst, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "附件落盘失败")
	}
	written, copyErr := io.Copy(dst, file)
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(diskPath) // 失败不留半截文件
		if strings.Contains(fmt.Sprint(copyErr), "request body too large") {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("附件超过单文件上限 %dMB", maxBytes>>20))
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "附件写入失败")
	}
	// 文件本体精确复核（multipart 开销不计入）：超限 ⇒ 删盘 + 413
	if written > maxBytes {
		_ = os.Remove(diskPath)
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("附件超过单文件上限 %dMB", maxBytes>>20))
	}

	row, err := s.Store.AddInspectionFile(c.Request().Context(), id,
		safeName, diskPath, header.Header.Get("Content-Type"), written, mdActor(c))
	if err != nil {
		_ = os.Remove(diskPath)
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"uploaded": true, "row": row, "size": written,
	})
}

// sanitizeFileName 去路径、去控制字符（保留中文等业务可读字符）。
func sanitizeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func (s *Server) handleListFiles(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	rows, err := s.Store.ListInspectionFiles(c.Request().Context(), id)
	if err != nil {
		return inspErr(err)
	}
	if rows == nil {
		rows = []store.InspFileRow{}
	}
	return c.JSON(http.StatusOK, map[string]interface{}{
		"count": len(rows), "rows": rows, "max_mb": s.attachMaxBytes() >> 20,
	})
}

// ---- D5 出结论 / 处置 / 双签 ----

func (s *Server) handleConclusion(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Conclusion     string `json:"conclusion"`
		DefectDesc     string `json:"defect_desc"`
		Remark         string `json:"remark"`
		AuthorizedBy   string `json:"authorized_by"`
		CustNotifiedAt string `json:"cust_notified_at"`
		CustContact    string `json:"cust_contact"`
		CustChannel    string `json:"cust_channel"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.SetConclusion(c.Request().Context(), store.ConclusionInput{
		InspectionID: id, Conclusion: body.Conclusion, DefectDesc: body.DefectDesc,
		Remark: body.Remark, AuthorizedBy: body.AuthorizedBy,
		CustNotifiedAt: body.CustNotifiedAt, CustContact: body.CustContact,
		CustChannel: body.CustChannel,
	}, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"saved": true, "row": row})
}

func (s *Server) handleDisposition(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	var body struct {
		Disposition string `json:"disposition"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	row, err := s.Store.SetDisposition(c.Request().Context(), id, body.Disposition, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"saved": true, "row": row})
}

// handleQcSign / handleDeptSign —— ★ 双签必须两个入口（两个权限点分别落给 qc 与 production）。
func (s *Server) handleQcSign(c echo.Context) error {
	return s.signConcession(c, "qc")
}

func (s *Server) handleDeptSign(c echo.Context) error {
	return s.signConcession(c, "dept")
}

func (s *Server) signConcession(c echo.Context, who string) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.SignConcession(c.Request().Context(), id, who, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"signed": true, "row": row})
}

// ---- D6 紧急放行 ----

func (s *Server) handleUrgentInit(c echo.Context) error {
	var body struct {
		Entity   string `json:"entity"`
		EntityID int64  `json:"entity_id"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	st, err := s.Store.UrgentReleaseInit(c.Request().Context(),
		body.Entity, body.EntityID, body.Reason, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"initiated": true, "row": st})
}

func (s *Server) handleUrgentApprove(c echo.Context) error {
	var body struct {
		Entity   string `json:"entity"`
		EntityID int64  `json:"entity_id"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(c, &body); err != nil {
		return err
	}
	st, err := s.Store.UrgentReleaseApprove(c.Request().Context(),
		body.Entity, body.EntityID, body.Reason, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"approved": true, "row": st})
}

func (s *Server) handleUrgentGet(c echo.Context) error {
	entity := strings.TrimSpace(c.QueryParam("entity"))
	idStr := strings.TrimSpace(c.QueryParam("entity_id"))
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "entity_id 须为正整数")
	}
	st, err := s.Store.GetUrgentRelease(c.Request().Context(), entity, id)
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"row": st})
}

// ---- D7 复检 / 修正 ----

func (s *Server) handleRecheck(c echo.Context) error {
	id, err := pathID(c)
	if err != nil {
		return err
	}
	row, err := s.Store.RecheckInspection(c.Request().Context(), id, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

func (s *Server) handleCorrect(c echo.Context) error {
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
	row, err := s.Store.CorrectInspection(c.Request().Context(), id, body.Reason, mdActor(c))
	if err != nil {
		return inspErr(err)
	}
	return c.JSON(http.StatusOK, map[string]interface{}{"created": true, "row": row})
}

// ---- 我的检测权限摘要（前端按权限显隐动作按钮）----

func (s *Server) handleInspPermSummary(c echo.Context) error {
	p := access.PrincipalFrom(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "未登录")
	}
	out := map[string]string{}
	for _, code := range []permission.Code{
		permission.InspTaskView,
		permission.InspScopeEdit,
		permission.InspResultEntry,
		permission.InspResultCorrect,
		permission.InspFileUpload,
		permission.InspConclusion,
		permission.InspDisposition,
		permission.InspConcessionQcSign,
		permission.InspConcessionDeptSign,
		permission.InspUrgentReleaseInit,
		permission.InspUrgentReleaseApprove,
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
//	404：单据/对象不存在；409：业务状态冲突（已录/已结论/已签/已作废/未发起…）；
//	403：服务层同人校验；400：输入不合法（含让步四字段缺失）。
func inspErr(err error) error {
	switch {
	case errors.Is(err, store.ErrInspNotFound),
		errors.Is(err, store.ErrInspTargetUnknown):
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrUrgentSameActor):
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrInspState),
		errors.Is(err, store.ErrInspResultRecorded),
		errors.Is(err, store.ErrInspConclusionSet),
		errors.Is(err, store.ErrInspDispositionSet),
		errors.Is(err, store.ErrInspNeedConclusion),
		errors.Is(err, store.ErrInspSigned),
		errors.Is(err, store.ErrInspSeqOverflow),
		errors.Is(err, store.ErrInspVoided),
		errors.Is(err, store.ErrUrgentNotInit),
		errors.Is(err, store.ErrUrgentAlready),
		errors.Is(err, store.ErrSeqBusy):
		return echo.NewHTTPError(http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInspBadInput),
		errors.Is(err, store.ErrInspNoSample),
		errors.Is(err, store.ErrInspConcessionIncomplete),
		errors.Is(err, store.ErrInspBadDisposition),
		errors.Is(err, store.ErrInspVoidReason),
		errors.Is(err, store.ErrInspItemInvalid):
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	default:
		return err
	}
}
