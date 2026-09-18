package mcprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTailingInstance 造一个"正在运行"、带空控制台日志文件的实例，用于测 tailLog。
//
// status 直接写成 "running"：isActive() 只看这个字段，测试里不需要真的拉起进程。
func newTailingInstance(t *testing.T) (*Instance, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "logs", "console.log")
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	inst := testInstance("t", dir, "", "1G", "1G")
	inst.status = "running"
	return inst, logPath
}

// collect 从订阅通道里收满 n 行（或超时/通道关闭）。
func collect(sub <-chan string, n int, timeout time.Duration) []string {
	var out []string
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case line, ok := <-sub:
			if !ok {
				return out
			}
			out = append(out, line)
		case <-deadline:
			return out
		}
	}
	return out
}

// attachTail 启动 tail 并**确认它已经在推送**，返回可追加写入的文件句柄。
//
// 不靠 sleep 猜"tail 是否已经追上文件末尾"：tailLog 是在文件末尾 seek 之后才开始
// 读的，若在它 seek 之前就写入数据，那批数据会被永久跳过 —— 靠固定 sleep 的话，
// 机器一忙就会随机失败。这里改用哨兵行探测：反复写入不同的哨兵，
// 直到收到当前这一条为止（收到即证明 tail 已就位）。返回时通道里保证没有残留的哨兵。
func attachTail(t *testing.T, inst *Instance, sub <-chan string, logPath string) *os.File {
	t.Helper()
	go inst.tailLog(logPath)

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		sentinel := fmt.Sprintf("__sentinel_%d__\n", i)
		if _, err := f.WriteString(sentinel); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			select {
			case line := <-sub:
				if line == sentinel {
					return f
				}
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	t.Fatal("tail 始终没有开始推送（哨兵行一条都没收到）")
	return nil
}

// 完整的一行要**立刻**推送（不因为攒行而增加延迟）。
func TestTailLogBroadcastsCompleteLinesPromptly(t *testing.T) {
	inst, logPath := newTailingInstance(t)
	sub, cancel := inst.Subscribe()
	defer cancel()
	f := attachTail(t, inst, sub, logPath)
	defer f.Close()

	if _, err := f.WriteString("line one\nline two\n"); err != nil {
		t.Fatal(err)
	}

	got := collect(sub, 2, 2*time.Second)
	if len(got) != 2 || got[0] != "line one\n" || got[1] != "line two\n" {
		t.Fatalf("应收到两整行，实际 %q", got)
	}
}

// 半行必须**攒起来**，等换行到达再作为**一条**消息推送。
//
// 为什么这条重要：追尾一个正在被写入的文件时，bufio.ReadString 会把
// "还没写完的半行"当作一次成功读取返回。原样广播的话，一行会被拆成两条消息 ——
// 后果不只是看着断成两截：行级高亮靠"行首的 [WARN]/[ERROR]"判断，
// 拆开后前半截有标记、后半截没有，同一行会被涂成两种样子。
func TestTailLogJoinsPartialLines(t *testing.T) {
	inst, logPath := newTailingInstance(t)
	sub, cancel := inst.Subscribe()
	defer cancel()
	f := attachTail(t, inst, sub, logPath)
	defer f.Close()

	// 分两次写入同一行（间隔远小于 idle 兜底时间）
	if _, err := f.WriteString("[12:00:00] [Server thread/WARN]: can't "); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := f.WriteString("keep up\n"); err != nil {
		t.Fatal(err)
	}

	got := collect(sub, 1, tailFlushIdleTicks*tailPollInterval+2*time.Second)
	want := "[12:00:00] [Server thread/WARN]: can't keep up\n"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("半行 + 续行应合并成 1 条消息 %q，实际 %d 条：%q", want, len(got), got)
	}
}

// 长时间补不齐的残行（服务端写进度条不换行）最终也要推出去，
// 否则那段内容在控制台上永远看不见。
func TestTailLogFlushesStalePartialLine(t *testing.T) {
	inst, logPath := newTailingInstance(t)
	sub, cancel := inst.Subscribe()
	defer cancel()
	f := attachTail(t, inst, sub, logPath)
	defer f.Close()

	if _, err := f.WriteString("进度 42%"); err != nil { // 没有换行
		t.Fatal(err)
	}

	got := collect(sub, 1, tailFlushIdleTicks*tailPollInterval+2*time.Second)
	if len(got) != 1 || got[0] != "进度 42%" {
		t.Fatalf("残行应在等待超时后被推出，实际 %q", got)
	}
}

// 订阅者消费不过来时：**不静默丢**，而是补一条"丢了多少行"的提示。
//
// 原来这里是 select/default 直接丢弃，用户只会看到日志莫名缺了一段，
// 从而去查一个并不存在的问题（"服务端是不是没输出"）。
func TestBroadcastReportsDropsInsteadOfSilentLoss(t *testing.T) {
	inst, _ := newTailingInstance(t)
	sub, cancel := inst.Subscribe()
	defer cancel()

	// 灌满缓冲再多发几条：多出来的部分必然被丢弃
	dropped := 5
	for i := 0; i < consoleSubBuf+dropped; i++ {
		inst.broadcast("line\n")
	}

	// 先把缓冲里的行读空（这一步同时证明缓冲内的行一条都没少）
	got := collect(sub, consoleSubBuf, 2*time.Second)
	if len(got) != consoleSubBuf {
		t.Fatalf("缓冲内的行应全部保留，实际 %d", len(got))
	}
	for _, l := range got {
		if strings.Contains(l, "控制台丢帧") {
			t.Fatalf("缓冲未满时不该出现丢帧提示：%q", l)
		}
	}

	// 再发一条：应当先补上丢帧提示，再发这一行
	inst.broadcast("after\n")
	rest := collect(sub, 2, 2*time.Second)
	if len(rest) != 2 {
		t.Fatalf("应收到 [丢帧提示, 新行]，实际 %q", rest)
	}
	if !strings.Contains(rest[0], "控制台丢帧") || !strings.Contains(rest[0], fmt.Sprint(dropped)) {
		t.Errorf("第一条应是丢帧提示并写明丢了几行，实际 %q", rest[0])
	}
	if rest[1] != "after\n" {
		t.Errorf("提示之后应是新行，实际 %q", rest[1])
	}
}

// 取消订阅之后继续广播不能 panic。
//
// 原来 broadcast 是"锁内拷贝订阅者列表、锁外发送"，而 cancel() 会 close(channel)：
// 两者交错就是 send on closed channel —— 用户关掉控制台页签、Daemon 跟着崩掉。
// 单靠这个用例很难每次都踩中那个窗口，配合下面的并发用例 + -race 才有意义。
func TestBroadcastAfterUnsubscribeDoesNotPanic(t *testing.T) {
	inst, _ := newTailingInstance(t)
	_, cancel := inst.Subscribe()
	inst.broadcast("before\n")
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			inst.broadcast("after\n")
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消订阅后广播卡住了")
	}
}

// 并发订阅/取消 + 广播（配合 -race 跑）。
func TestSubscribeUnsubscribeRace(t *testing.T) {
	inst, _ := newTailingInstance(t)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				inst.broadcast("x\n")
			}
		}
	}()
	for i := 0; i < 50; i++ {
		_, cancel := inst.Subscribe()
		cancel()
	}
	close(stop)
}
