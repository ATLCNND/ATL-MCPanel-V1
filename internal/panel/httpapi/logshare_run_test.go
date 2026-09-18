package httpapi

import (
	"sync"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/logshare"
)

// 这一组测试盯的是"分析不绑在浏览器连接上"这个改动。
//
// 背景：最初 AI 分析直接跑在 HTTP 请求里，用户切页 → 上游被取消 → 结论丢失；
// 两个页面同时看同一份日志 → 消耗对方两次 AI。改成后台运行 + 多订阅者后，
// 下面这几条不变量就是正确性的全部依据：
//
//  1. 后加入的订阅者能拿到此前**全部**事件（否则它看到的是残缺结论）；
//  2. 订阅者退出后再 finish，不会重复关同一个 channel（会 panic）；
//  3. 一次运行结束后，重放仍可用，且 channel 已被关闭（handler 才能退出）；
//  4. 结束后的 push 不再产生事件（避免已落库结论被后到的事件污染）。

func contentDelta(s string) logshare.AIEvent {
	return logshare.AIEvent{Event: "status", Data: `{"type":"content","delta":"` + s + `"}`}
}

func TestAIRun_ReplayToLateSubscriber(t *testing.T) {
	run := &aiRun{subs: map[chan logshare.AIEvent]struct{}{}}

	_, ch1, detach1 := run.subscribe()
	defer detach1()

	run.accept(contentDelta("A"))
	run.accept(contentDelta("B"))

	if got := <-ch1; got.Event != "status" {
		t.Fatalf("第一个订阅者没收到事件: %+v", got)
	}
	<-ch1

	// 后加入的页面：backlog 必须是完整的两个事件
	backlog, ch2, detach2 := run.subscribe()
	defer detach2()
	if len(backlog) != 2 {
		t.Fatalf("后加入者应拿到 2 条历史事件，实际 %d 条", len(backlog))
	}

	run.accept(contentDelta("C"))
	if got := <-ch2; got.Data == "" {
		t.Fatal("后加入者应继续收到新事件")
	}

	if answer := run.finish(); answer != "ABC" {
		t.Fatalf("结论文本应为 ABC，实际 %q", answer)
	}
}

func TestAIRun_DetachThenFinishNoDoubleClose(t *testing.T) {
	run := &aiRun{subs: map[chan logshare.AIEvent]struct{}{}}

	_, _, detach := run.subscribe()
	detach()
	detach() // 幂等：handler 的 defer 可能再调一次

	// 订阅者已摘掉，finish 不应重复关闭它（会 panic: close of closed channel）
	run.accept(contentDelta("X"))
	if answer := run.finish(); answer != "X" {
		t.Fatalf("结论文本应为 X，实际 %q", answer)
	}
}

func TestAIRun_SubscriberAfterFinishGetsBacklogAndClosed(t *testing.T) {
	run := &aiRun{subs: map[chan logshare.AIEvent]struct{}{}}
	run.accept(contentDelta("done"))
	run.finish()

	backlog, ch, detach := run.subscribe()
	defer detach()

	if len(backlog) != 1 {
		t.Fatalf("结束后的订阅者也应能重放，实际 %d 条", len(backlog))
	}
	if _, ok := <-ch; ok {
		t.Fatal("结束后的订阅 channel 应已关闭，否则 handler 会永久挂住")
	}

	// 结束后再来事件：既不广播也不攒结论
	run.accept(contentDelta("late"))
	if answer := run.finish(); answer != "done" {
		t.Fatalf("结束后不应再改结论，实际 %q", answer)
	}
}

func TestAIRun_ThinkingNotAccumulated(t *testing.T) {
	run := &aiRun{subs: map[chan logshare.AIEvent]struct{}{}}
	run.accept(logshare.AIEvent{Event: "status", Data: `{"type":"thinking","delta":"想"}`})
	run.accept(contentDelta("答"))
	if answer := run.finish(); answer != "答" {
		t.Fatalf("思考过程不该进结论，实际 %q", answer)
	}
}

func TestAIRun_ConcurrentAcceptAndSubscribe(t *testing.T) {
	// -race 下跑：确认 accept / subscribe / detach 之间没有数据竞争
	run := &aiRun{subs: map[chan logshare.AIEvent]struct{}{}}
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			run.accept(contentDelta("x"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, ch, detach := run.subscribe()
			// 立刻读空，避免缓冲区写满触发"断开慢订阅者"分支
			for len(ch) > 0 {
				<-ch
			}
			detach()
		}
	}()
	wg.Wait()
	run.finish()
}

func TestAIRunHub_ReuseAndRelease(t *testing.T) {
	hub := &aiRunHub{}
	launched := 0
	mk := func() *aiRun { return &aiRun{subs: map[chan logshare.AIEvent]struct{}{}} }
	launch := func(*aiRun) { launched++ }

	a := hub.attach("i/1", mk, launch)
	b := hub.attach("i/1", mk, launch)
	if a != b {
		t.Fatal("同一 key 应复用同一次分析，否则会重复消耗对方的 AI")
	}
	if launched != 1 {
		t.Fatalf("应只启动一次，实际 %d 次", launched)
	}

	// 别人释放：不能把当前这次分析从表里删掉
	hub.release("i/1", &aiRun{})
	if hub.attach("i/1", mk, launch) != a {
		t.Fatal("release 只应清理自己那一次")
	}

	hub.release("i/1", a)
	if hub.attach("i/1", mk, launch) == a {
		t.Fatal("自己 release 后应能重新开始一次分析")
	}
}

func TestAIRunHub_NoStaleAfterInstantFinish(t *testing.T) {
	// 回归：launch 里同步跑完并 release 时，不能把已结束的 run 永久留在表里
	// （否则该日志此后每次打开都只能看到那次失败的结果，再也跑不起来）。
	hub := &aiRunHub{}
	mk := func() *aiRun { return &aiRun{subs: map[chan logshare.AIEvent]struct{}{}} }
	first := hub.attach("i/2", mk, func(r *aiRun) {
		r.finish()
		hub.release("i/2", r)
	})
	second := hub.attach("i/2", mk, func(*aiRun) {})
	if first == second {
		t.Fatal("上一次已结束后，表里不该还留着它")
	}
}
