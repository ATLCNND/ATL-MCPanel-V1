package httpapi

import (
	"fmt"
	"sync"
	"time"
)

// limiterConfig 限流参数。
type limiterConfig struct {
	maxFailures int           // 窗口内允许的失败次数
	window      time.Duration // 计数窗口
	lockout     time.Duration // 触发后锁定时长
}

// 登录限流策略：
//   - 按用户名严格限制（防定向爆破）
//   - 按来源 IP 宽松限制（防撞库），阈值远高于用户名维度，
//     避免面板位于 frp/反向代理之后时「单个攻击者锁死所有用户」
var (
	userLimiterCfg = limiterConfig{maxFailures: 5, window: 15 * time.Minute, lockout: 15 * time.Minute}
	ipLimiterCfg   = limiterConfig{maxFailures: 50, window: 10 * time.Minute, lockout: 5 * time.Minute}
)

const loginEntryTTL = time.Hour // 空闲条目清理阈值

// attemptInfo 单个 key 的失败记录。
type attemptInfo struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
	lastSeen    time.Time
}

// loginLimiter 登录失败限流器（内存实现）。
//
// 说明：进程重启后计数清零，属可接受的权衡（面板为单实例部署）；
// 如需跨重启持久化，可替换为数据库实现。
type loginLimiter struct {
	mu   sync.Mutex
	data map[string]*attemptInfo
	cfg  limiterConfig
}

func newLoginLimiter(cfg limiterConfig) *loginLimiter {
	return &loginLimiter{data: make(map[string]*attemptInfo), cfg: cfg}
}

// Allow 判断该 key 当前是否允许尝试登录。被锁定时返回剩余时长。
func (l *loginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	info, ok := l.data[key]
	if !ok {
		return true, 0
	}
	info.lastSeen = time.Now()

	if time.Now().Before(info.lockedUntil) {
		return false, time.Until(info.lockedUntil)
	}
	return true, 0
}

// RecordFailure 记录一次失败；达到阈值则锁定。
// 返回是否因此次失败触发锁定。
func (l *loginLimiter) RecordFailure(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	info, ok := l.data[key]
	if !ok || now.Sub(info.windowStart) > l.cfg.window {
		info = &attemptInfo{windowStart: now}
		l.data[key] = info
	}
	info.count++
	info.lastSeen = now

	if info.count >= l.cfg.maxFailures {
		info.lockedUntil = now.Add(l.cfg.lockout)
		info.count = 0
		info.windowStart = now
		return true
	}
	return false
}

// Reset 登录成功后清除该 key 的失败记录。
func (l *loginLimiter) Reset(key string) {
	l.mu.Lock()
	delete(l.data, key)
	l.mu.Unlock()
}

// Cleanup 清理长时间未活动的条目（避免内存无限增长）。
func (l *loginLimiter) Cleanup() (removed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-loginEntryTTL)
	for k, v := range l.data {
		if v.lastSeen.Before(cutoff) && time.Now().After(v.lockedUntil) {
			delete(l.data, k)
			removed++
		}
	}
	return removed
}

// retryAfterMessage 生成用户可读的等待提示。
func retryAfterMessage(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("登录尝试过于频繁，请 %d 秒后重试", sec)
	}
	return fmt.Sprintf("登录尝试过于频繁，请 %d 分钟后重试", (sec+59)/60)
}
