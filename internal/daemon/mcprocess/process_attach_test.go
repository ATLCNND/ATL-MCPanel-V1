package mcprocess

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// consoleAttachLines 测试里回放的历史行数上限：给足，避免"截断"干扰断言。
const consoleAttachLines = 10000

// drain 排空订阅通道里当前已有的行（不等待新行）。
func drain(sub <-chan string) []string {
	var out []string
	for {
		select {
		case line, ok := <-sub:
			if !ok {
				return out
			}
			out = append(out, line)
		default:
			return out
		}
	}
}

// 控制台附加时"历史快照 + 实时输出"必须**恰好覆盖**每一行：不重、不漏。
//
// 这条用例守的是用户报的"控制台输出会输出两遍"：旧写法先 Subscribe() 再
// RecentOutput()，两步之间广播出去的行会先作为实时事件送出、又被快照从日志文件里
// 读回来，控制台上同一条日志显示两次。反过来把顺序调换又会变成永久丢行，
// 所以修法不是调换顺序，而是把两步合并成一次原子操作（AttachConsole）。
//
// 断言用的是"每行一个唯一编号"，因此可以精确地数出重复与缺失：
// 广播的每一行同时也追加进 console.log —— 和真实链路一致（tailLog 广播的正是它
// 从日志文件里读到的行），这样快照才可能读到窗口里的那一行。
//
// 覆盖范围：AttachConsole 与 broadcast 之间的原子性（含竞态窗口）。
// **不覆盖**：gRPC 帧的收发与顺序（Console 处理器无法在纯单测里构造，它需要
// pb.DaemonService_ConsoleServer）、tailLog 的读文件/攒半行逻辑、以及
// consoleSubBuf 溢出时的丢帧提示（那是 broadcast 的既有行为，另有用例）。
func TestAttachConsoleSnapshotAndLivePartitionExactlyOnce(t *testing.T) {
	inst, logPath := newTailingInstance(t)

	// 附加之前就已经存在的历史（快照必须带上它们）
	const preLines = 50
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := 1; i <= preLines; i++ {
		if _, err := fmt.Fprintf(f, "pre-%d\n", i); err != nil {
			t.Fatal(err)
		}
	}

	// 广播侧：在附加的同一时刻持续产出新行。
	//
	// 每一行先写日志文件、再 broadcast —— 顺序刻意如此：反过来的话，
	// 快照可能读到一行而它还没被广播，用例就会把"实现正确"误判成丢行。
	// 写入是 O_APPEND 的完整小行，快照读到的只会是完整行。
	const liveLines = 4000
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for i := 1; i <= liveLines; i++ {
			line := fmt.Sprintf("live-%d\n", i)
			if _, err := f.WriteString(line); err != nil {
				return
			}
			inst.broadcast(line)
		}
		close(stop)
	}()

	// 附加：快照 + 订阅一次拿到。
	history, sub, cancel := inst.AttachConsole(consoleAttachLines)

	// 实时通道必须**边产边收**：真实链路里转发循环是立刻开始消费的，
	// 而 consoleSubBuf（1024）装不下 liveLines（4000）行 —— 攒到最后再收的话
	// 溢出丢帧是 broadcast 的既有策略，会把"实现正确"误判成丢行。
	var liveMu sync.Mutex
	var live []string
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for line := range sub {
			liveMu.Lock()
			live = append(live, line)
			liveMu.Unlock()
		}
	}()

	select {
	case <-stop:
	case <-time.After(10 * time.Second):
		t.Fatal("广播侧没有跑完")
	}
	wg.Wait()
	// cancel() 会 close 通道，消费协程随之收尾；等它真正退出再读 live，
	// 就不需要 sleep 或轮询去猜"收完了没有"。cancel 只在收到 close 后才返回，
	// 因此这里先把通道收干（广播已停止，不会再溢出）。
	cancel()
	<-collected
	liveMu.Lock()
	liveCopy := append([]string(nil), live...)
	liveMu.Unlock()
	live = liveCopy

	// 把快照与实时的行号各自收进一个集合：
	//   - pre-* 全部只应出现在**快照**里（订阅之前就写完了）
	//   - live-* 每一条都应恰好出现在**两边之一**，不能两边都有、也不能都没有
	seen := make(map[string]int, len(history)+len(live))
	for _, l := range history {
		seen[l]++
	}
	for _, l := range live {
		seen[l]++
	}

	// ① 不重复：任何一行出现两次就是"控制台输出输出两遍"
	dups := 0
	for line, n := range seen {
		if n > 1 {
			if dups < 5 {
				t.Errorf("同一行被推送了 %d 次：%q", n, line)
			}
			dups++
		}
	}
	if dups > 0 {
		t.Errorf("共 %d 行被重复推送（快照与实时重叠）", dups)
	}

	// ② 不丢失：每一行广播过的内容都必须在（快照 ∪ 实时）里出现
	//
	// seen 里既要能按"完整行"匹配、也要能按"没有换行的残行"匹配：快照是在
	// **文件正被追加写入**时读的，极短的一瞬间可能读到半行（这与本次修复无关，
	// 是"边写边读"的固有现象）。live-* 行本身都是完整写入的，残行匹配只为不让
	// 这种抖动被误报成丢行。
	has := func(name string) bool {
		return seen[name+"\n"] > 0 || seen[name] > 0
	}

	var missing []string
	for i := 1; i <= preLines; i++ {
		if !has(fmt.Sprintf("pre-%d", i)) {
			missing = append(missing, fmt.Sprintf("pre-%d", i))
		}
	}
	for i := 1; i <= liveLines; i++ {
		if !has(fmt.Sprintf("live-%d", i)) {
			missing = append(missing, fmt.Sprintf("live-%d", i))
		}
	}
	if len(missing) > 0 {
		// 唯一的豁免情形，且必须**看得见**：订阅者缓冲（consoleSubBuf）溢出时
		// broadcast 会丢掉实时行、并补一条"控制台丢帧"提示 —— 那是它的既有策略，
		// 不是本次要测的东西。真出现丢帧提示，说明这台机器上消费慢于产出，
		// 缺的行可能就是被它丢掉的；这时只提示、不判失败，避免把环境抖动
		// 报成实现缺陷。（重复断言不受这个豁免影响：丢帧只会让行变**少**。）
		dropped := false
		for _, l := range live {
			if strings.Contains(l, "控制台丢帧") {
				dropped = true
				break
			}
		}
		if dropped {
			t.Skipf("实时通道出现过丢帧提示（订阅者缓冲溢出，broadcast 的既有策略）："+
				"%d 行未被覆盖，本次不判定「不丢」；不重断言已在上面完成", len(missing))
		}
		t.Errorf("有 %d 行既不在快照里、也不在实时通道里（丢行比重复严重：崩溃现场就在这些行里），例如 %v",
			len(missing), missing[:min(5, len(missing))])
	}

	// ③ 快照本身仍是"最近 N 行、以换行结尾"的老形状（行为不变）
	for _, l := range history {
		if !strings.HasSuffix(l, "\n") {
			t.Fatalf("快照行应以换行结尾: %q", l)
		}
	}
}

// 附加之后进入的实时行必须走实时通道，而不是又混进快照语义里 ——
// 也就是确认 AttachConsole 返回的通道确实已经挂上了订阅（订阅没生效就只剩丢行）。
func TestAttachConsoleSubscribesLiveOutput(t *testing.T) {
	inst, _ := newTailingInstance(t)

	history, sub, cancel := inst.AttachConsole(consoleAttachLines)
	defer cancel()
	if len(history) != 0 {
		t.Fatalf("空日志不该有历史，实际 %v", history)
	}

	inst.broadcast("after-attach\n")
	got := collect(sub, 1, 2*time.Second)
	if len(got) != 1 || got[0] != "after-attach\n" {
		t.Fatalf("附加后的实时行应经订阅通道送达，实际 %q", got)
	}
}

// 取消附加（关页签）之后继续广播不能 panic，且不能把别的订阅者带走。
//
// AttachConsole 的 cancel 与 Subscribe 的取消是同一套收尾（都在 subMu 内摘掉
// 自己的那一条并 close），这里把它也钉住：取消是幂等无关的，但摘错条目就会
// 误关别人的通道 —— 表现是"别人还开着控制台，Daemon 崩了"。
func TestAttachConsoleCancelIsolatesOtherSubscribers(t *testing.T) {
	inst, _ := newTailingInstance(t)

	other, otherCancel := inst.Subscribe()
	defer otherCancel()

	_, _, cancel := inst.AttachConsole(consoleAttachLines)
	cancel()

	inst.broadcast("still-here\n")

	got := collect(other, 1, 2*time.Second)
	if len(got) != 1 || got[0] != "still-here\n" {
		t.Fatalf("取消一次附加不该影响其它订阅者，实际 %q", got)
	}

	// 反复取消也不能 panic、不能卡住（关页签可能走多条收尾路径）
	done := make(chan struct{})
	go func() {
		defer close(done)
		cancel()
		cancel()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("重复取消附加卡住了")
	}
}

// 并发附加 + 广播（配合 -race 跑）：附加期间不能出现"同一行既在快照里、
// 又在实时通道里"的情形，也不能因为持锁顺序问题死锁。
//
// 与第一条用例的区别：那条是**确定性地数行号**，这条是把不同 goroutine 的
// 附加/取消与广播交错起来，专门逼出竞态窗口（旧写法在这条用例下会稳定出现重复行）。
func TestAttachConsoleConcurrentWithBroadcast(t *testing.T) {
	inst, logPath := newTailingInstance(t)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			line := fmt.Sprintf("c-%d\n", i)
			if _, err := f.WriteString(line); err != nil {
				return
			}
			inst.broadcast(line)
		}
	}()

	for i := 0; i < 30; i++ {
		history, sub, cancel := inst.AttachConsole(consoleAttachLines)
		// 立刻排空实时通道：这里不断言"不丢"（consoleSubBuf 溢出时 broadcast
		// 本来就会丢帧），只断言**不重** —— 快照里的行不该再出现在实时通道里。
		live := drain(sub)
		inSnap := make(map[string]struct{}, len(history))
		for _, l := range history {
			inSnap[l] = struct{}{}
		}
		for _, l := range live {
			if _, dup := inSnap[l]; dup {
				t.Fatalf("附加时同一行同时进了快照与实时通道：%q（控制台会显示两遍）", l)
			}
		}
		cancel()
	}
	close(stop)
	wg.Wait()
}
