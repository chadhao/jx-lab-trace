package access

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════
// D3 组织同步**对外报文形状断言**（任务包：对外报文必须配形状断言用例）。
// 假飞书 = httptest；断言 Authorization / 字段名 / 分页参数。
// ══════════════════════════════════════════════════════════════════════════

func TestFeishu_TenantAccessToken_报文形状(t *testing.T) {
	var gotCT string
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","tenant_access_token":"t-abc","expire":7200}`))
	}))
	defer srv.Close()

	c := &FeishuClient{AppID: "cli_x", AppSecret: "sec_y", TenantTokenURLOverride: srv.URL}
	tok, err := c.TenantAccessToken(context.Background())
	if err != nil {
		t.Fatalf("TenantAccessToken 失败: %v", err)
	}
	if tok != "t-abc" {
		t.Errorf("token 解析错: %q", tok)
	}
	if body["app_id"] != "cli_x" || body["app_secret"] != "sec_y" {
		t.Errorf("★ 请求体应为 app_id+app_secret，实际 %v", body)
	}
	if !strings.Contains(gotCT, "application/json") {
		t.Errorf("Content-Type 应含 application/json，实际 %q", gotCT)
	}
}

func TestFeishu_ListUsersByDepartment_报文形状与分页(t *testing.T) {
	var calls []string
	var auths []string
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.String())
		auths = append(auths, r.Header.Get("Authorization"))
		page++
		if page == 1 {
			_, _ = w.Write([]byte(`{"code":0,"data":{"has_more":true,"page_token":"p2","items":[{"open_id":"ou_1","name":"甲"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"has_more":false,"items":[{"open_id":"ou_2","name":"乙"}]}}`))
	}))
	defer srv.Close()

	c := &FeishuClient{AppID: "a", AppSecret: "b", DeptUsersURLOverrideBase: srv.URL}
	users, err := c.ListUsersByDepartment(context.Background(), "t-1", "od-root")
	if err != nil {
		t.Fatalf("ListUsersByDepartment 失败: %v", err)
	}
	if len(users) != 2 || users[0].OpenID != "ou_1" || users[1].Name != "乙" {
		t.Errorf("分页合并错: %+v", users)
	}
	// ★ Authorization = Bearer tenant_token（应用身份）
	for i, a := range auths {
		if a != "Bearer t-1" {
			t.Errorf("★第%d次请求 Authorization 应为 Bearer t-1，实际 %q", i+1, a)
		}
	}
	// ★ 第一页：department_id + department_id_type=open_department_id + 无 page_token
	if !strings.Contains(calls[0], "department_id=od-root") ||
		!strings.Contains(calls[0], "department_id_type=open_department_id") ||
		!strings.Contains(calls[0], "user_id_type=open_id") {
		t.Errorf("★首页 query 错: %s", calls[0])
	}
	if strings.Contains(calls[0], "page_token=") {
		t.Errorf("★首页不应带 page_token: %s", calls[0])
	}
	// ★ 第二页：带 page_token=p2
	if !strings.Contains(calls[1], "page_token=p2") {
		t.Errorf("★第二页应带 page_token=p2: %s", calls[1])
	}
}

func TestFeishu_ListChildDepartments_报文形状(t *testing.T) {
	var gotPath, gotAuth string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"code":0,"data":{"has_more":false,"items":[{"open_department_id":"od-a","name":"部门A"}]}}`))
	}))
	defer srv.Close()

	c := &FeishuClient{AppID: "a", AppSecret: "b", DeptChildrenURLOverride: srv.URL + "/depts/%s"}
	kids, err := c.ListChildDepartments(context.Background(), "t-9", "od-parent")
	if err != nil {
		t.Fatalf("ListChildDepartments 失败: %v", err)
	}
	if len(kids) != 1 || kids[0].OpenDepartmentID != "od-a" {
		t.Errorf("解析错: %+v", kids)
	}
	if gotPath != "/od-parent" && !strings.Contains(gotPath, "od-parent") {
		t.Errorf("路径应含部门 ID，实际 %s", gotPath)
	}
	if gotAuth != "Bearer t-9" {
		t.Errorf("★Authorization 应为 Bearer t-9，实际 %q", gotAuth)
	}
	if gotQuery.Get("department_id_type") != "open_department_id" {
		t.Errorf("★department_id_type 应为 open_department_id，实际 %q", gotQuery.Get("department_id_type"))
	}
	if gotQuery.Get("fetch_child") != "false" {
		t.Errorf("★fetch_child 应为 false（列直接子部门），实际 %q", gotQuery.Get("fetch_child"))
	}
}

func TestFeishu_TenantAccessToken_被拒必须报错(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10003,"msg":"invalid app_id"}`))
	}))
	defer srv.Close()
	c := &FeishuClient{AppID: "bad", AppSecret: "x", TenantTokenURLOverride: srv.URL}
	if _, err := c.TenantAccessToken(context.Background()); err == nil {
		t.Fatal("被拒时必须报错，不得返回空 token 当成功")
	}
}
