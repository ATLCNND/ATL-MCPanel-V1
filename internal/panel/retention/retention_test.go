package retention

import (
	"testing"
	"time"
)

func at(now time.Time, hoursAgo float64) time.Time {
	return now.Add(-time.Duration(hoursAgo * float64(time.Hour)))
}

// TestDefaultPolicyMatchesSpec 验证默认策略与需求一致：
// 「自动 6 小时一次、保存 2 天后消失，手动保留 3 份」。
func TestDefaultPolicyMatchesSpec(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	p := DefaultPolicy()

	// 模拟 5 天内每 6 小时一份自动备份（共 20 份）+ 5 份手动备份
	var entries []Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, Entry{
			ID:        "auto-" + itoa(i),
			CreatedAt: at(now, float64(i)*6),
			Manual:    false,
		})
	}
	for i := 0; i < 5; i++ {
		entries = append(entries, Entry{
			ID:        "manual-" + itoa(i),
			CreatedAt: at(now, float64(i)*12),
			Manual:    true,
		})
	}

	expired := SelectExpired(entries, p, now)
	expiredIDs := map[string]bool{}
	for _, e := range expired {
		expiredIDs[e.ID] = true
	}

	// 手动备份：5 份保留 3 份 → 最旧的 2 份应被删除
	for _, id := range []string{"manual-3", "manual-4"} {
		if !expiredIDs[id] {
			t.Errorf("手动备份 %s 应被淘汰（保留 3 份）", id)
		}
	}
	for _, id := range []string{"manual-0", "manual-1", "manual-2"} {
		if expiredIDs[id] {
			t.Errorf("手动备份 %s 应保留", id)
		}
	}

	// 自动备份：24 小时内（age<=24）应保留 4 份 → auto-0..3
	for i := 0; i <= 3; i++ {
		if expiredIDs["auto-"+itoa(i)] {
			t.Errorf("最近的自动备份 auto-%d 应保留（24 小时内保留 4 份）", i)
		}
	}
	if !expiredIDs["auto-4"] {
		t.Error("auto-4（24 小时）之后超出第一档的应被淘汰")
	}

	// 24~48 小时应保留 2 份 → auto-5, auto-6
	if expiredIDs["auto-5"] || expiredIDs["auto-6"] {
		t.Error("24~48 小时的自动备份应保留 2 份")
	}
	if !expiredIDs["auto-7"] {
		t.Error("auto-7（42 小时）超出第二档应被淘汰")
	}

	// 超过 48 小时的全部删除
	for i := 8; i < 20; i++ {
		if !expiredIDs["auto-"+itoa(i)] {
			t.Errorf("auto-%d 超过 48 小时应被删除", i)
		}
	}

	// 稳态：自动备份保留 6 份
	keptAuto := 0
	for i := 0; i < 20; i++ {
		if !expiredIDs["auto-"+itoa(i)] {
			keptAuto++
		}
	}
	if keptAuto != 6 {
		t.Errorf("自动备份应稳定保留 6 份，实际 %d", keptAuto)
	}
}

func TestNoDeletionWhenWithinLimits(t *testing.T) {
	now := time.Now()
	p := DefaultPolicy()
	entries := []Entry{
		{ID: "a1", CreatedAt: at(now, 1), Manual: false},
		{ID: "a2", CreatedAt: at(now, 7), Manual: false},
		{ID: "m1", CreatedAt: at(now, 2), Manual: true},
	}
	if got := SelectExpired(entries, p, now); len(got) != 0 {
		t.Errorf("未超限时不应删除任何备份，实际删除 %d 份", len(got))
	}
}

func TestManualAndAutoCountedSeparately(t *testing.T) {
	now := time.Now()
	p := Policy{ManualKeep: 3, Tiers: []Tier{{WithinHours: 24, Keep: 4}}}

	// 4 份手动 + 4 份自动，全部在 24 小时内
	var entries []Entry
	for i := 0; i < 4; i++ {
		entries = append(entries, Entry{ID: "m" + itoa(i), CreatedAt: at(now, float64(i)), Manual: true})
		entries = append(entries, Entry{ID: "a" + itoa(i), CreatedAt: at(now, float64(i)+0.5), Manual: false})
	}
	expired := SelectExpired(entries, p, now)

	// 手动 4 份 → 删 1；自动 4 份 → 不删
	if len(expired) != 1 {
		t.Fatalf("应只删除 1 份（最旧的手动备份），实际 %d", len(expired))
	}
	if expired[0].ID != "m3" {
		t.Errorf("应删除最旧的手动备份 m3，实际 %s", expired[0].ID)
	}
}

func TestTiersSortedAndNormalized(t *testing.T) {
	// 乱序与非法档位应被规整
	p := Policy{ManualKeep: -5, Tiers: []Tier{
		{WithinHours: 48, Keep: 2},
		{WithinHours: 0, Keep: 5},   // 非法：区间上界必须 > 0
		{WithinHours: 12, Keep: 0},  // 非法：保留份数必须 > 0
		{WithinHours: 24, Keep: 4},
	}}
	n := p.Normalize()
	if n.ManualKeep != 0 {
		t.Errorf("负数 ManualKeep 应规整为 0，实际 %d", n.ManualKeep)
	}
	if len(n.Tiers) != 2 {
		t.Fatalf("应保留 2 个合法档位，实际 %d", len(n.Tiers))
	}
	if n.Tiers[0].WithinHours != 24 || n.Tiers[1].WithinHours != 48 {
		t.Errorf("档位应按区间升序，实际 %+v", n.Tiers)
	}
}

func TestEmptyTiersKeepsAllAuto(t *testing.T) {
	now := time.Now()
	p := Policy{ManualKeep: 0, Tiers: nil}
	entries := []Entry{
		{ID: "a1", CreatedAt: at(now, 1000), Manual: false},
		{ID: "m1", CreatedAt: at(now, 1), Manual: true},
	}
	expired := SelectExpired(entries, p, now)
	// 未配置梯度 → 自动备份不淘汰；ManualKeep=0 → 手动备份全部过期
	if len(expired) != 1 || expired[0].ID != "m1" {
		t.Errorf("应仅删除手动备份，实际 %+v", expired)
	}
}

func TestBoundaryExact(t *testing.T) {
	now := time.Now()
	p := Policy{ManualKeep: 0, Tiers: []Tier{{WithinHours: 24, Keep: 1}}}

	// 恰好 24 小时：属于 (0,24] 区间 → 保留（Keep=1，它是该区间唯一一份）
	exact := []Entry{{ID: "exact24", CreatedAt: at(now, 24), Manual: false}}
	if got := SelectExpired(exact, p, now); len(got) != 0 {
		t.Error("恰好 24 小时的备份应落在第一档内并被保留")
	}

	// 24 小时零 1 分：超出唯一档位 → 删除
	beyond := []Entry{{ID: "beyond", CreatedAt: at(now, 24.02), Manual: false}}
	if got := SelectExpired(beyond, p, now); len(got) != 1 {
		t.Error("超过 24 小时的备份应被删除")
	}
}

func TestNewlyCreatedBackupFallsInFirstTier(t *testing.T) {
	// 回归测试：年龄恰好为 0 的备份（刚创建就触发清理）必须落在第一档内，
	// 否则它会掉在所有档位之外，永远不会被淘汰。
	now := time.Now()
	p := Policy{ManualKeep: 0, Tiers: []Tier{{WithinHours: 24, Keep: 2}}}

	entries := []Entry{
		{ID: "just-created", CreatedAt: now, Manual: false},
		{ID: "old1", CreatedAt: at(now, 6), Manual: false},
		{ID: "old2", CreatedAt: at(now, 12), Manual: false},
	}
	expired := SelectExpired(entries, p, now)
	// 三份都在第一档（Keep=2）→ 最旧的一份应被淘汰
	if len(expired) != 1 {
		t.Fatalf("应淘汰 1 份，实际 %d 份: %+v", len(expired), expired)
	}
	if expired[0].ID != "old2" {
		t.Errorf("应淘汰最旧的 old2，实际 %s", expired[0].ID)
	}
}

func TestDescribe(t *testing.T) {
	d := DefaultPolicy().Describe()
	if d == "" {
		t.Fatal("描述不应为空")
	}
	// 默认策略应描述出 1 天与 2 天两个档位
	t.Logf("默认策略描述: %s", d)
	for _, want := range []string{"1 天", "2 天", "4 份", "2 份"} {
		if !contains(d, want) {
			t.Errorf("描述应包含 %q，实际: %s", want, d)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
