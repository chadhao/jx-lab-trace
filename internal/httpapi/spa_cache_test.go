package httpapi

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/chadhao/jx-lab-trace/internal/config"
	"github.com/chadhao/jx-lab-trace/internal/webui"
	"github.com/labstack/echo/v4"
)

// TestSPA_CacheHeadersAndContentType —— N-017 #4 复核回归（**纯单测**，不依赖数据库）。
//
//	① 入口 index.html 与 SPA 兜底必须 no-cache：
//	   原实现不设任何缓存头 ⇒ 浏览器启发式缓存 ⇒ **发了新版本用户还在跑旧前端**；
//	② 带内容哈希的 assets/* 长缓存 immutable；
//	③ SPA 兜底必须按 text/html 吐出：原实现走 mimeOf(无扩展名路径) ⇒
//	   application/octet-stream ⇒ 浏览器把深链当文件下载（复核中发现，已修）。
func TestSPA_CacheHeadersAndContentType(t *testing.T) {
	// 从内嵌产物里**动态**找一个带内容哈希的文件（不写死文件名：build.sh 重建后哈希会变）
	entries, err := fs.Glob(webui.Files, "dist/assets/*")
	if err != nil {
		t.Fatalf("枚举内嵌 assets 失败: %v", err)
	}
	hashed := ""
	for _, p := range entries {
		if strings.Contains(path.Base(p), "-") {
			hashed = strings.TrimPrefix(p, "dist/")
			break
		}
	}
	if hashed == "" {
		t.Fatal("内嵌 dist/assets 下没有带内容哈希的产物（先跑 scripts/build.sh）")
	}
	// 按真实扩展名取期望 Content-Type（js / css 都带哈希，动态判定不写死）
	wantHashCT := "text/javascript"
	if strings.HasSuffix(hashed, ".css") {
		wantHashCT = "text/css"
	}

	// 只挂 SPA 路由：handleSPA 不触库 ⇒ 纯单测可在本机跑（docs/05）
	s := &Server{Cfg: &config.Config{}}
	e := echo.New()
	e.GET("/*", s.handleSPA)

	cases := []struct {
		name    string
		reqPath string
		wantCC  string
		wantCT  string
	}{
		{"入口 /", "/", "no-cache, must-revalidate", "text/html"},
		{"入口 /index.html", "/index.html", "no-cache, must-revalidate", "text/html"},
		{"SPA 兜底（无扩展名深链）", "/trace", "no-cache, must-revalidate", "text/html"},
		{"带哈希产物", "/" + hashed, "public, max-age=31536000, immutable", wantHashCT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.reqPath, nil)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("应 200，实际 %d", rec.Code)
			}
			if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, tc.wantCC) {
				t.Fatalf("Cache-Control 应含 %q，实际 %q", tc.wantCC, cc)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCT) {
				t.Fatalf("Content-Type 应含 %q，实际 %q", tc.wantCT, ct)
			}
		})
	}
}
