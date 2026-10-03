package scheduler

import (
	"testing"
	"time"
)

// 顺序回归：到期停机与磁盘采样必须排在**派发定时任务之前**。
//
// 为什么这条值得锁住：runDueBackups / runDueTasks 是同步执行真实动作的
//（开机要等 JVM 起来，单条任务最长 3 分钟），一串任务就能把一轮 RunOnce
// 拖过好几个 tick。而被拖到后面的恰恰是到期自动停机与磁盘超限停机 ——
// 它们一旦被饿死，"到期"就只是一句提示，节点磁盘也能被写到满。
func TestExpiryAndDiskSweepRunBeforeTasks(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "running")
	if _, err := d.Exec(
		`UPDATE instances SET expires_at = ?, expiry_autostop = 1 WHERE instance_id = 'inst1'`,
		time.Now().Add(-time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	// 一条"每分钟"的任务：last_run 为空且落在补跑窗口内，本轮必然到期
	if _, err := d.Exec(
		`INSERT INTO instance_tasks (instance_id, name, action, cron, enabled)
		 VALUES ('inst1', '每分钟开机', 'start', '* * * * *', 1)`); err != nil {
		t.Fatal(err)
	}

	var order []string
	s := New(Options{
		DB:              d,
		InstanceStopper: func(id string) error { order = append(order, "stop:"+id); return nil },
		TaskRunner:      func(id int64) error { order = append(order, "task"); return nil },
	})
	s.RunOnce(t.Context())

	if len(order) != 2 {
		t.Fatalf("应各执行一次到期停机与任务派发，实际 %v", order)
	}
	if order[0] != "stop:inst1" || order[1] != "task" {
		t.Errorf("到期停机必须排在定时任务之前（否则任务风暴会把它饿死），实际顺序 %v", order)
	}
}

// 非法 cron 表达式的任务要被自动禁用，而不是每轮重试刷日志。
//
// 顺带锁住 runDueTasks 的写法：它必须**先把结果集读进内存并关闭 rows**，
// 再去执行这条 UPDATE —— 边遍历 *sql.Rows 边发新查询是"占着一条连接再要一条"，
// 池一小就互相等死（2026-10-01 面板整体卡死正是这个形状）。
func TestInvalidCronTaskIsDisabled(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "stopped")
	if _, err := d.Exec(
		`INSERT INTO instance_tasks (instance_id, name, action, cron, enabled)
		 VALUES ('inst1', '坏任务', 'start', '不是表达式', 1)`); err != nil {
		t.Fatal(err)
	}

	ran := 0
	s := New(Options{DB: d, TaskRunner: func(int64) error { ran++; return nil }})
	s.RunOnce(t.Context())

	if ran != 0 {
		t.Error("表达式非法的任务不该被执行")
	}
	var enabled int
	var lastErr string
	if err := d.QueryRow(
		`SELECT enabled, last_error FROM instance_tasks WHERE instance_id = 'inst1'`).
		Scan(&enabled, &lastErr); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Error("表达式非法的任务应被自动禁用（否则每轮都会重试并刷日志）")
	}
	if lastErr == "" {
		t.Error("应记录禁用原因")
	}
}
