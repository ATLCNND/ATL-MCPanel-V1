package grpcapi

import (
	"reflect"
	"testing"
)

// 增量判据必须被钉死：判错的后果是**静默丢文件**（备份看着成功，恢复后才发现少了）。
func TestChangedSince(t *testing.T) {
	prev := &backupManifest{Files: []manifestFile{
		{Path: "world/region/r.0.0.mca", Size: 100, Mtime: 1000},
		{Path: "server.properties", Size: 50, Mtime: 2000},
		{Path: "world/level.dat", Size: 10, Mtime: 3000},
	}}
	current := []manifestFile{
		{Path: "world/region/r.0.0.mca", Size: 100, Mtime: 1000}, // 未变 → 不进增量
		{Path: "server.properties", Size: 50, Mtime: 9999},       // mtime 变了 → 进
		{Path: "world/region/r.1.1.mca", Size: 200, Mtime: 4000}, // 新增 → 进
		// world/level.dat 不在本轮 → 墓碑
	}
	changed, deleted := changedSince(prev, current)

	var got []string
	for _, f := range changed {
		got = append(got, f.Path)
	}
	want := []string{"server.properties", "world/region/r.1.1.mca"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed = %v，期望 %v", got, want)
	}
	if !reflect.DeepEqual(deleted, []string{"world/level.dat"}) {
		t.Errorf("deleted = %v，期望 [world/level.dat]", deleted)
	}
}

// 没有上一环时必须**全量**（等同基线）—— 否则第一次增量会漏掉所有老文件。
func TestChangedSinceNoPrev(t *testing.T) {
	cur := []manifestFile{{Path: "a", Size: 1, Mtime: 2}, {Path: "b", Size: 3, Mtime: 4}}
	changed, deleted := changedSince(nil, cur)
	if len(changed) != 2 {
		t.Errorf("没有上一环时应全部算新，实际 %d 个", len(changed))
	}
	if len(deleted) != 0 {
		t.Errorf("没有上一环时不应有墓碑，实际 %v", deleted)
	}
}

// size 变了而 mtime 没变，也要算变化（touch -d 回拨时钟、或拷贝保留时间戳的情况）。
func TestChangedSinceSizeOnlyChange(t *testing.T) {
	prev := &backupManifest{Files: []manifestFile{{Path: "x", Size: 1, Mtime: 5}}}
	cur := []manifestFile{{Path: "x", Size: 2, Mtime: 5}}
	changed, _ := changedSince(prev, cur)
	if len(changed) != 1 {
		t.Errorf("size 变化应算变化，实际 %d 个", len(changed))
	}
}

// 同一秒内、尺寸相同的改动必须被识别出来（P3：只比秒会永久漏掉这种改动）。
// 场景：脚本批量改配置、编辑器保存、`sed -i` —— 全都能落在一秒内。
func TestChangedSinceSameSecondDifferentNano(t *testing.T) {
	const sec = 1794200000
	prev := &backupManifest{Files: []manifestFile{
		{Path: "server.properties", Size: 50, Mtime: sec, MtimeNano: sec*1e9 + 100},
	}}
	cur := []manifestFile{
		// 秒相同、尺寸相同，只有纳秒不同 → 必须算变化
		{Path: "server.properties", Size: 50, Mtime: sec, MtimeNano: sec*1e9 + 900},
	}
	changed, deleted := changedSince(prev, cur)
	if len(changed) != 1 {
		t.Fatalf("同秒同尺寸但纳秒不同应算变化，实际 changed=%d", len(changed))
	}
	if len(deleted) != 0 {
		t.Errorf("不应有墓碑，实际 %v", deleted)
	}
}

// 纳秒完全相同 → 不算变化（否则每次增量都会把整个目录重打一遍，增量就没意义了）。
func TestChangedSinceSameNanoUnchanged(t *testing.T) {
	const sec = 1794200000
	const nano = sec*1_000_000_000 + 123456789
	prev := &backupManifest{Files: []manifestFile{
		{Path: "world/level.dat", Size: 10, Mtime: sec, MtimeNano: nano},
	}}
	cur := []manifestFile{{Path: "world/level.dat", Size: 10, Mtime: sec, MtimeNano: nano}}
	changed, _ := changedSince(prev, cur)
	if len(changed) != 0 {
		t.Errorf("完全没变不应进增量，实际 %d 个", len(changed))
	}
}

// 老清单（没有 mtime_nano）必须退回秒级比较：升级的瞬间不能把整条老链判成"全都变了"，
// 否则第一次增量就是一整份完整快照。
func TestChangedSinceLegacyManifestFallsBackToSeconds(t *testing.T) {
	const sec = 1794200000
	// 上一环是老清单：MtimeNano 为 0
	prev := &backupManifest{Files: []manifestFile{
		{Path: "world/level.dat", Size: 10, Mtime: sec},
		{Path: "server.properties", Size: 50, Mtime: sec},
	}}
	cur := []manifestFile{
		// 秒没变（纳秒有值也不该被当成变化）
		{Path: "world/level.dat", Size: 10, Mtime: sec, MtimeNano: sec*1e9 + 500},
		// 秒变了 → 进增量
		{Path: "server.properties", Size: 50, Mtime: sec + 1, MtimeNano: (sec + 1) * 1e9},
	}
	changed, _ := changedSince(prev, cur)
	var got []string
	for _, f := range changed {
		got = append(got, f.Path)
	}
	want := []string{"server.properties"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed = %v，期望 %v（老清单退回秒级比较）", got, want)
	}
}

// sameMtime 的三种组合直接钉一遍，避免以后有人"顺手简化"成只比秒。
func TestSameMtime(t *testing.T) {
	cases := []struct {
		name                               string
		prevSec, prevNano, curSec, curNano int64
		want                               bool
	}{
		{"两边都有纳秒且相同", 10, 10_000_000_005, 10, 10_000_000_005, true},
		{"两边都有纳秒且不同", 10, 10_000_000_001, 10, 10_000_000_002, false},
		{"上一环没有纳秒，秒相同", 10, 0, 10, 10_000_000_001, true},
		{"上一环没有纳秒，秒不同", 10, 0, 11, 11_000_000_000, false},
		{"两边都没有纳秒，秒相同", 10, 0, 10, 0, true},
	}
	for _, c := range cases {
		if got := sameMtime(c.prevSec, c.prevNano, c.curSec, c.curNano); got != c.want {
			t.Errorf("%s: sameMtime = %v，期望 %v", c.name, got, c.want)
		}
	}
}
