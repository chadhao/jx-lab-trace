package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestTouchSession_ConcurrentValidity —— N-017 复核 #1 的并发回归（2026-10-10）。
//
//	① 并发 N 次 TouchSession（**含「临近过期」真写窗口**）⇒ 全部 ok=true、err=nil。
//	   （原实现把「写没命中 / 写报错」当会话无效 ⇒ 并发下随机 401；此为其反向断言。）
//	② 撤销 / 过期 / 不存在 ⇒ ok=false —— 收窄写条件**不等于**放松安全性。
//	③ 续期及时性：剩余有效期 < renewBefore 即真写库 ⇒ expires_at 被推到 expiresAt，
//	   不会等到快过期才续。
//
// ★ DB 集成（docs/05）：本机未设 JX_TEST_DB=1 时 Skip（有说明，非静默）；
//
//	在测试服务器上以 JX_TEST_DB=1 执行。
func TestTouchSession_ConcurrentValidity(t *testing.T) {
	st := migrateForTest(t)
	ctx := context.Background()
	openID := "ou_test_touch_conc"
	t.Cleanup(func() {
		_, _ = st.DB().ExecContext(ctx, `DELETE FROM s_session WHERE open_id = ?`, openID)
	})

	newSID := func() string {
		t.Helper()
		sid, err := NewSessionID()
		if err != nil {
			t.Fatalf("生成会话 ID 失败: %v", err)
		}
		return sid
	}

	// —— 场景 A：会话处于续期窗口内（剩余 10 分钟 < renewBefore 1 小时）⇒ 并发真写库 ——
	now := time.Now()
	sidA := newSID()
	expA := now.Add(10 * time.Minute)
	if err := st.CreateSession(ctx, sidA, openID, "127.0.0.1", expA); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	touchConcurrent := func(sid string, now, expiresAt, renewBefore time.Time) []error {
		t.Helper()
		const n = 16
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ok, err := st.TouchSession(ctx, sid, now, expiresAt, renewBefore)
				if err != nil {
					errs[i] = err
					return
				}
				if !ok {
					errs[i] = errNotValid // 并发有效会话不得被判无效（随机 401 的根源）
				}
			}(i)
		}
		wg.Wait()
		return errs
	}
	for i, err := range touchConcurrent(sidA, now, now.Add(2*time.Hour), now.Add(time.Hour)) {
		if err != nil {
			t.Fatalf("并发 TouchSession #%d 失败（有效会话必须恒 ok=true）: %v", i, err)
		}
	}
	// ③ 续期及时：并发后 expires_at 应已被推到 ≈ now+2h（不是仍停在 now+10m）
	sessA, err := st.LoadSession(ctx, sidA)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if !sessA.ExpiresAt.After(now.Add(time.Hour)) {
		t.Fatalf("续期不及时：expires_at 应 ≥ now+1h，实际 %v", sessA.ExpiresAt)
	}

	// —— 场景 B：远未到续期窗口（剩余 3 小时 > renewBefore 1 小时）⇒ 不写库，但仍判有效 ——
	sidB := newSID()
	expB := time.Now().Add(3 * time.Hour)
	if err := st.CreateSession(ctx, sidB, openID, "127.0.0.1", expB); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	for i, err := range touchConcurrent(sidB, time.Now(), time.Now().Add(2*time.Hour), time.Now().Add(time.Hour)) {
		if err != nil {
			t.Fatalf("窗口外并发 TouchSession #%d 失败: %v", i, err)
		}
	}
	sessB, err := st.LoadSession(ctx, sidB)
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	// 窗口外必须**不写**：expires_at 仍是初始值（容差 1 秒）
	if d := sessB.ExpiresAt.Sub(expB); d < -time.Second || d > time.Second {
		t.Fatalf("窗口外不应写库：expires_at 应保持 %v，实际 %v", expB, sessB.ExpiresAt)
	}

	// —— 场景 C：撤销 ⇒ ok=false（即使仍在续期窗口内）——
	if err := st.RevokeSession(ctx, sidA); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if ok, err := st.TouchSession(ctx, sidA, time.Now(), time.Now().Add(time.Hour), time.Now().Add(30*time.Minute)); err != nil || ok {
		t.Fatalf("已撤销会话必须 ok=false err=nil，实际 ok=%v err=%v", ok, err)
	}

	// —— 场景 D：已过期 ⇒ ok=false ——
	sidD := newSID()
	if err := st.CreateSession(ctx, sidD, openID, "127.0.0.1", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("建过期会话失败: %v", err)
	}
	if ok, err := st.TouchSession(ctx, sidD, time.Now(), time.Now().Add(time.Hour), time.Now().Add(30*time.Minute)); err != nil || ok {
		t.Fatalf("已过期会话必须 ok=false err=nil，实际 ok=%v err=%v", ok, err)
	}

	// —— 场景 E：不存在 ⇒ ok=false、err=nil（不是错误）——
	sidE := newSID()
	if ok, err := st.TouchSession(ctx, sidE, time.Now(), time.Now().Add(time.Hour), time.Now().Add(30*time.Minute)); err != nil || ok {
		t.Fatalf("不存在的会话必须 ok=false err=nil，实际 ok=%v err=%v", ok, err)
	}

	// —— 场景 F：库错误 ⇒ 如实上报（err≠nil），不得伪装成"会话无效"——
	//   （关闭连接池模拟 DB 故障：若吞成 ok=false/err=nil，故障会被掩盖成 401）
	if err := st.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}
	if ok, err := st.TouchSession(ctx, sidA, time.Now(), time.Now().Add(time.Hour), time.Now().Add(30*time.Minute)); err == nil || ok {
		t.Fatalf("库错误必须如实上报：应 ok=false err≠nil，实际 ok=%v err=%v", ok, err)
	}
}

// errNotValid 是并发断言用的哨兵错误（会话有效却被判无效）。
var errNotValid = errors.New("有效会话被误判为无效（随机 401 回归）")
