package httpapi

import (
	"testing"
	"time"
)

// testLimiter 使用用户名维度的策略构造限流器
func testLimiter() *loginLimiter { return newLoginLimiter(userLimiterCfg) }

func TestLimiterAllowsInitially(t *testing.T) {
	l := testLimiter()
	if ok, _ := l.Allow("u:alice"); !ok {
		t.Error("初始状态应允许登录")
	}
}

func TestLimiterLocksAfterMaxFailures(t *testing.T) {
	l := testLimiter()
	key := "u:alice"
	max := userLimiterCfg.maxFailures

	for i := 0; i < max-1; i++ {
		if locked := l.RecordFailure(key); locked {
			t.Fatalf("第 %d 次失败不应触发锁定", i+1)
		}
		if ok, _ := l.Allow(key); !ok {
			t.Fatalf("第 %d 次失败后仍应允许尝试", i+1)
		}
	}

	if locked := l.RecordFailure(key); !locked {
		t.Fatalf("第 %d 次失败应触发锁定", max)
	}
	ok, wait := l.Allow(key)
	if ok {
		t.Error("锁定后不应允许登录")
	}
	if wait <= 0 || wait > userLimiterCfg.lockout {
		t.Errorf("剩余锁定时间异常: %v", wait)
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l := testLimiter()
	key := "u:bob"
	max := userLimiterCfg.maxFailures

	for i := 0; i < max-1; i++ {
		l.RecordFailure(key)
	}
	l.Reset(key)
	if ok, _ := l.Allow(key); !ok {
		t.Error("重置后应允许登录")
	}
	// 重置后计数重新开始
	for i := 0; i < max-1; i++ {
		if locked := l.RecordFailure(key); locked {
			t.Fatalf("重置后第 %d 次失败不应锁定", i+1)
		}
	}
}

func TestLimiterIndependentKeys(t *testing.T) {
	l := testLimiter()
	for i := 0; i < userLimiterCfg.maxFailures; i++ {
		l.RecordFailure("u:alice")
	}
	if ok, _ := l.Allow("u:bob"); !ok {
		t.Error("其他用户不应被牵连")
	}
	if ok, _ := l.Allow("ip:1.2.3.4"); !ok {
		t.Error("不同 key 维度应独立计数")
	}
}

func TestIPLimiterIsMoreLenient(t *testing.T) {
	// IP 维度阈值必须显著高于用户名维度，否则面板位于代理之后时
	// 单个攻击者即可锁死所有用户
	if ipLimiterCfg.maxFailures <= userLimiterCfg.maxFailures*5 {
		t.Errorf("IP 限流阈值应远高于用户名阈值：ip=%d user=%d",
			ipLimiterCfg.maxFailures, userLimiterCfg.maxFailures)
	}
}

func TestLimiterCleanup(t *testing.T) {
	l := testLimiter()
	l.RecordFailure("u:stale")
	l.RecordFailure("u:fresh")

	l.mu.Lock()
	l.data["u:stale"].lastSeen = time.Now().Add(-2 * loginEntryTTL)
	l.data["u:stale"].windowStart = time.Now().Add(-2 * loginEntryTTL)
	l.mu.Unlock()

	if removed := l.Cleanup(); removed != 1 {
		t.Errorf("应清理 1 条过期记录，实际 %d", removed)
	}
	if _, ok := l.data["u:fresh"]; !ok {
		t.Error("活跃记录不应被清理")
	}
}

func TestLimiterCleanupKeepsLockedEntries(t *testing.T) {
	l := testLimiter()
	key := "u:locked"
	for i := 0; i < userLimiterCfg.maxFailures; i++ {
		l.RecordFailure(key)
	}
	l.mu.Lock()
	l.data[key].lastSeen = time.Now().Add(-2 * loginEntryTTL)
	l.mu.Unlock()

	if removed := l.Cleanup(); removed != 0 {
		t.Errorf("锁定中的记录不应被清理，实际清理 %d 条", removed)
	}
	if ok, _ := l.Allow(key); ok {
		t.Error("锁定状态应保持")
	}
}

func TestLimiterWindowExpiry(t *testing.T) {
	// 使用极短窗口验证窗口过期后计数重置
	l := newLoginLimiter(limiterConfig{maxFailures: 3, window: 20 * time.Millisecond, lockout: time.Minute})
	key := "u:win"
	l.RecordFailure(key)
	l.RecordFailure(key)
	time.Sleep(30 * time.Millisecond)
	if locked := l.RecordFailure(key); locked {
		t.Error("窗口过期后不应立即锁定")
	}
}

func TestRetryAfterMessage(t *testing.T) {
	if msg := retryAfterMessage(30 * time.Second); msg == "" {
		t.Error("应返回提示信息")
	}
	if msg := retryAfterMessage(5 * time.Minute); msg == "" {
		t.Error("应返回提示信息")
	}
}
