package mcprocess

import (
	"path/filepath"
	"testing"
	"time"
)

// newTestInstance 造一个用 sh 跑指定命令的实例（核心无关，测试不需要真的 java）。
func newTestInstance(t *testing.T, dir, command string) *Instance {
	t.Helper()
	inst := NewInstance(filepath.Base(dir), dir, filepath.Join(dir, "server.jar"), "1G", "1G")
	inst.StartCommand = command
	return inst
}

// waitHook 等 exit 钩子被触发，返回是否等到。
func waitHook(t *testing.T, ch <-chan string, d time.Duration) (string, bool) {
	t.Helper()
	select {
	case id := <-ch:
		return id, true
	case <-time.After(d):
		return "", false
	}
}

// 进程自行退出（含崩溃）时必须触发 exit 钩子 —— 隧道靠它收尾，
// 否则实例崩了 frpc 会一直留着占住公网端口。
func TestExitHookFiresOnNaturalExit(t *testing.T) {
	fired := make(chan string, 4)
	SetExitHook(func(id string) { fired <- id })
	defer SetExitHook(nil)

	dir := t.TempDir()
	inst := newTestInstance(t, dir, "exit 0")
	if err := inst.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	id, ok := waitHook(t, fired, 5*time.Second)
	if !ok {
		t.Fatal("进程已退出，但 exit 钩子没被触发")
	}
	if id != inst.ID {
		t.Errorf("钩子收到的实例 ID 应为 %s，实际 %s", inst.ID, id)
	}
}

// **关键语义**：Stop() 只是"发完停止指令就返回"，此刻进程往往还活着 ——
// 这时绝不能触发 exit 钩子，否则隧道会比实例先消失（这正是本次要修的 bug）。
func TestExitHookNotFiredOnStopRequest(t *testing.T) {
	fired := make(chan string, 4)
	SetExitHook(func(id string) { fired <- id })
	defer SetExitHook(nil)

	dir := t.TempDir()
	// sleep 不读 stdin，因此 stop 指令不会让它退出 —— 正好模拟
	// "服务端没在读控制台"（首次启动下载依赖、JVM 卡住）的情形。
	inst := newTestInstance(t, dir, "sleep 30")
	if err := inst.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	if err := inst.Stop(); err != nil {
		t.Fatalf("发送停止指令失败: %v", err)
	}
	if inst.Status() != "running" {
		t.Fatalf("Stop() 只发指令，状态应仍为 running，实际 %s", inst.Status())
	}

	if id, ok := waitHook(t, fired, 2*time.Second); ok {
		t.Fatalf("进程仍在运行（%s），不该触发 exit 钩子 —— 隧道会比实例先没", id)
	}

	// 收尾时确认钩子本身是好的（不是"永远不触发"）：强杀之后应当触发。
	// 顺带把后台协程走完，免得测试结束时它还在读全局钩子。
	if err := inst.Kill(); err != nil {
		t.Fatalf("强制关闭失败: %v", err)
	}
	if _, ok := waitHook(t, fired, 5*time.Second); !ok {
		t.Fatal("强杀之后应触发 exit 钩子（说明钩子通路本身是好的）")
	}
}

// 强制关闭确实让进程消失，所以要触发（Kill 走的是 cmd.Wait 那条路径）。
func TestExitHookFiresOnKill(t *testing.T) {
	fired := make(chan string, 4)
	SetExitHook(func(id string) { fired <- id })
	defer SetExitHook(nil)

	dir := t.TempDir()
	inst := newTestInstance(t, dir, "sleep 30")
	if err := inst.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	if err := inst.Kill(); err != nil {
		t.Fatalf("强制关闭失败: %v", err)
	}
	if _, ok := waitHook(t, fired, 5*time.Second); !ok {
		t.Fatal("进程被强杀后应触发 exit 钩子")
	}
}

// 状态必须先变 stopped、钩子后到？不 —— 顺序是**钩子先、状态后**：
// 谁看到 stopped 谁就可能立刻重启实例并拉起新 frpc（Restart 靠轮询状态判断），
// 若那时旧进程的收尾才发生，收掉的会是新的 frpc。
func TestExitHookRunsBeforeStatusFlips(t *testing.T) {
	stateWhenFired := make(chan string, 4)
	dir := t.TempDir()
	inst := newTestInstance(t, dir, "exit 0")

	SetExitHook(func(id string) {
		// 钩子里读状态：此刻必须还是 running（说明钩子先于状态翻转执行）
		stateWhenFired <- inst.Status()
	})
	defer SetExitHook(nil)

	if err := inst.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	select {
	case st := <-stateWhenFired:
		if st != "running" {
			t.Fatalf("exit 钩子应在状态翻成 stopped **之前**触发，实际读到 %q", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("钩子没被触发")
	}

	// 等状态归位，避免测试结束时后台协程还在写
	for n := 0; n < 50 && inst.Status() != "stopped"; n++ {
		time.Sleep(50 * time.Millisecond)
	}
	if inst.Status() != "stopped" {
		t.Errorf("钩子之后状态应变为 stopped，实际 %s", inst.Status())
	}
}
