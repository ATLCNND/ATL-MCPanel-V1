package grpcapi

import (
	"os"
	"path/filepath"
	"testing"
)

// writeIcon 在实例目录里放一个给定大小的图标文件。
func writeIcon(t *testing.T, dir, name string, size int) {
	t.Helper()
	b := make([]byte, size)
	if size >= 8 { // 让它看起来像个 PNG（内容不重要 —— 这里只测文件查找）
		copy(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", name, err)
	}
}

// 没有图标是最常见的情况（默认就没有）—— 必须干净地返回"没有"。
func TestFindIconAbsent(t *testing.T) {
	if _, ok := findIcon(t.TempDir()); ok {
		t.Error("空目录不该找到图标")
	}
	if m := instanceIconMtime(t.TempDir()); m != 0 {
		t.Errorf("没有图标时 mtime 应为 0，实际 %d", m)
	}
}

// server-icon.png 是原版约定，优先级要高于 icon.png。
func TestFindIconPrefersServerIcon(t *testing.T) {
	dir := t.TempDir()
	writeIcon(t, dir, "icon.png", 100)
	writeIcon(t, dir, "server-icon.png", 200)

	ic, ok := findIcon(dir)
	if !ok {
		t.Fatal("应该找到图标")
	}
	if ic.Name != "server-icon.png" {
		t.Errorf("应优先 server-icon.png，实际 %s", ic.Name)
	}
	if ic.Size != 200 {
		t.Errorf("大小应为 200，实际 %d", ic.Size)
	}
	if ic.Mtime == 0 {
		t.Error("mtime 不该为 0")
	}
}

// 只放了 icon.png 时也要认（一些整合包/面板的习惯名字）。
func TestFindIconFallsBackToIconPng(t *testing.T) {
	dir := t.TempDir()
	writeIcon(t, dir, "icon.png", 100)

	ic, ok := findIcon(dir)
	if !ok || ic.Name != "icon.png" {
		t.Fatalf("应回退到 icon.png，实际 ok=%v name=%s", ok, ic.Name)
	}
}

// 图标是要发给每个访问者的（列表页每行一个），过大的必须忽略 ——
// 否则误放一个几百 MB 的 PNG 会变成所有人每次刷新都要下载的东西。
func TestFindIconIgnoresOversize(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "server-icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	// 稀疏文件：不必真的写 2MiB+ 数据
	if err := f.Truncate(maxIconBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, ok := findIcon(dir); ok {
		t.Error("超过上限的图标应被忽略")
	}
}

// 超限时若还有个小的备选名字，就用备选 —— 而不是整体放弃。
func TestFindIconSkipsOversizeAndUsesFallback(t *testing.T) {
	dir := t.TempDir()
	big, err := os.Create(filepath.Join(dir, "server-icon.png"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(maxIconBytes + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	writeIcon(t, dir, "icon.png", 100)

	ic, ok := findIcon(dir)
	if !ok || ic.Name != "icon.png" {
		t.Fatalf("过大的 server-icon.png 应被跳过、改用 icon.png，实际 ok=%v name=%s", ok, ic.Name)
	}
}

// 空文件 / 目录同名 都不算图标（目录会让 os.Open 之后才失败，不如这里就挡掉）。
func TestFindIconRejectsEmptyAndDir(t *testing.T) {
	dir := t.TempDir()
	writeIcon(t, dir, "icon.png", 0)
	if _, ok := findIcon(dir); ok {
		t.Error("空文件不该当作图标")
	}

	dir2 := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir2, "server-icon.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := findIcon(dir2); ok {
		t.Error("同名目录不该当作图标")
	}
}
