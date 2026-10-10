package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSession_ConcurrentMeNoRandom401 —— N-017 #1 端到端并发回归（DB 集成）。
//
// 复现 2026-10-10 事故场景：首屏 Promise.all 多请求并发共用一个会话，
// 且会话处于**续期窗口内**（剩余 < TTL/2 ⇒ 每个请求都真写库）——
// 原实现把「写没命中 / 写报错」当会话无效 ⇒ **随机一个 401**。
//
//	① 并发 N 次 /api/me ⇒ 全部 200；
//	② 并发后续期及时（expires_at 被推到 ≈ now+TTL）；
//	③ 撤销会话后 ⇒ 401（收窄写条件不等于放松安全性）。
//
// ★ DB 集成（docs/05）：本机未设 JX_TEST_DB=1 时 Skip；测试服务器上执行。
func TestSession_ConcurrentMeNoRandom401(t *testing.T) {
	st := requireDB(t)
	bindRole(t, st, testAdminOpenID, "sysadmin")
	e := newEnv(t, time.Hour) // TTL=1h ⇒ renewBefore = now+30m
	ck := e.login(testAdminOpenID)
	ctx := context.Background()

	// 把会话压进续期窗口：expires_at = now+10m < renewBefore(now+30m) ⇒ 并发必写库
	sid := strings.TrimPrefix(ck, cookieName+"=")
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE s_session SET expires_at = ? WHERE session_id = ?`,
		time.Now().Add(10*time.Minute), sid); err != nil {
		t.Fatalf("压会话进续期窗口失败: %v", err)
	}

	// ① 并发访问
	const n = 16
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/me", nil)
			if err != nil {
				codes[i] = -1
				return
			}
			req.Header.Set("Cookie", ck)
			resp, err := e.srv.Client().Do(req)
			if err != nil {
				codes[i] = -1
				return
			}
			_ = resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("并发请求 #%d 应 200（不得随机 401），实际 %d", i, code)
		}
	}

	// ② 续期及时：expires_at 应已从 now+10m 被推到 ≈ now+1h
	sess, err := st.LoadSession(ctx, sid)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if !sess.ExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("续期不及时：expires_at 应 > now+30m，实际 %v", sess.ExpiresAt)
	}

	// ③ 撤销 ⇒ 401（安全不放松）
	if err := st.RevokeSession(ctx, sid); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	resp, _ := e.do("GET", "/api/me", ck, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已撤销会话访问 /api/me 应 401，实际 %d", resp.StatusCode)
	}
}
