package mcprocess

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// stubLimiter 记录调用并返回固定错误（ResourceLimiter 注释里说明测试可注入桩）。
type stubLimiter struct {
	assignTreeErr error
	calls         int
}

func (s *stubLimiter) Apply(string, int) error            { return nil }
func (s *stubLimiter) Assign(string, int) error           { return nil }
func (s *stubLimiter) SetMemoryLimit(string, int64) error { return nil }
func (s *stubLimiter) AssignTree(string, int) (int, error) {
	s.calls++
	return 0, s.assignTreeErr
}

// 进程已经退出时，往 cgroup.procs 写会得到 ESRCH（"no such process"）。
// 这**不是**"配额没生效"，只是没东西可收了 —— 秒退的实例很容易命中。
//
// 这条以前看不见（limitWarn 没有出口）；2026-09-15 把它接到界面后，
// 一个已经停掉的临时实例立刻显示出"资源限制未生效"，属于纯噪音。
func TestReconcileLimitIgnoresDeadProcess(t *testing.T) {
	// 造一个"已经退出、但 cmd 还挂着"的窗口（真实启动后 cmd.Wait 协程
	// 会清掉 i.cmd，但收敛协程正好卡在这几毫秒里就会走到这个分支）
	cmd := exec.Command("sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动替身进程失败: %v", err)
	}
	pid := cmd.Process.Pid
	_, _ = cmd.Process.Wait()

	inst := testInstance("t1", t.TempDir(), "", "1G", "1G")
	inst.cmd = cmd // 模拟尚未被清理
	lim := &stubLimiter{assignTreeErr: errors.New("write /sys/fs/cgroup/atlmcpanel/t1/cgroup.procs: no such process")}
	inst.Limiter = lim

	inst.reconcileLimit(pid)

	if lim.calls == 0 {
		t.Fatal("应至少尝试收敛一次（否则这条测试没覆盖到目标分支）")
	}
	if got := inst.LimitWarning(); got != "" {
		t.Errorf("进程已退出时不该留下资源限制失败，实际: %s", got)
	}
}

// 进程还活着、cgroup 却写失败：这才是真的要报的"配额没生效"。
func TestReconcileLimitReportsErrorForLiveProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动替身进程失败: %v", err)
	}
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }()

	inst := testInstance("t2", t.TempDir(), "", "1G", "1G")
	inst.cmd = cmd
	inst.Limiter = &stubLimiter{assignTreeErr: errors.New("permission denied")}

	// 只跑一轮就够：直接把进程留着（别等它退出）
	done := make(chan struct{})
	go func() { inst.reconcileLimit(cmd.Process.Pid); close(done) }()

	if !isWarningSet(t, inst, done, 5*time.Second) {
		t.Error("进程存活时 cgroup 写失败应记进 LimitWarning（界面上要能看到）")
	}
}

// isWarningSet 等到 reconcileLimit 跑完（它内部有 100ms 起的四次等待），再看是否记了警告。
func isWarningSet(t *testing.T, inst *Instance, done <-chan struct{}, d time.Duration) bool {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case <-done:
			return inst.LimitWarning() != ""
		case <-deadline:
			return inst.LimitWarning() != ""
		case <-time.After(50 * time.Millisecond):
			if inst.LimitWarning() != "" {
				return true
			}
		}
	}
}
