package cron

import (
	"testing"
	"time"
)

// mustParse 解析失败直接失败测试。
func mustParse(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("解析 %q 失败: %v", spec, err)
	}
	return s
}

func TestParseValid(t *testing.T) {
	for _, spec := range []string{
		"* * * * *",
		"0 3 * * *",
		"30 4 1,15 * 5",
		"*/15 * * * *",
		"0 0 1 * 1",
		"0 22 * * 1-5",
		"5 0 * 8 *",
		"0 0 1 1 *",
		"59 23 31 12 7",
	} {
		if _, err := Parse(spec); err != nil {
			t.Errorf("Parse(%q) 失败: %v", spec, err)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for _, spec := range []string{
		"",
		"* * * *",      // 段数不足
		"* * * * * *",  // 段数过多
		"60 * * * *",   // 分钟越界
		"* 24 * * *",   // 小时越界
		"* * 0 * *",    // 日从 1 开始
		"* * 32 * *",   // 日越界
		"* * * 13 *",   // 月越界
		"* * * * 8",    // 周越界
		"*/0 * * * *",  // 步长必须为正
		"abc * * * *",  // 非数字
		"10-5 * * * *", // 区间反了
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) 本应失败但成功了", spec)
		}
	}
}

// TestNextDaily 验证「每天某时刻」的下一次触发。
func TestNextDaily(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	base := time.Date(2025, 3, 10, 1, 0, 0, 0, time.Local)
	next := s.Next(base)
	want := time.Date(2025, 3, 10, 3, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("Next = %v，期望 %v", next, want)
	}

	// 已经过了当天的触发点 → 顺延到次日
	base = time.Date(2025, 3, 10, 5, 30, 0, 0, time.Local)
	next = s.Next(base)
	want = time.Date(2025, 3, 11, 3, 0, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("Next = %v，期望 %v", next, want)
	}
}

// TestNextInterval 验证步长语法。
func TestNextInterval(t *testing.T) {
	s := mustParse(t, "*/15 * * * *")
	base := time.Date(2025, 3, 10, 1, 7, 0, 0, time.Local)
	next := s.Next(base)
	want := time.Date(2025, 3, 10, 1, 15, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Errorf("Next = %v，期望 %v", next, want)
	}
}

// TestPrevMatchesNext 验证 Prev 与 Next 在同一序列上互为反向：
// Prev(Next(t)) 应当回到 t 之后最近的触发点。
func TestPrevMatchesNext(t *testing.T) {
	s := mustParse(t, "30 4 1,15 * 5")
	base := time.Date(2025, 3, 10, 1, 7, 0, 0, time.Local)
	next := s.Next(base)
	if next.IsZero() {
		t.Fatal("Next 返回零值")
	}
	prev := s.Prev(next)
	if !prev.Equal(next) {
		t.Errorf("Prev(Next(t)) = %v，期望 %v", prev, next)
	}
	// next 之后一分钟再取 Prev 仍应回到 next（而不是跳到更早）
	prev2 := s.Prev(next.Add(time.Minute))
	if !prev2.Equal(next) {
		t.Errorf("Prev(next+1m) = %v，期望 %v", prev2, next)
	}
}

// TestPrevFromNow 验证「最近一次应触发时刻」的语义，这是调度器
// 判断任务是否到期的核心依据。
func TestPrevFromNow(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	now := time.Date(2025, 3, 10, 5, 30, 0, 0, time.Local)
	prev := s.Prev(now)
	want := time.Date(2025, 3, 10, 3, 0, 0, 0, time.Local)
	if !prev.Equal(want) {
		t.Errorf("Prev = %v，期望 %v", prev, want)
	}
}

// TestDomDowOrSemantics 验证「日与周都被限定时取或」的标准 cron 语义。
//
// 这条最容易被写反：`0 0 1 * 1` 是"每月 1 号**或**每周一"，
// 若实现成"与"，用户配的每周一任务会变成"只有恰好是 1 号的周一"才跑。
func TestDomDowOrSemantics(t *testing.T) {
	s := mustParse(t, "0 0 1 * 1")

	// 2025-03-01 是周六：命中"1 号"
	if !s.Match(time.Date(2025, 3, 1, 0, 0, 0, 0, time.Local)) {
		t.Error("3 月 1 日（非周一）应命中")
	}
	// 2025-03-03 是周一：命中"周一"
	if !s.Match(time.Date(2025, 3, 3, 0, 0, 0, 0, time.Local)) {
		t.Error("3 月 3 日（周一，非 1 号）应命中")
	}
	// 2025-03-04 是周二且非 1 号：不命中
	if s.Match(time.Date(2025, 3, 4, 0, 0, 0, 0, time.Local)) {
		t.Error("3 月 4 日不应命中")
	}

	// 只限定周时，日字段为 * 应完全忽略
	s2 := mustParse(t, "0 8 * * 1")
	if !s2.Match(time.Date(2025, 3, 3, 8, 0, 0, 0, time.Local)) {
		t.Error("每周一 08:00 应命中")
	}
	if s2.Match(time.Date(2025, 3, 4, 8, 0, 0, 0, time.Local)) {
		t.Error("周二 08:00 不应命中")
	}
}

// TestSundayAlias 验证 7 与 0 都表示周日。
func TestSundayAlias(t *testing.T) {
	a := mustParse(t, "0 12 * * 0")
	b := mustParse(t, "0 12 * * 7")
	sunday := time.Date(2025, 3, 9, 12, 0, 0, 0, time.Local) // 2025-03-09 是周日
	if sunday.Weekday() != time.Sunday {
		t.Fatalf("测试基准日不是周日：%v", sunday.Weekday())
	}
	if !a.Match(sunday) || !b.Match(sunday) {
		t.Error("0 与 7 都应命中周日")
	}
}

// TestNextUnreachable 验证不可能成立的表达式返回零值而不是死循环。
func TestNextUnreachable(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *") // 2 月 30 日不存在
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local)
	if next := s.Next(base); !next.IsZero() {
		t.Errorf("本应无解，却返回 %v", next)
	}
	// 必须在有限时间内返回 —— 若实现里逐分钟硬扫两年就是几百万次循环
	done := make(chan struct{})
	go func() {
		s.Prev(time.Date(2025, 1, 1, 0, 0, 0, 0, time.Local))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Prev 超时，可能存在性能问题")
	}
}

func TestDescribe(t *testing.T) {
	cases := map[string]string{
		"0 3 * * *":    "每天 03:00",
		"30 8 * * 1":   "周一 08:30",
		"*/15 * * * *": "每 15 分钟",
		"* * * * *":    "每分钟",
		"0 8 * * 1-5":  "周一至周五 08:00",
		"0 0 1 * *":    "每月 1 日 00:00",
		"30 2 * * *":   "每天 02:30",
	}
	for spec, want := range cases {
		if got := Describe(spec); got != want {
			t.Errorf("Describe(%q) = %q，期望 %q", spec, got, want)
		}
	}
	// 非法表达式原样返回，不编造描述
	if got := Describe("乱七八糟"); got != "乱七八糟" {
		t.Errorf("非法表达式应原样返回，得到 %q", got)
	}
}
