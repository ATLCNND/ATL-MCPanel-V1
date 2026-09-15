// Package retention 实现备份的保留策略（手动定量 + 自动梯度）。
//
// 设计目标：让「自动备份每 6 小时一次、保存 2 天后消失」这类需求表达为
// 一组**梯度（tier）**规则，而不是一个简单的"保留 N 份"。
//
// 梯度模型：按时间从新到旧划分若干区间，每个区间只保留最新的若干份，
// 超出所有区间边界的历史备份全部删除。这样在同样的磁盘占用下，
// 「近期密集、远期稀疏」——近期可回滚到任意 6 小时前，远期仍能按天追溯。
//
// 手动备份与自动备份**分别计数**：手动备份是用户明确的意图（如"开荒前"），
// 不应被自动备份的滚动淘汰挤掉。
package retention

import (
	"sort"
	"time"
)

// Tier 一个保留区间。
type Tier struct {
	// WithinHours 区间上界（距现在的小时数）。区间为 (上一档, 本档]。
	WithinHours int `json:"within_hours"`
	// Keep 该区间内保留的最大份数（保留最新的）。
	Keep int `json:"keep"`
}

// Policy 保留策略。
type Policy struct {
	// ManualKeep 手动备份保留份数（独立于自动备份）。
	ManualKeep int `json:"manual_keep"`
	// Tiers 梯度区间，按 WithinHours 升序；空表示不限制自动备份（仅按 ManualKeep 处理手动）。
	Tiers []Tier `json:"tiers"`
}

// DefaultPolicy 默认策略：自动每 6 小时一次、保存 2 天。
//
//	最近 24 小时 → 保留 4 份（每 6 小时一份）
//	24~48 小时   → 再保留 2 份（每天一份）
//	超过 48 小时 → 全部删除
//
// 与「6 小时一次、两天后消失」完全对应，自动备份稳态约 6 份。
func DefaultPolicy() Policy {
	return Policy{
		ManualKeep: 3,
		Tiers: []Tier{
			{WithinHours: 24, Keep: 4},
			{WithinHours: 48, Keep: 2},
		},
	}
}

// Normalize 规整策略：补默认值、按区间升序排序、丢弃非法档位。
func (p Policy) Normalize() Policy {
	if p.ManualKeep < 0 {
		p.ManualKeep = 0
	}
	tiers := make([]Tier, 0, len(p.Tiers))
	for _, t := range p.Tiers {
		if t.WithinHours <= 0 || t.Keep <= 0 {
			continue
		}
		tiers = append(tiers, t)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i].WithinHours < tiers[j].WithinHours })
	p.Tiers = tiers
	return p
}

// Entry 参与保留判定的一个备份。
type Entry struct {
	ID        string
	CreatedAt time.Time
	Manual    bool
}

// SelectExpired 返回应当删除的备份。
//
// now 用于计算各备份的"年龄"；调用方应传入同一时刻以保证判定一致。
func SelectExpired(entries []Entry, p Policy, now time.Time) []Entry {
	p = p.Normalize()

	// 分成手动 / 自动两组，各自按时间从新到旧
	manual := make([]Entry, 0, len(entries))
	autos := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Manual {
			manual = append(manual, e)
		} else {
			autos = append(autos, e)
		}
	}
	byNewest := func(s []Entry) {
		sort.Slice(s, func(i, j int) bool { return s[i].CreatedAt.After(s[j].CreatedAt) })
	}
	byNewest(manual)
	byNewest(autos)

	expired := make([]Entry, 0)

	// 1. 手动备份：只保留最新 ManualKeep 份
	if p.ManualKeep >= 0 && len(manual) > p.ManualKeep {
		expired = append(expired, manual[p.ManualKeep:]...)
	}

	// 2. 自动备份：按梯度区间逐段保留
	if len(p.Tiers) == 0 {
		return expired // 未配置梯度 → 自动备份不参与淘汰
	}

	// 首档下界取 -1 而非 0：区间为 (lo, hi]，若 lo 取 0 则"年龄恰好为 0"的备份
	// （刚创建就触发清理的场景）会掉在所有档位之外，从而永远不会被淘汰。
	lowerBound := -1.0
	for _, tier := range p.Tiers {
		upper := float64(tier.WithinHours)
		// 收集落在 (lowerBound, upper] 区间的自动备份（已按新到旧排序）
		inTier := make([]Entry, 0)
		for _, e := range autos {
			age := now.Sub(e.CreatedAt).Hours()
			if age > lowerBound && age <= upper {
				inTier = append(inTier, e)
			}
		}
		if len(inTier) > tier.Keep {
			expired = append(expired, inTier[tier.Keep:]...)
		}
		lowerBound = upper
	}

	// 3. 超出最后一档的历史自动备份全部删除
	for _, e := range autos {
		if now.Sub(e.CreatedAt).Hours() > lowerBound {
			expired = append(expired, e)
		}
	}

	return expired
}

// Describe 返回策略的人类可读描述（用于界面展示与审计）。
func (p Policy) Describe() string {
	p = p.Normalize()
	if len(p.Tiers) == 0 {
		return "不自动淘汰"
	}
	s := ""
	for i, t := range p.Tiers {
		lo := 0
		if i > 0 {
			lo = p.Tiers[i-1].WithinHours
		}
		if i > 0 {
			s += "；"
		}
		if lo == 0 {
			s += timeRangeText(0, t.WithinHours) + "保留 " + itoa(t.Keep) + " 份"
		} else {
			s += timeRangeText(lo, t.WithinHours) + "保留 " + itoa(t.Keep) + " 份"
		}
	}
	return s + "；更早的删除"
}

func timeRangeText(lo, hi int) string {
	f := func(h int) string {
		switch {
		case h%24 == 0 && h >= 24:
			return itoa(h/24) + " 天"
		case h >= 24:
			return itoa(h/24) + " 天 " + itoa(h%24) + " 小时"
		default:
			return itoa(h) + " 小时"
		}
	}
	if lo == 0 {
		return "最近 " + f(hi) + " 内 "
	}
	return f(lo) + "~" + f(hi) + " "
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
