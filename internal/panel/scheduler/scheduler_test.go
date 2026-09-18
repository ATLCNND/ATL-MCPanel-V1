package scheduler

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/retention"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open("sqlite3", filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seedInstance(t *testing.T, d *sql.DB, id, status string) {
	t.Helper()
	// nodes.ssh_user / ssh_auth 为 NOT NULL 且无默认值，必须显式提供
	if _, err := d.Exec(`INSERT OR IGNORE INTO nodes (id, name, ip, ssh_user, ssh_auth) VALUES (1, 'node-1', '127.0.0.1', 'root', 'x')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO instances (instance_id, node_id, name, status) VALUES (?, 1, ?, ?)`, id, id, status); err != nil {
		t.Fatal(err)
	}
}

func activeAlerts(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM alerts WHERE active = 1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInstanceCrashRaisesAlert(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "running")

	s := New(Options{
		DB: d,
		InstanceProbe: func(id string) (string, error) {
			return "stopped", nil // 实例实际已停止，但数据库记录为 running
		},
	})
	s.RunOnce(t.Context())

	if got := activeAlerts(t, d); got != 1 {
		t.Fatalf("应产生 1 条告警，实际 %d", got)
	}
	var kind, target string
	_ = d.QueryRow(`SELECT kind, target FROM alerts WHERE active = 1`).Scan(&kind, &target)
	if kind != "instance_crash" || target != "inst1" {
		t.Errorf("告警内容不正确: kind=%s target=%s", kind, target)
	}
}

func TestInstanceCrashAlertDeduped(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "running")

	s := New(Options{DB: d, InstanceProbe: func(string) (string, error) { return "stopped", nil }})
	s.RunOnce(t.Context())
	s.RunOnce(t.Context())
	s.RunOnce(t.Context())

	// 同一问题重复检测不应产生多条告警
	if got := activeAlerts(t, d); got != 1 {
		t.Errorf("告警应去重，实际 %d 条", got)
	}
}

func TestInstanceCrashAlertAutoResolved(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "running")

	stopped := true
	s := New(Options{DB: d, InstanceProbe: func(string) (string, error) {
		if stopped {
			return "stopped", nil
		}
		return "running", nil
	}})

	s.RunOnce(t.Context())
	if activeAlerts(t, d) != 1 {
		t.Fatal("应先产生告警")
	}

	// 实例恢复运行后告警应自动关闭
	stopped = false
	s.RunOnce(t.Context())
	if got := activeAlerts(t, d); got != 0 {
		t.Errorf("实例恢复后告警应关闭，实际仍有 %d 条", got)
	}

	// 历史记录应保留（含 resolved_at）
	// 注意：聚合表达式（MAX）返回的列没有类型信息，需按字符串扫描
	var total int
	var resolved sql.NullString
	if err := d.QueryRow(`SELECT COUNT(*), MAX(resolved_at) FROM alerts`).Scan(&total, &resolved); err != nil {
		t.Fatal(err)
	}
	if total != 1 || !resolved.Valid || resolved.String == "" {
		t.Errorf("应保留 1 条已恢复告警：total=%d resolved=%q", total, resolved.String)
	}
}

func TestNormalStopDoesNotAlert(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "stopped")

	s := New(Options{DB: d, InstanceProbe: func(string) (string, error) { return "stopped", nil }})
	s.RunOnce(t.Context())

	// 数据库与实际情况一致（都是 stopped），不应告警
	if got := activeAlerts(t, d); got != 0 {
		t.Errorf("正常停止不应告警，实际 %d 条", got)
	}
}

func TestNodeOfflineAndDiskAlerts(t *testing.T) {
	d := newTestDB(t)
	if _, err := d.Exec(`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth) VALUES (1, 'node-1', '127.0.0.1', 'root', 'x')`); err != nil {
		t.Fatal(err)
	}

	online := false
	s := New(Options{
		DB: d, DiskWarnMB: 2048,
		NodeProbe: func(int64, string) (bool, int64, error) {
			if online {
				return true, 500, nil // 在线但磁盘仅剩 500MB
			}
			return false, 0, nil
		},
	})

	s.RunOnce(t.Context())
	if got := activeAlerts(t, d); got != 1 {
		t.Fatalf("节点离线应产生 1 条告警，实际 %d", got)
	}

	// 节点恢复在线但磁盘不足
	online = true
	s.RunOnce(t.Context())
	if got := activeAlerts(t, d); got != 1 {
		t.Fatalf("离线告警应关闭并转为磁盘告警，实际 %d 条", got)
	}
	var kind string
	_ = d.QueryRow(`SELECT kind FROM alerts WHERE active = 1`).Scan(&kind)
	if kind != "disk_low" {
		t.Errorf("应产生 disk_low 告警，实际 %s", kind)
	}
}

func TestDueBackupRuns(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "stopped")
	if _, err := d.Exec(`INSERT INTO backup_schedules (instance_id, enabled, interval_hours, keep, include_config)
		VALUES ('inst1', 1, 1, 5, 1)`); err != nil {
		t.Fatal(err)
	}

	called := 0
	s := New(Options{DB: d, Backup: func(id string, includeCfg bool, policy retention.Policy) error {
		called++
		return nil
	}})
	s.RunOnce(t.Context())

	if called != 1 {
		t.Fatalf("应执行 1 次备份，实际 %d", called)
	}

	// 立即再跑不应重复执行（未到间隔）
	s.RunOnce(t.Context())
	if called != 1 {
		t.Errorf("未到间隔不应重复备份，实际执行 %d 次", called)
	}
}

func TestBackupFailureRaisesAlert(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "stopped")
	if _, err := d.Exec(`INSERT INTO backup_schedules (instance_id, enabled, interval_hours, keep, include_config)
		VALUES ('inst1', 1, 1, 5, 1)`); err != nil {
		t.Fatal(err)
	}

	s := New(Options{DB: d, Backup: func(string, bool, retention.Policy) error {
		return errTest
	}})
	s.RunOnce(t.Context())

	var kind string
	if err := d.QueryRow(`SELECT kind FROM alerts WHERE active = 1`).Scan(&kind); err != nil {
		t.Fatal("备份失败应产生告警")
	}
	if kind != "backup_failed" {
		t.Errorf("告警类型应为 backup_failed，实际 %s", kind)
	}
	var lastErr string
	_ = d.QueryRow(`SELECT last_error FROM backup_schedules WHERE instance_id='inst1'`).Scan(&lastErr)
	if lastErr == "" {
		t.Error("应记录失败原因")
	}
}

func TestDisabledScheduleNotRun(t *testing.T) {
	d := newTestDB(t)
	seedInstance(t, d, "inst1", "stopped")
	if _, err := d.Exec(`INSERT INTO backup_schedules (instance_id, enabled, interval_hours)
		VALUES ('inst1', 0, 1)`); err != nil {
		t.Fatal(err)
	}

	called := 0
	s := New(Options{DB: d, Backup: func(string, bool, retention.Policy) error { called++; return nil }})
	s.RunOnce(t.Context())
	if called != 0 {
		t.Error("已禁用的计划不应执行")
	}
}

func TestStartStopLifecycle(t *testing.T) {
	d := newTestDB(t)
	s := New(Options{DB: d, Interval: 10 * time.Millisecond})
	s.Start()
	// 首轮延迟 20 秒，这里只验证 Stop 能正常返回
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Stop 应能及时返回")
	}
}

var errTest = errString("模拟备份失败")

type errString string

func (e errString) Error() string { return string(e) }
