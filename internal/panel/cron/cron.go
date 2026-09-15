// Package cron 实现面板「定时任务」所需的 5 段 cron 表达式解析。
//
// 为什么自己写而不是引第三方库：需要的功能非常小（5 段、标准语法、
// 求下一次触发时间），而面板对依赖数量敏感 —— 少一个依赖就少一份
// 供应链与许可证审查负担。同时自研便于给出**中文**的自然语言描述，
// 这是 Robfig 之类的通用库不会提供的。
//
// 支持的语法（与 crontab(5) 一致）：
//
//	*        任意值
//	5        具体值
//	1-5      区间
//	*/15     步长
//	1,3,5    枚举
//
// 字段顺序：分 时 日 月 周
//
//	分 0-59 / 时 0-23 / 日 1-31 / 月 1-12 / 周 0-6（0 与 7 都表示周日）
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule 已解析的 cron 表达式。
type Schedule struct {
	Spec string

	minute uint64
	hour   uint64
	day    uint64 // 日（1-31）
	month  uint64 // 月（1-12）
	week   uint64 // 周（0-6，0=周日）

	dayAny bool
	weekAny bool
}

// fieldRange 各字段的取值范围。
type fieldRange struct {
	name  string
	min   int
	max   int
	index int // 在表达式中的位置（用于报错）
}

var fields = []fieldRange{
	{"分钟", 0, 59, 0},
	{"小时", 0, 23, 1},
	{"日", 1, 31, 2},
	{"月", 1, 12, 3},
	{"星期", 0, 7, 4},
}

// Parse 解析 5 段 cron 表达式。
func Parse(spec string) (*Schedule, error) {
	parts := strings.Fields(strings.TrimSpace(spec))
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron 表达式需要 5 段（分 时 日 月 周），当前 %d 段", len(parts))
	}
	s := &Schedule{Spec: spec}
	sets := make([]uint64, 5)
	for i, f := range fields {
		set, err := parseField(parts[i], f)
		if err != nil {
			return nil, err
		}
		sets[i] = set
	}
	s.minute, s.hour, s.day, s.month, s.week = sets[0], sets[1], sets[2], sets[3], sets[4]
	s.dayAny = strings.TrimSpace(parts[2]) == "*"
	s.weekAny = strings.TrimSpace(parts[4]) == "*"

	// 7 表示周日，与 0 等价
	if s.week&(1<<7) != 0 {
		s.week |= 1 << 0
		s.week &^= 1 << 7
	}
	return s, nil
}

// parseField 解析单个字段为位集合。
func parseField(expr string, f fieldRange) (uint64, error) {
	var set uint64
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return 0, fmt.Errorf("%s 字段为空", f.name)
	}
	for _, item := range strings.Split(expr, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		step := 1
		if idx := strings.Index(item, "/"); idx >= 0 {
			stepStr := item[idx+1:]
			item = item[:idx]
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("%s 字段的步长无效：%s", f.name, stepStr)
			}
			step = n
		}

		lo, hi := f.min, f.max
		switch {
		case item == "*":
			// 全范围
		case strings.Contains(item, "-"):
			segs := strings.SplitN(item, "-", 2)
			a, err1 := strconv.Atoi(strings.TrimSpace(segs[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(segs[1]))
			if err1 != nil || err2 != nil {
				return 0, fmt.Errorf("%s 字段的区间无效：%s", f.name, item)
			}
			lo, hi = a, b
		default:
			n, err := strconv.Atoi(item)
			if err != nil {
				return 0, fmt.Errorf("%s 字段含非法值：%s", f.name, item)
			}
			lo, hi = n, n
		}

		if lo < f.min || hi > f.max || lo > hi {
			return 0, fmt.Errorf("%s 字段超出范围（%d-%d）：%s", f.name, f.min, f.max, item)
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << uint(v)
		}
	}
	if set == 0 {
		return 0, fmt.Errorf("%s 字段没有匹配值", f.name)
	}
	return set, nil
}

// Match 判断某个时刻（精确到分钟）是否命中。
func (s *Schedule) Match(t time.Time) bool {
	if s.minute&(1<<uint(t.Minute())) == 0 {
		return false
	}
	if s.hour&(1<<uint(t.Hour())) == 0 {
		return false
	}
	if s.month&(1<<uint(int(t.Month()))) == 0 {
		return false
	}

	domHit := s.day&(1<<uint(t.Day())) != 0
	// Go 的 Weekday：Sunday=0，与 cron 一致
	dowHit := s.week&(1<<uint(int(t.Weekday()))) != 0

	// 标准 cron 语义：日与周都被限定时取「或」，否则取「与」。
	// 容易记反的一点：`0 0 1 * 1` 表示"每月 1 号**或**每周一"，不是两者同时。
	switch {
	case s.dayAny && s.weekAny:
		return true
	case s.dayAny:
		return dowHit
	case s.weekAny:
		return domHit
	default:
		return domHit || dowHit
	}
}

// Next 返回 after 之后（不含 after 所在分钟）的第一个触发时刻。
// 一年内无解时返回零值（例如 2 月 30 日）。
func (s *Schedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(1, 0, 1)
	for t.Before(limit) {
		if s.Match(t) {
			return t
		}
		// 优化：跳过不可能命中的整小时，避免逐分钟空转
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = startOfHour(t).Add(time.Hour)
			continue
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// Prev 返回 before 之前（含 before 所在分钟）的最近一次触发时刻。
// 一年内无解时返回零值。
func (s *Schedule) Prev(before time.Time) time.Time {
	t := before.Truncate(time.Minute)
	limit := t.AddDate(-1, 0, 0)
	for t.After(limit) {
		if s.Match(t) {
			return t
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			// 回退到上一小时的最后一分钟
			t = startOfHour(t).Add(-time.Minute)
			continue
		}
		t = t.Add(-time.Minute)
	}
	return time.Time{}
}

// startOfHour 返回所在小时的整点。
//
// 不用 Time.Truncate：Truncate 以「UTC 纪元以来的绝对时长」取整，
// 在半小时/45 分钟时区（如印度 +05:30、尼泊尔 +05:45）会截到错误的整点。
func startOfHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

// ---- 自然语言描述 ----

var weekNames = []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// Describe 生成中文描述，例如「每天 03:30」「每周一 08:00」「每 15 分钟」。
//
// 只覆盖常见形态；复杂表达式回退为原样展示，避免给出**看似精确实则错误**
// 的说明 —— 那比不解释更糟。
func Describe(spec string) string {
	if _, err := Parse(spec); err != nil {
		return spec
	}
	parts := strings.Fields(spec)
	minute, hour, dom, month, dow := parts[0], parts[1], parts[2], parts[3], parts[4]

	// 每 N 分钟
	if dom == "*" && month == "*" && dow == "*" && hour == "*" {
		if n, ok := stepOf(minute); ok {
			if minute == "*" {
				return "每分钟"
			}
			return fmt.Sprintf("每 %d 分钟", n)
		}
	}

	// 固定时刻
	if n, err := strconv.Atoi(minute); err == nil {
		if h, err := strconv.Atoi(hour); err == nil {
			clock := fmt.Sprintf("%02d:%02d", h, n)
			weekPart := ""
			if dow != "*" {
				weekPart = describeWeek(dow)
			}
			switch {
			case dom == "*" && month == "*" && dow == "*":
				return "每天 " + clock
			case dom == "*" && month == "*" && weekPart != "":
				return weekPart + " " + clock
			case dom == "*" && month == "*":
				return clock
			case month == "*" && dow == "*":
				return fmt.Sprintf("每月 %s 日 %s", dom, clock)
			default:
				return fmt.Sprintf("%s %s %s", month, dom, clock)
			}
		}
		// 每小时的第 n 分钟
		if hour == "*" && dom == "*" && month == "*" && dow == "*" {
			return fmt.Sprintf("每小时第 %d 分钟", n)
		}
	}

	// 每小时的第 n 分钟（带步长）
	if h, ok := stepOf(hour); ok && dom == "*" && month == "*" && dow == "*" {
		if minute == "0" && hour == "*" {
			return "每小时整点"
		}
		return fmt.Sprintf("每 %d 小时的第 %s 分钟", h, minute)
	}

	return spec
}

func describeWeek(dow string) string {
	var names []string
	for _, item := range strings.Split(dow, ",") {
		item = strings.TrimSpace(item)
		if n, err := strconv.Atoi(item); err == nil {
			if n == 7 {
				n = 0
			}
			if n >= 0 && n < len(weekNames) {
				names = append(names, weekNames[n])
			}
		} else if strings.Contains(item, "-") {
			segs := strings.SplitN(item, "-", 2)
			a, err1 := strconv.Atoi(segs[0])
			b, err2 := strconv.Atoi(segs[1])
			if err1 == nil && err2 == nil && a >= 0 && b <= 7 && a <= b {
				names = append(names, weekNames[a%7]+"至"+weekNames[b%7])
			}
		}
	}
	if len(names) == 0 {
		return "每周 " + dow
	}
	return strings.Join(names, "、")
}

func stepOf(expr string) (int, bool) {
	if expr == "*" {
		return 1, true
	}
	if idx := strings.Index(expr, "/"); idx >= 0 && strings.TrimSpace(expr[:idx]) == "*" {
		if n, err := strconv.Atoi(strings.TrimSpace(expr[idx+1:])); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}
