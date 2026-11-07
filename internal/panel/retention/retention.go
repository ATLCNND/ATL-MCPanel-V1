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
	// ChainID 增量链的标识（= 链上基线的备份 ID）。
	//
	// 为空表示「独立的一份」：手动备份、以及增量功能之前的老备份都是这样。
	// 非空表示它是一条链（base + inc*）的成员，淘汰时必须**整条**一起走
	//（DESIGN-INCREMENTAL-BACKUP.md §4：允许删整链，禁止删 base 留 inc）。
	ChainID string
}

// SelectExpired 返回应当删除的备份（**按份**判定，不看链）。
//
// now 用于计算各备份的"年龄"；调用方应传入同一时刻以保证判定一致。
//
// ⚠️ 有增量链的场景要用 SelectExpiredChains：按份判定会把一条链拆成几半。
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
	sortNewestFirst(manual)
	sortNewestFirst(autos)

	expired := make([]Entry, 0)

	// 1. 手动备份：只保留最新 ManualKeep 份
	if p.ManualKeep >= 0 && len(manual) > p.ManualKeep {
		expired = append(expired, manual[p.ManualKeep:]...)
	}

	// 2/3. 自动备份：按梯度区间逐段保留 + 超出最后一档的全删
	expired = append(expired, selectExpiredByTier(autos, p.Tiers, now)...)

	return expired
}

// SelectExpiredChains 返回应当删除的备份，但**以链为单位**判定。
//
// 为什么必须有这一版：增量备份是一条 base → inc* 的链，单独删掉基线的后果不是
// "少一份备份"，而是剩下的 inc 全部变成**恢复不回去的垃圾**。所以：
//
//   - 自动备份按 ChainID 分组成链，**链的代表 = 链里最新的一份**
//     （它的年龄就是这条链最后一次真正捕获数据的时间点）；
//   - 梯度规则作用在"代表"上 —— 否则一条 6 环的链会把「24 小时留 4 份」的额度
//     全吃掉，用户反而一份新备份都没有；
//   - 代表被选中 → 把这**整条链的所有成员**都返回；
//   - 手动备份与没有链的老备份（ChainID 为空）自成一组，按份处理，行为与以前一致。
func SelectExpiredChains(entries []Entry, p Policy, now time.Time) []Entry {
	p = p.Normalize()

	manual := make([]Entry, 0, len(entries))
	byChain := make(map[string][]Entry, len(entries))
	order := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Manual {
			manual = append(manual, e)
			continue
		}
		k := chainKey(e)
		if _, seen := byChain[k]; !seen {
			order = append(order, k)
		}
		byChain[k] = append(byChain[k], e)
	}

	expired := make([]Entry, 0)

	// 1. 手动备份：按份（与以前一致 —— 手动备份不在链上，各删各的）
	sortNewestFirst(manual)
	if p.ManualKeep >= 0 && len(manual) > p.ManualKeep {
		expired = append(expired, manual[p.ManualKeep:]...)
	}

	if len(p.Tiers) == 0 {
		return expired // 未配置梯度 → 自动备份不参与淘汰
	}

	// 2. 自动备份：每条链只用一个"代表"（最新那份）参选
	reps := make([]Entry, 0, len(order))
	for _, k := range order {
		members := byChain[k]
		sortNewestFirst(members)
		reps = append(reps, members[0])
	}
	// 梯度判定要求入参**已按新到旧排序**（它用 inTier[Keep:] 丢掉的正是最老的那些），
	// 而 reps 的顺序来自 map 遍历 → 这里必须再排一次。
	sortNewestFirst(reps)
	expiredReps := selectExpiredByTier(reps, p.Tiers, now)

	// 3. 代表被选中 → 整条链一起淘汰
	for _, rep := range expiredReps {
		expired = append(expired, byChain[chainKey(rep)]...)
	}
	return expired
}

// chainKey 一条链的分组键：有 ChainID 按链，没有就自成一组。
func chainKey(e Entry) string {
	if e.ChainID != "" {
		return "chain:" + e.ChainID
	}
	return "single:" + e.ID
}

func sortNewestFirst(s []Entry) {
	sort.Slice(s, func(i, j int) bool { return s[i].CreatedAt.After(s[j].CreatedAt) })
}

// selectExpiredByTier 梯度规则本体：入参**必须已按新到旧排序**。
func selectExpiredByTier(autos []Entry, tiers []Tier, now time.Time) []Entry {
	expired := make([]Entry, 0)
	if len(tiers) == 0 {
		return expired
	}
	// 首档下界取 -1 而非 0：区间为 (lo, hi]，若 lo 取 0 则"年龄恰好为 0"的备份
	// （刚创建就触发清理的场景）会掉在所有档位之外，从而永远不会被淘汰。
	lowerBound := -1.0
	for _, tier := range tiers {
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

	// 超出最后一档的历史自动备份全部删除
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
