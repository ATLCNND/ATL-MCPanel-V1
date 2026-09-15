package grpcapi

import (
	"testing"
	"time"
)

func TestHeartbeatPersistsFirstTime(t *testing.T) {
	h := newHeartbeatTracker(30 * time.Second)
	if !h.ShouldPersist("node-1") {
		t.Error("首次心跳应写库（否则 last_seen 永远为空）")
	}
}

// 关键回归测试：心跳节流必须周期性放行。
//
// 曾经的实现每次心跳都刷新时间戳，导致「距上次写入」永远等于心跳间隔，
// 节流条件永不满足 —— 表现为 last_seen 只在进程启动后更新一次，
// 节点随后被误判离线。
func TestHeartbeatThrottleAllowsPeriodicWrites(t *testing.T) {
	const heartbeatInterval = 10 * time.Second
	const persistInterval = 30 * time.Second

	h := newHeartbeatTracker(persistInterval)

	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }

	persists := 0
	// 模拟 5 分钟心跳（每 10 秒一次）
	for i := 0; i < 30; i++ {
		if h.ShouldPersist("node-1") {
			persists++
		}
		now = now.Add(heartbeatInterval)
	}

	// 5 分钟内按 30 秒节流，应写入约 10 次（允许 ±1 的边界差异）
	if persists < 9 || persists > 11 {
		t.Errorf("5 分钟内应写库约 10 次，实际 %d 次（节流可能失效）", persists)
	}
}

func TestHeartbeatThrottleSkipsWithinInterval(t *testing.T) {
	h := newHeartbeatTracker(30 * time.Second)
	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }

	if !h.ShouldPersist("n") {
		t.Fatal("首次应写库")
	}
	// 间隔内多次心跳都不应写库
	for i := 0; i < 2; i++ {
		now = now.Add(10 * time.Second)
		if h.ShouldPersist("n") {
			t.Errorf("距上次写入仅 %d 秒，不应写库", 10*(i+1))
		}
	}
	// 达到间隔后应写库
	now = now.Add(10 * time.Second)
	if !h.ShouldPersist("n") {
		t.Error("距上次写入已达 30 秒，应写库")
	}
}

func TestHeartbeatTrackerIndependentNodes(t *testing.T) {
	h := newHeartbeatTracker(30 * time.Second)
	now := time.Unix(1_700_000_000, 0)
	h.now = func() time.Time { return now }

	if !h.ShouldPersist("a") || !h.ShouldPersist("b") {
		t.Fatal("两个节点的首次心跳都应写库")
	}
	now = now.Add(10 * time.Second)
	// 两个节点都在节流窗口内
	if h.ShouldPersist("a") || h.ShouldPersist("b") {
		t.Error("两个节点都应处于节流窗口内")
	}
}

func TestHeartbeatForget(t *testing.T) {
	h := newHeartbeatTracker(30 * time.Second)
	h.ShouldPersist("n")
	if _, ok := h.LastSeen("n"); !ok {
		t.Fatal("应记录写库时间")
	}
	h.Forget("n")
	if _, ok := h.LastSeen("n"); ok {
		t.Error("Forget 后不应再记录")
	}
	// Forget 后再次心跳应重新写库
	if !h.ShouldPersist("n") {
		t.Error("Forget 后首次心跳应写库")
	}
}

func TestHeartbeatDefaultInterval(t *testing.T) {
	h := newHeartbeatTracker(0)
	if h.interval != heartbeatPersistInterval {
		t.Errorf("非正间隔应回退为默认值 %v，实际 %v", heartbeatPersistInterval, h.interval)
	}
}
