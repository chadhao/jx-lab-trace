package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chadhao/jx-lab-trace/internal/access"
)

// ══════════════════════════════════════════════════════════════════════════
// D3/D4 · DB 集成测试（JX_TEST_DB=1；本机按 docs/05 跳过）。
// ══════════════════════════════════════════════════════════════════════════

const (
	testOrgOpenID  = "ou_test_org_sync_a"
	testLocalOnly  = "ou_test_org_local_only"
	testLeaveID    = "ou_test_org_leave_user"
	testKeepRoleID = "ou_test_org_keep_b"
	testCreatedID  = "ou_test_org_created_c"
)

// newTestServer 装一个 Store/Feishu 可控的完整 handler。
func newTestServer(t *testing.T, e *env, feishu *access.FeishuClient) http.Handler {
	t.Helper()
	s := &Server{
		Cfg: e.cfg, Store: e.st, Version: "test", Feishu: feishu,
		Opts: access.Opts{TTL: e.ttl, CookieName: cookieName},
	}
	return s.Handler()
}

// devLoginCookie 在指定 handler 上走 dev 免登，返回全部 cookie。
func devLoginCookie(t *testing.T, h http.Handler, openID string) []*http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/dev-login",
		strings.NewReader(`{"open_id":"`+openID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dev 登录失败 %d: %s", rec.Code, rec.Body.String())
	}
	cks := rec.Result().Cookies()
	if len(cks) == 0 {
		t.Fatal("dev 登录未下发 cookie")
	}
	return cks
}

// newFakeFeishu：假 token + 假部门 + 假用户列表（形状断言在 access 包）。
func newFakeFeishu(t *testing.T, users map[string]string) *access.FeishuClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"t-fake","expire":7200}`))
	})
	mux.HandleFunc("/depts/0/children", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"has_more":false,"items":[]}}`))
	})
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		items := make([]map[string]string, 0, len(users))
		for id, name := range users {
			items = append(items, map[string]string{"open_id": id, "name": name})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0,
			"data": map[string]interface{}{"has_more": false, "items": items},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &access.FeishuClient{
		AppID: "cli_t", AppSecret: "sec_t",
		TenantTokenURLOverride:   srv.URL + "/token",
		DeptChildrenURLOverride:  srv.URL + "/depts/%s/children",
		DeptUsersURLOverrideBase: srv.URL + "/users",
	}
}

// TC-D3：同步只 upsert 不删除、不绑角色；幂等（二次全 skipped）。
func TestTC_OrgSync_UpsertOnlyIdempotent(t *testing.T) {
	st := requireDB(t)
	e := newEnv(t, time.Hour)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	bindRole(t, st, testKeepRoleID, "receiver")

	// 预置：本地独有账号（不在通讯录里）——同步绝不能删
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO m_user (open_id, name, status, created_by) VALUES (?, '本地保留', '启用', 'test')`,
		testLocalOnly); err != nil {
		t.Fatalf("预置本地账号失败: %v", err)
	}

	fake := newFakeFeishu(t, map[string]string{
		testOrgOpenID:  "张三",
		testKeepRoleID: "李四",
	})
	h := newTestServer(t, e, fake)
	cks := devLoginCookie(t, h, testAdminOpenID)

	doSync := func() (int, map[string]interface{}) {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/org/sync", nil)
		for _, c := range cks {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// ① 首次同步
	code, out := doSync()
	if code != http.StatusOK {
		t.Fatalf("同步应 200，实际 %d body=%v", code, out)
	}
	if created, _ := out["created"].(float64); created < 1 {
		t.Fatalf("首次应有 created>=1，实际 %v", out)
	}
	if out["deleted"] != float64(0) {
		t.Fatalf("★deleted 必须恒为 0，实际 %v", out["deleted"])
	}
	if out["roles_touched"] != false {
		t.Fatalf("★roles_touched 必须 false，实际 %v", out["roles_touched"])
	}

	// ② 账号行已建；本地保留账号未删；角色绑定未动；通讯录用户未绑角色
	var n int
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM m_user WHERE open_id = ?`, testOrgOpenID).Scan(&n)
	if n != 1 {
		t.Fatal("同步应建账号行")
	}
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM m_user WHERE open_id = ?`, testLocalOnly).Scan(&n)
	if n != 1 {
		t.Fatal("★★同步绝不能删除本地已有账号")
	}
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ? AND role_code='receiver'`, testKeepRoleID).Scan(&n)
	if n != 1 {
		t.Fatal("★★同步绝不能动 s_user_role")
	}
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, testOrgOpenID).Scan(&n)
	if n != 0 {
		t.Fatalf("★★同步不得自动绑角色，实际绑定 %d", n)
	}

	// ③ 二次同步：幂等
	code2, out2 := doSync()
	if code2 != http.StatusOK {
		t.Fatalf("二次同步应 200，实际 %d", code2)
	}
	if created, _ := out2["created"].(float64); created != 0 {
		t.Fatalf("★幂等：二次 created 应 0，实际 %v", out2)
	}
	if skipped, _ := out2["skipped"].(float64); skipped < 2 {
		t.Fatalf("二次应至少 skipped 2，实际 %v", out2)
	}
}

// TC-D4a：created 事件 ⇒ upsert 幂等、不绑角色。
func TestTC_FeishuEvent_UserCreated_Upsert幂等(t *testing.T) {
	st := requireDB(t)
	e := newEnv(t, time.Hour)
	e.cfg.FeishuEncryptKey = "k1"
	h := newTestServer(t, e, nil)

	body := []byte(`{"schema":"2.0","header":{"event_type":"contact.user.created_v3","token":""},` +
		`"event":{"object":{"open_id":"` + testCreatedID + `","name":"王五"}}}`)
	post := func() (int, map[string]interface{}) {
		req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(body)))
		req.Header.Set("X-Lark-Request-Timestamp", "9")
		req.Header.Set("X-Lark-Request-Nonce", "nn")
		req.Header.Set("X-Lark-Signature", signFeishu("9", "nn", "k1", body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := post()
	if code != http.StatusOK || out["result"] != "ok" {
		t.Fatalf("created 应 200/ok，实际 %d %v", code, out)
	}
	if c, _ := out["created"].(float64); c != 1 {
		t.Fatalf("首次 created 应 1，实际 %v", out)
	}

	code2, out2 := post()
	if code2 != http.StatusOK {
		t.Fatalf("二次应 200，实际 %d", code2)
	}
	if out2["created"] != float64(0) || out2["skipped"] != float64(1) {
		t.Fatalf("★created 幂等应 skipped=1 created=0，实际 %v", out2)
	}

	var n int
	_ = st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM s_user_role WHERE open_id = ?`, testCreatedID).Scan(&n)
	if n != 0 {
		t.Fatal("created 事件不得绑角色")
	}
}

// TC-D4b：deleted 事件 ⇒ 停用 + 在线会话当场失效 + 拒再登录 + 审计。
func TestTC_FeishuEvent_UserDeleted_会话失效拒再登录(t *testing.T) {
	st := requireDB(t)
	_, _ = st.DB().ExecContext(context.Background(),
		`DELETE FROM s_user_role WHERE open_id = ?`, testLeaveID)
	bindRole(t, st, testLeaveID, "receiver")
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(context.Background(), `DELETE FROM s_user_role WHERE open_id = ?`, testLeaveID)
		_, _ = st.DB().ExecContext(context.Background(), `UPDATE m_user SET status='启用' WHERE open_id = ?`, testLeaveID)
		_, _ = st.DB().ExecContext(context.Background(), `DELETE FROM m_user WHERE open_id = ?`, testLeaveID)
		_, _ = st.DB().ExecContext(context.Background(), `DELETE FROM s_session WHERE open_id = ?`, testLeaveID)
	})

	e := newEnv(t, time.Hour)
	ck := e.login(testLeaveID)

	// 会话有效
	if resp, _ := e.do("GET", "/api/me", ck, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("登录后 /api/me 应 200，实际 %d", resp.StatusCode)
	}

	e.cfg.FeishuEncryptKey = "k2"
	h := newTestServer(t, e, nil)

	evt := []byte(`{"schema":"2.0","header":{"event_type":"contact.user.deleted_v3","token":""},` +
		`"event":{"object":{"open_id":"` + testLeaveID + `"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/feishu/event", strings.NewReader(string(evt)))
	req.Header.Set("X-Lark-Request-Timestamp", "10")
	req.Header.Set("X-Lark-Request-Nonce", "nx")
	req.Header.Set("X-Lark-Signature", signFeishu("10", "nx", "k2", evt))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deleted 应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["disabled"] != true {
		t.Fatalf("应 disabled=true，实际 %v", out)
	}
	if rev, _ := out["sessions_revoked"].(float64); rev < 1 {
		t.Fatalf("★在线会话必须当场失效，实际 sessions_revoked=%v", out)
	}

	// ① 原会话立即失效
	if resp, _ := e.do("GET", "/api/me", ck, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("★离职后原会话应 401，实际 %d", resp.StatusCode)
	}

	// ② 拒再登录（即使角色还在）
	resp3, body3 := e.do("POST", "/api/auth/dev-login", "", `{"open_id":"`+testLeaveID+`"}`)
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("★离职账号再登录应 403，实际 %d body=%v", resp3.StatusCode, body3)
	}

	// ③ 审计留痕
	audits, err := st.ListAudit(context.Background(), 100)
	if err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Entity == "m_user" && a.Action == "disable" && a.ActorOpenID == "feishu" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("★★离职停用必须写审计（m_user/disable by feishu）")
	}
}
