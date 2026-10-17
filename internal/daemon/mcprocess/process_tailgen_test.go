package mcprocess

import (
	"os"
	"testing"
	"time"
)

// appendLine 追加一行到控制台日志（tailLog 读的就是它）。
func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// 本文件守的是 2026-10-12 端到端实测到的一个真事故：
//
//	一个实例上**同时跑着 3 条 tailLog**，于是每一行被广播 3 次 —— 一条 `say`
//	在 563ms 后收到 3 条完全相同的输出（时间戳都一样）。用户看到的是"控制台里
//	同一行出现好几遍"，而且**刷新页面也不消失**（重复源在服务端，不在浏览器）。
//
// 成因：tailLog 原先只靠 "status != running" 判断该不该退出，而 Restart 是
// Stop → Start —— 老 tailer 一旦在它下一次轮询**之前**就看到 status 又被改回
// running（新进程已经起来），它就永远认为自己在岗，继续读同一个日志文件并广播。
// 每重启一次可能多留一条，累积起来就成了 3 条。
//
// Start() 里对 status 的防重入检查拦不住它：那拦的是"两次调用"，而这是两条协程。

// TestSupersededTailerExits 被更新的一轮取代之后，老 tailer 必须退出。
func TestSupersededTailerExits(t *testing.T) {
	inst, logPath := newTailingInstance(t)

	inst.mu.Lock()
	inst.tailGen = 1
	inst.mu.Unlock()

	done := make(chan struct{})
	go func() {
		inst.tailLog(logPath, 1)
		close(done)
	}()
	time.Sleep(2 * tailPollInterval) // 让它进入读循环

	// 模拟新一次 Start：世代推进，老那条从此是"僵尸"
	inst.mu.Lock()
	inst.tailGen = 2
	inst.mu.Unlock()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("被取代的 tailer 没有退出 —— 它会继续读同一个日志文件并广播，" +
			"控制台上每一行都会翻倍（实测能累积到 3 份）")
	}
}

// TestOnlyCurrentTailerGenerationBroadcasts 只有**当前世代**的 tailer 会广播。
//
// 这是重复投递的直接回归用例：留下一条"僵尸"再加一条"现役"，写入一行，
// 订阅者必须**恰好收到一次**。
func TestOnlyCurrentTailerGenerationBroadcasts(t *testing.T) {
	inst, logPath := newTailingInstance(t)

	sub, cancel := inst.Subscribe()
	defer cancel()

	// 「上一轮遗留」的 tailer（世代 1）
	inst.mu.Lock()
	inst.tailGen = 1
	inst.mu.Unlock()
	go inst.tailLog(logPath, 1)
	time.Sleep(2 * tailPollInterval)

	// 新一次 Start：世代 → 2，并起新一轮
	inst.mu.Lock()
	inst.tailGen = 2
	inst.mu.Unlock()
	time.Sleep(2 * tailPollInterval) // 等老那条退出

	go inst.tailLog(logPath, 2)
	time.Sleep(2 * tailPollInterval) // 等新那条就位

	appendLine(t, logPath, "only-once\n")

	got := collect(sub, 1, 3*time.Second)
	if len(got) != 1 {
		t.Fatalf("应收到 1 行，实际 %d 行：%v", len(got), got)
	}

	// 再等两拍，确认**没有第二条** —— 这正是"控制台重复"的形态
	time.Sleep(2 * tailPollInterval)
	if extra := drain(sub); len(extra) != 0 {
		t.Fatalf("同一行被投递了多次（控制台重复的直接原因）：多出 %v", extra)
	}

	// 收尾：让世代 2 的 tailer 也退出，别把协程漏给别的用例
	inst.mu.Lock()
	inst.tailGen = 3
	inst.mu.Unlock()
}
