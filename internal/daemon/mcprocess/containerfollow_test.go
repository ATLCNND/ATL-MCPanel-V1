package mcprocess

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 造一个"正在跑的跟随进程"（真的子进程，这样才能断言它确实被收掉了）。
func startFakeFollow(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动 sleep（%v），跳过", err)
	}
	return cmd
}

// waitExit 等进程真的退出（**必须 Wait**：只探活会把僵尸进程当成活着的 ——
// 僵尸没被回收，Signal(0) 依然成功）。
func waitExit(t *testing.T, cmd *exec.Cmd, d time.Duration) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// 接管运行中的容器时补的那条跟随进程，必须能在强杀实例时被收掉，
// **且拿到锁的路径不能自死锁**。
//
// 这条测试是补出来的，因为上一版真的写错了：stopFollow 自己去 i.mu.Lock()，
// 而 Stop/Kill 是拿着 i.mu 进来的（Go 的 sync.Mutex 不可重入）——
// 编译能过、其它测试全绿，只有在"停止一个接管来的容器实例"时永久挂住。
//
// 刻意**不用 NewAdopted**：它会起 watchAdopted 协程，那个协程会在进程消失时
// 触发**全局** ExitHook；而 ExitHook 是包级变量、下一个测试又会安装自己的钩子，
// 于是钩子会串到别的测试里去（实测把 exithook_test 打成 FAIL）。
func TestFollowProcessCollectedOnKill(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("t-follow", dir, filepath.Join(dir, "server.jar"), "1G", "1G")

	follow := startFakeFollow(t)
	inst.mu.Lock()
	inst.followCmd = follow
	inst.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = inst.Kill() // 非接管实例且无 cmd → 返回"无进程"，但必须先收掉跟随进程
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Kill() 没有返回：跟随进程存在时发生了死锁")
	}

	if !waitExit(t, follow, 3*time.Second) {
		t.Error("实例被强杀后，跟随容器输出的进程还活着 —— 每接管一次就漏一个进程")
	}
	inst.mu.Lock()
	left := inst.followCmd
	inst.mu.Unlock()
	if left != nil {
		t.Error("跟随进程已收掉，引用应当清空")
	}
}

// 同上，走 Stop() 的"接管 + 容器"那条路径（它同样持有 i.mu）。
func TestFollowProcessCollectedOnStop(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("t-follow-stop", dir, filepath.Join(dir, "server.jar"), "1G", "1G")
	// 手工把它摆成"接管的运行中实例"：不调 NewAdopted，避免 watchAdopted 的全局钩子串场
	inst.mu.Lock()
	inst.status = "running"
	inst.adoptedPID = 999999 // 不存在的 pid：Stop 会在发信号失败后返回，但不该挂住
	inst.mu.Unlock()

	follow := startFakeFollow(t)
	inst.mu.Lock()
	inst.followCmd = follow
	inst.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = inst.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() 没有返回：跟随进程存在时发生了死锁")
	}
	if !waitExit(t, follow, 3*time.Second) {
		t.Error("停止实例时没有收掉跟随进程（会出现「实例已停止、控制台还在冒字」）")
	}
}

// 没有容器运行时（节点没装 docker）时要**明确报错**，
// 让调用方去写"输出无法接续"的兜底说明 —— 而不是静默什么都不做，
// 让用户对着一个再也不更新的控制台猜。
func TestEnsureContainerLogFollowWithoutRuntime(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("t-nofollow", dir, filepath.Join(dir, "server.jar"), "1G", "1G")
	following, err := inst.EnsureContainerLogFollow()
	if err == nil {
		t.Error("没有容器运行时应当返回错误（调用方据此写兜底说明）")
	}
	if following {
		t.Error("没接上时说「有人在采集」会让调用方以为一切正常")
	}
}

// containerRunCLIAlive 的判断：不存在的容器名必须为 false
//（这条防的是"扫 /proc 时匹配得太宽，把整个节点上任何一个 docker 进程都算上"）。
func TestContainerRunCLIAliveNoFalsePositive(t *testing.T) {
	if containerRunCLIAlive("definitely-not-a-real-instance-id") {
		t.Error("不存在的实例不该被判为「采集进程还活着」（否则接管后永远不补跟随）")
	}
}
