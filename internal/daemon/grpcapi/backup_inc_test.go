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
		{Path: "world/region/r.0.0.mca", Size: 100, Mtime: 1000},   // 未变 → 不进增量
		{Path: "server.properties", Size: 50, Mtime: 9999},           // mtime 变了 → 进
		{Path: "world/region/r.1.1.mca", Size: 200, Mtime: 4000},     // 新增 → 进
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
