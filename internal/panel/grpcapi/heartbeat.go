package grpcapi

import (
	"sync"
	"time"
)

// heartbeatTracker 记录节点最近一次心跳，并控制写库频率。
//
// 心跳每 10 秒到达一次，而数据库写入按 interval 节流（默认 30 秒），
// 避免高频写库。
//
// 注意：只有真正写入数据库时才更新时间戳。若每次心跳都更新时间戳，
// 那么「距上次写入的时间」将永远只有「心跳间隔」这么长，
// 节流条件永远不会满足 —— 表现为 last_seen 只在进程启动后更新一次，
// 进而导致节点被误判离线并产生错误告警。
type heartbeatTracker struct {
	mu       sync.Mutex
	lastSave map[string]time.Time
	interval time.Duration
	now      func() time.Time // 可注入，便于测试
}

func newHeartbeatTracker(interval time.Duration) *heartbeatTracker {
	if interval <= 0 {
		interval = heartbeatPersistInterval
	}
	return &heartbeatTracker{
		lastSave: make(map[string]time.Time),
		interval: interval,
		now:      time.Now,
	}
}

// ShouldPersist 判断该节点的心跳是否需要写入数据库。
// 返回 true 时会同时记录本次写入时间。
func (h *heartbeatTracker) ShouldPersist(nodeID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.now()
	last, seen := h.lastSave[nodeID]
	if !seen || now.Sub(last) >= h.interval {
		h.lastSave[nodeID] = now
		return true
	}
	return false
}

// Forget 移除节点记录（节点被删除时调用）。
func (h *heartbeatTracker) Forget(nodeID string) {
	h.mu.Lock()
	delete(h.lastSave, nodeID)
	h.mu.Unlock()
}

// LastSeen 返回最近一次写库时间（用于诊断）。
func (h *heartbeatTracker) LastSeen(nodeID string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.lastSave[nodeID]
	return t, ok
}
