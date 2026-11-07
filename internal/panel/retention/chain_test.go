package retention

import (
	"sort"
	"testing"
	"time"
)

// 整链淘汰（DESIGN-INCREMENTAL-BACKUP.md §4）。
//
// 要钉住的就两条：
//  1. **允许删整条链**（base + 它的所有 inc），
//  2. **禁止**删 base 留 inc、或删中间一环 —— 那等于留下一堆恢复不回去的垃圾。
//
// 另外还要防一个反直觉的坑：一条 6 环的链如果按"份"参与梯度判定，会把
// 「24 小时留 4 份」的额度全吃掉，用户反而一份新备份都没有。所以链要以
// **代表（链里最新那份）**的身份参选。

func chainEntries(base string, ids ...string) []Entry {
	now := time.Now()
	out := make([]Entry, 0, len(ids))
	for i, id := range ids {
		// 链内按分钟递进，最后一份最新
		out = append(out, Entry{
			ID:        id,
			CreatedAt: now.Add(-time.Duration(len(ids)-i) * time.Minute),
			ChainID:   base,
		})
	}
	return out
}

func idsOf(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	sort.Strings(out)
	return out
}

func sameIDs(t *testing.T, got []Entry, want ...string) {
	t.Helper()
	g := idsOf(got)
	sort.Strings(want)
	if len(g) != len(want) {
		t.Fatalf("淘汰集 = %v，期望 %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("淘汰集 = %v，期望 %v", g, want)
		}
	}
}

// 一条 6 环的链在「24 小时留 4 份」下不能把自己拆掉：链只算一个名额，一份都不删。
func TestChainCountsAsOneUnit(t *testing.T) {
	p := Policy{ManualKeep: 3, Tiers: []Tier{{WithinHours: 24, Keep: 4}}}
	entries := chainEntries("base1", "base1", "inc1", "inc2", "inc3", "inc4", "inc5")
	got := SelectExpiredChains(entries, p, time.Now())
	if len(got) != 0 {
		t.Fatalf("单条链不该被淘汰任何成员，实际 %v", idsOf(got))
	}
}

// 两条链竞争一个名额 → 老链**整条**消失（含 base），新链完整保留。
func TestOldChainRemovedWholly(t *testing.T) {
	p := Policy{ManualKeep: 3, Tiers: []Tier{{WithinHours: 24, Keep: 1}}}
	now := time.Now()
	entries := []Entry{
		// 老链：最后一份数据是 5 小时前
		{ID: "old-base", CreatedAt: now.Add(-6 * time.Hour), ChainID: "old-base"},
		{ID: "old-inc1", CreatedAt: now.Add(-5 * time.Hour), ChainID: "old-base"},
		{ID: "old-inc2", CreatedAt: now.Add(-4 * time.Hour), ChainID: "old-base"},
		// 新链：最后一份数据是 1 小时前
		{ID: "new-base", CreatedAt: now.Add(-3 * time.Hour), ChainID: "new-base"},
		{ID: "new-inc1", CreatedAt: now.Add(-1 * time.Hour), ChainID: "new-base"},
	}
	got := SelectExpiredChains(entries, p, now)
	sameIDs(t, got, "old-base", "old-inc1", "old-inc2")
}

// 没有链的老备份（增量功能之前）与手动备份都按份处理，行为不变。
func TestStandaloneAndManualUnchanged(t *testing.T) {
	p := Policy{ManualKeep: 2, Tiers: []Tier{{WithinHours: 24, Keep: 2}}}
	now := time.Now()
	entries := []Entry{
		// 三份独立的老自动备份，只留最新 2 份
		{ID: "legacy1", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "legacy2", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "legacy3", CreatedAt: now.Add(-1 * time.Hour)},
		// 三份手动备份，只留最新 2 份
		{ID: "m1", CreatedAt: now.Add(-30 * time.Hour), Manual: true},
		{ID: "m2", CreatedAt: now.Add(-20 * time.Hour), Manual: true},
		{ID: "m3", CreatedAt: now.Add(-10 * time.Hour), Manual: true},
	}
	got := SelectExpiredChains(entries, p, now)
	sameIDs(t, got, "legacy1", "m1")
}

// 链被选中时，**不在**梯度里被单独判定的成员（比如 base 很老、inc 很新）也要一起走。
func TestChainMembersNotSplitByAge(t *testing.T) {
	p := Policy{ManualKeep: 0, Tiers: []Tier{{WithinHours: 2, Keep: 5}}}
	now := time.Now()
	entries := []Entry{
		// base 是 10 小时前（超出所有档位 → 本来会被单删），inc 是 1 小时前
		{ID: "b", CreatedAt: now.Add(-10 * time.Hour), ChainID: "b"},
		{ID: "i1", CreatedAt: now.Add(-1 * time.Hour), ChainID: "b"},
	}
	got := SelectExpiredChains(entries, p, now)
	// 链的代表 = i1（10 小时前的 base 不算数）；1 小时内、档位内 keep 5 → 不淘汰
	if len(got) != 0 {
		t.Fatalf("代表在档位内，整条链都该留，实际淘汰 %v", idsOf(got))
	}

	// 把代表也推老 → 整条链一起淘汰（包括看起来还很新的 i1）
	entries[1].CreatedAt = now.Add(-30 * time.Hour)
	got = SelectExpiredChains(entries, p, now)
	sameIDs(t, got, "b", "i1")
}

// 旧函数保持原语义（按份），避免外部/测试引用被悄悄改变行为。
func TestSelectExpiredStillPerEntry(t *testing.T) {
	p := Policy{ManualKeep: 3, Tiers: []Tier{{WithinHours: 24, Keep: 2}}}
	now := time.Now()
	entries := []Entry{
		{ID: "base1", CreatedAt: now.Add(-5 * time.Hour), ChainID: "base1"},
		{ID: "inc1", CreatedAt: now.Add(-4 * time.Hour), ChainID: "base1"},
		{ID: "inc2", CreatedAt: now.Add(-3 * time.Hour), ChainID: "base1"},
	}
	got := SelectExpired(entries, p, now)
	if len(got) != 1 || got[0].ID != "base1" {
		t.Fatalf("按份判定时最老的一份应被选中，实际 %v", idsOf(got))
	}
}
