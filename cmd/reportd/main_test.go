package main

// reportd 行为单测（★ 纯单测：httptest.NewRecorder 直接打 handler，不起监听、
// 不连任何外部服务 ⇒ 本机可跑，符合 docs/05）。
//
// 覆盖：/healthz 200、命中 served/ ⇒ 200 text/html、未命中 ⇒ 404（撤销 / 过期
// 的自然结果）、路径穿越与短名拒绝、访问日志 TSV 落行与转义（防日志注入）、
// 仅回环监听（非回环拒绝启动）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T) (http.Handler, string, *os.File) {
	t.Helper()
	root := t.TempDir()
	served := filepath.Join(root, "served")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	logf, err := os.OpenFile(filepath.Join(root, "access.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("打开日志失败: %v", err)
	}
	t.Cleanup(func() { _ = logf.Close() })
	return newHandler(served, logf), root, logf
}

const testToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 字符 base64url

func TestHealthzOK(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("/healthz 应 200 + ok，实际 %d %q", rec.Code, rec.Body.String())
	}
}

func TestServedSnapshot200AndMissing404(t *testing.T) {
	h, root, _ := newTestHandler(t)
	page := "<!DOCTYPE html><html><body>快照</body></html>"
	if err := os.WriteFile(filepath.Join(root, "served", testToken+".html"),
		[]byte(page), 0o644); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	// 命中 ⇒ 200 text/html
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/"+testToken+".html", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("命中应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type 应 text/html，实际 %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "快照") {
		t.Fatalf("响应体应含快照内容，实际 %q", rec.Body.String())
	}

	// 未命中（撤销 / 过期后移出 served/）⇒ 404
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet,
		"/r/BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB.html", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("★★★ 未命中必须 404（不查库、纯看文件），实际 %d", rec2.Code)
	}
}

func TestRejectsTraversalAndShortName(t *testing.T) {
	h, root, _ := newTestHandler(t)
	// 在 served/ 之外放一个文件，试图穿越读取
	outside := filepath.Join(root, "secret.html")
	if err := os.WriteFile(outside, []byte("TOP-SECRETDATA"), 0o644); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}
	for _, p := range []string{
		"/r/short.html",               // token 太短（<32）
		"/r/" + testToken + ".txt",    // 非 .html
		"/r/" + testToken + "/x.html", // 带路径
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("★ %s 应 404，实际 %d", p, rec.Code)
		}
	}
	// 穿越尝试：ServeMux 可能先做路径清洗（301，body 只回显路径）⇒
	// 断言「不 200、不吐出 served/ 之外的文件内容」
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/r/../secret.html", nil))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "TOP-SECRETDATA") {
		t.Fatalf("★★★ 路径穿越不得成功（code=%d body=%q）", rec.Code, rec.Body.String())
	}
}

func TestAccessLogTSVAndEscaping(t *testing.T) {
	h, root, _ := newTestHandler(t)
	if err := os.WriteFile(filepath.Join(root, "served", testToken+".html"),
		[]byte("<html>x</html>"), 0o644); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/r/"+testToken+".html", nil)
	req.RemoteAddr = "198.51.100.20:54321"
	req.Header.Set("User-Agent", "evil\tua\nsecond")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", rec.Code)
	}

	// /healthz 不记访问日志
	hrec := httptest.NewRecorder()
	h.ServeHTTP(hrec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	b, err := os.ReadFile(filepath.Join(root, "access.log"))
	if err != nil {
		t.Fatalf("读访问日志失败: %v", err)
	}
	log := string(b)
	if log == "" {
		t.Fatalf("★★★ 访问报告必须落 access.log")
	}
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("★ /healthz 不应记日志，应恰 1 行，实际 %d：%q", len(lines), log)
	}
	f := strings.Split(lines[0], "\t")
	if len(f) != 5 {
		t.Fatalf("★★ TSV 应 5 列，实际 %d：%q", len(f), lines[0])
	}
	if f[1] != testToken || f[2] != "198.51.100.20" || f[3] != "200" {
		t.Fatalf("TSV 字段错乱: %q", f)
	}
	// ★ 转义：UA 内的制表符 / 换行被压平（防日志注入）
	if strings.Contains(lines[0][:len(f[0])+1+len(testToken)+1+len("198.51.100.20")+1+3], "\n") {
		t.Fatalf("TSV 行内不得含换行")
	}
	if strings.Contains(f[4], "\t") || strings.Contains(f[4], "\n") {
		t.Fatalf("★★★ UA 中的制表符 / 换行必须转义，实际 %q", f[4])
	}
}

func TestLoopbackOnly(t *testing.T) {
	if err := checkLoopback("127.0.0.1:18090"); err != nil {
		t.Fatalf("回环地址应放行: %v", err)
	}
	if err := checkLoopback("localhost:18090"); err != nil {
		t.Fatalf("localhost 应放行: %v", err)
	}
	for _, bad := range []string{"0.0.0.0:18090", ":18090", "192.168.10.50:18090"} {
		if err := checkLoopback(bad); err == nil {
			t.Fatalf("★★ 非回环 %q 必须拒绝启动", bad)
		}
	}
}

func TestValidSnapshotName(t *testing.T) {
	if !validSnapshotName(testToken + ".html") {
		t.Fatal("合法名应通过")
	}
	for _, bad := range []string{
		"../x.html", "/etc/passwd.html", "short.html",
		testToken + ".htm", "abc def" + strings.Repeat("x", 40) + ".html",
	} {
		if validSnapshotName(bad) {
			t.Fatalf("非法名应拒绝: %q", bad)
		}
	}
}
