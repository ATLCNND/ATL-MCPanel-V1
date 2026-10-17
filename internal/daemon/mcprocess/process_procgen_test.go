package mcprocess

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// 本文件守的是 2026-10-17 在真机上抓到的**状态错乱**（0.9.21 重启一个"接管态"实例之后）：
//
//	服务器其实在正常跑（`Done (40.783s)`、25565 在听、daemon 就是它的父进程），
//	而面板与 daemon 都显示 **stopped**，`state/<id>/daemon.pid` 也不见了。
//
// 根因是两条"等进程退出再收尾"的协程与重启路径抢时序：
//
//	Stop()（接管分支）会起一个协程，每 1 秒看一次被 SIGTERM 的进程死没死；
//	watchAdopted 每 2 秒看一次。进程一死，它们就把 status 改成 stopped 并删掉 pid 记录。
//	而重启路径（Restart = Stop → 立刻 Start）会在进程退出后**马上**拉起新进程、
//	把 status 写回 running、pid 记录写成新进程的 —— 那两个轮询协程完全可能在那之后
//	才轮到检查，于是**把新进程的状态与 pid 记录清掉**。
//
// 修法与 tailGen 同一套：给"当前这一代进程"一个世代号 procGen，收尾协程创建时记下
// 当时的世代，收尾前比一次；不是当前世代就什么都不做。

func TestStaleAdoptWatchersDoNotClobberRestartedProcess(t *testing.T) {
	inst := testInstance("gen01", t.TempDir(), "", "1G", "1G")

	// 造出"新进程已经起来了"的局面：running + pid 记录 + 新世代（7）
	if err := os.WriteFile(inst.pidFile, []byte("424242"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst.mu.Lock()
	inst.status = "running"
	inst.adoptedPID = 0
	inst.procGen = 7
	inst.mu.Unlock()

	dead := deadPID(t)
	stale := uint64(6) // 旧世代：这两个协程守的是上一代进程

	// ① Stop() 那一侧的收尾协程
	done1 := make(chan struct{})
	go func() { inst.watchAdoptedStop(dead, stale); close(done1) }()
	select {
	case <-done1:
	case <-time.After(8 * time.Second):
		t.Fatal("watchAdoptedStop 没有在 8 秒内返回（它本该一轮就发现进程已死）")
	}

	// ② watchAdopted
	done2 := make(chan struct{})
	go func() { inst.watchAdopted(dead, stale); close(done2) }()
	select {
	case <-done2:
	case <-time.After(10 * time.Second):
		t.Fatal("watchAdopted 没有在 10 秒内返回")
	}

	if got := inst.Status(); got != "running" {
		t.Errorf("过期的收尾协程把状态改成了 %q，期望 running —— "+
			"真机现象就是这里：面板显示「已停止」，其实服务器在跑（端口在听、日志在长）", got)
	}
	if _, err := os.Stat(inst.pidFile); err != nil {
		t.Errorf("过期的收尾协程删掉了新进程的 pid 记录：%v\n"+
			"（记录没了的后果：下一次 Daemon 重启时接管不到这个进程，"+
			"面板会认为实例已停止，用户点启动会撞上 session.lock）", err)
	}
}

// TestCurrentAdoptWatchersStillCleanUp 反向确认：**当代**的收尾协程照旧要干活。
//
// 这一条是为了防止"把收尾逻辑直接废掉"式的假修复 —— 只加上世代判断而不做这条断言，
// 一个把两个协程都改成直接 return 的实现也能让上一条用例通过。
func TestCurrentAdoptWatchersStillCleanUp(t *testing.T) {
	inst := testInstance("gen02", t.TempDir(), "", "1G", "1G")
	if err := os.WriteFile(inst.pidFile, []byte("424243"), 0o644); err != nil {
		t.Fatal(err)
	}
	dead := deadPID(t)

	inst.mu.Lock()
	inst.status = "running"
	inst.adoptedPID = dead // 我们守的就是这个已死的进程
	inst.procGen = 7
	inst.mu.Unlock()

	// 世代一致 → 必须收尾
	done := make(chan struct{})
	go func() { inst.watchAdopted(dead, 7); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("watchAdopted 没在 10 秒内返回")
	}

	if got := inst.Status(); got != "stopped" {
		t.Errorf("当代收尾协程应把状态改成 stopped，实际 %q", got)
	}
	if _, err := os.Stat(inst.pidFile); !os.IsNotExist(err) {
		t.Errorf("当代收尾协程应删掉 pid 记录，实际 err=%v", err)
	}
}

// deadPID 返回一个**确定已经退出**的进程号。
//
// 不用"随便挑个大数字"：那依赖 kill(pid,0) 对超出 pid_max 的值返回 ESRCH 的实现细节，
// 而在容器/受限环境里还可能返回 EPERM（会被判成"活着"）。起一个真进程再等它退出最实在。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skipf("起一个短进程失败，跳过：%v", err)
	}
	pid := cmd.Process.Pid
	if IsAlive(pid) {
		t.Skipf("pid %d 仍被判为存活（可能已被系统回收复用），跳过", pid)
	}
	return pid
}
