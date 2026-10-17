package mcprocess

import (
	"os"
	"strings"
	"testing"
	"time"
)

// 本文件守的是 2026-10-17 在真机上抓到的**自死锁**。
//
// 现象（节点 0.9.20，daemon 升级后）：面板点「重启实例」→ 实例优雅关服后**不再起来**，
// 面板状态永远停在 running，连面板查节点的实时状态都超时。daemon 日志里只有
// 「重启实例」，既没有「重启完成」也没有「重启失败」—— 那条 goroutine 再没返回过。
//
// 协程栈（`systemctl kill -s QUIT` 抓的，行号是实测）：
//
//	mcprocess.(*Instance).resolveJavaBin   process.go:1661  ← 在等一把自己已持有的锁
//	mcprocess.(*Instance).containerJava    process.go:843
//	mcprocess.(*Instance).containerArgv    process.go:788
//	mcprocess.(*Instance).Start            process.go:616   ← 持锁者（Start 全程持 i.mu）
//	mcprocess.(*Instance).Restart          process.go:1297
//	grpcapi.(*Server).RestartInstance.func1 server.go:304
//
// 根因：`Start()` 持有 `i.mu` 时调进 `containerJava()`，而它调的是会**自己加锁**的
// `resolveJavaBin()`。Go 的 sync.Mutex 不可重入 → 永久阻塞；锁被一直攥着，
// 该实例的 `Status()` / 控制台尾随协程 / `Containerized()` 全部跟着卡死，
// 于是外面看到的是"状态卡在 running、实例其实没起来"。
//
// 同一个坑 2026-10-06 修过一次（为 renderCommand 拆出 resolveJavaBinLocked，
// 见该函数注释），但漏了 containerJava 这个调用点 —— 这次把它钉住。

// TestContainerJavaDoesNotDeadlockWhileHoldingLock 复现调用方的持锁姿势。
//
// 不需要 docker、不需要真起进程：老代码会在这里永久卡住，用例超时失败 ——
// 这正是真机上的形态（"再也没返回过"）。
func TestContainerJavaDoesNotDeadlockWhileHoldingLock(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("lock01", dir, "", "1G", "1G")
	// 给一个具体的 Java 版本，让解析真的走到"找 JDK"那条路
	inst.JavaVersion = "21"

	// 复现 Start() 的姿势：持锁调用
	inst.mu.Lock()
	defer inst.mu.Unlock()

	done := make(chan [2]string, 1)
	go func() {
		b, h := inst.containerJava()
		done <- [2]string{b, h}
	}()

	select {
	case r := <-done:
		t.Logf("containerJava 正常返回：javaBin=%q javaHome=%q", r[0], r[1])
	case <-time.After(5 * time.Second):
		t.Fatal("containerJava 在持有 i.mu 的情况下没有返回 —— 它内部又去抢同一把锁（自死锁）。" +
			"Start() 正是这样调用它的；真机后果：实例停了不再起来、状态卡在 running、面板查节点状态超时。")
	}
}

// TestStartContainerBranchGatedByWantContainer 钉住"什么时候才走容器分支"这个判据。
//
// 为什么必须有：`i.Container` 非 nil 只代表**这台节点有 docker**，不代表这个实例开了
// 容器化 —— 接管一个 native 实例时 registry 也会调 bindContainer（registry.go:430）。
// Start() 若只看 `i.Container != nil`，那么"装了 docker + 实例是接管来的"就会让
// 任何实例的启动都走进容器分支，真机上就是这样踩到上面那个自死锁的。
//
// 这条用**源码级**判据而不是起进程：起进程需要真实的降权身份、JDK 与 cgroup，
// 单测里要么起不来、要么依赖宿主环境，而这里要钉住的其实只有"判据写成什么"。
func TestStartContainerBranchGatedByWantContainer(t *testing.T) {
	src, err := os.ReadFile("process.go")
	if err != nil {
		t.Fatalf("读 process.go 失败：%v", err)
	}
	code := string(src)

	if !strings.Contains(code, "if i.wantContainer && i.Container != nil {") {
		t.Error("Start() 里容器分支的判据不再是 `i.wantContainer && i.Container != nil`。" +
			"只判 `i.Container != nil` 会把「节点有 docker」当成「实例要容器化」，" +
			"接管来的 native 实例启动时就会走进容器分支（2026-10-17 真机自死锁的触发条件）。")
	}
	// 反向：containerJava 必须用持锁变体，不能再调回会自己加锁的那个
	if strings.Contains(code, "bin := i.resolveJavaBin()") {
		t.Error("containerJava 又调回了 resolveJavaBin()（它会自己加锁）—— " +
			"它只在 Start() 持锁时被调用，这样写就是自死锁。")
	}
	if !strings.Contains(code, "bin := i.resolveJavaBinLocked()") {
		t.Error("containerJava 里应当明确使用 resolveJavaBinLocked()（表意：调用者已持锁）")
	}
}
