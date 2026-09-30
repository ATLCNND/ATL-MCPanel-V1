package analysis

import (
	"fmt"
	"sync"
	"time"
)

// RateLimiter 按用户的滑动窗口限流（D1b）。
//
// 为什么必须做：
//  1. 用户填了自己的 key，一旦被脚本反复调用，**吃的是他自己的额度**；
//  2. 更麻烦的是**公益合作的额度**（LogShare）会被单个租户吃光，让所有人不可用；
//  3. 面板是"带凭据的固定出站请求方"，不加限速就等于给外部世界一个放大器。
//
// 计数对象是**每一次对外部提供方的调用**，包含重试与回退后的第二次调用 ——
// 否则"失败重试"会成为绕过限额的后门。
type RateLimiter struct {
	mu     sync.Mutex
	events map[string][]time.Time // key(userID) → 最近若干次调用的时间戳
	perMin int
	perDay int
	now    func() time.Time // 便于单测注入时间
}

// DefaultRateLimits 默认限额：一分钟 6 次、一天 200 次。
//
// 取值依据：一次分析要读日志、传几十 KB~几 MB、等几十秒，正常人手点不可能超过
// 每分钟 6 次；而"每人每天 200 次"足够真实排障，又不至于把公益额度打光。
const (
	DefaultPerMinute = 6
	DefaultPerDay    = 200
)

// NewRateLimiter 创建限流器（0 或负数表示用默认值）。
func NewRateLimiter(perMin, perDay int) *RateLimiter {
	if perMin <= 0 {
		perMin = DefaultPerMinute
	}
	if perDay <= 0 {
		perDay = DefaultPerDay
	}
	return &RateLimiter{
		events: map[string][]time.Time{},
		perMin: perMin,
		perDay: perDay,
		now:    time.Now,
	}
}

// Limits 返回当前限额（供界面展示）。
func (l *RateLimiter) Limits() (perMin, perDay int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.perMin, l.perDay
}

// SetLimits 调整限额（管理员在界面上改设置时调用）。
func (l *RateLimiter) SetLimits(perMin, perDay int) {
	if perMin <= 0 {
		perMin = DefaultPerMinute
	}
	if perDay <= 0 {
		perDay = DefaultPerDay
	}
	l.mu.Lock()
	l.perMin, l.perDay = perMin, perDay
	l.mu.Unlock()
}

// Allow 记一次调用并判断是否放行。
//
// 返回 (ok, retryAfter, reason)：被拒时 retryAfter 是"还要等多久"，
// 界面据此告诉用户"配额用尽，N 秒后可再试"，而不是干巴巴一个 429。
func (l *RateLimiter) Allow(key string) (bool, time.Duration, string) {
	if key == "" {
		key = "anonymous"
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	// 只保留 24 小时内的记录：再早的既不影响分钟窗口也不影响天窗口
	cut := now.Add(-24 * time.Hour)
	kept := l.events[key][:0]
	for _, t := range l.events[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}

	var inMin int
	minCut := now.Add(-time.Minute)
	var oldestInMin time.Time
	for _, t := range kept {
		if t.After(minCut) {
			inMin++
			if oldestInMin.IsZero() || t.Before(oldestInMin) {
				oldestInMin = t
			}
		}
	}
	inDay := len(kept) // 保留集本身就是"最近 24 小时内的调用"

	if inMin >= l.perMin {
		wait := oldestInMin.Add(time.Minute).Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		l.events[key] = kept
		return false, wait, fmt.Sprintf("调用过于频繁：每 %d 分钟最多 %d 次", 1, l.perMin)
	}
	if inDay >= l.perDay {
		// 等最早那次退出 24 小时窗口
		wait := kept[0].Add(24 * time.Hour).Sub(now)
		if wait < time.Second {
			wait = time.Second
		}
		l.events[key] = kept
		return false, wait, fmt.Sprintf("今日额度已用完：每天最多 %d 次", l.perDay)
	}

	l.events[key] = append(kept, now)
	return true, 0, ""
}
