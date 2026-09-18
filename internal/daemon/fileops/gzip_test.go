package fileops

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRawGz 生成"单个文件被 gzip 压过"的 .gz（**不是** tar.gz）。
//
// 这正是 Minecraft 日志轮转的产物形态（2026-09-17-1.log.gz），
// 也是真机上实测解压失败的那个输入：老实现看到 gzip 魔数就按 tar 解，
// 直接报 `archive/tar: invalid tar header`。
func writeRawGz(t *testing.T, p, content string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeTarGz 生成一个真正的 tar.gz（用于对照：它必须仍走归档那条路）。
func writeTarGz(t *testing.T, p string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// 裸 gzip（单文件）必须能解出来，文件名去掉 .gz。
//
// 回归测试：真机上双击 logs/2026-09-17-1.log.gz 之前会失败
// （archive/tar: invalid tar header），因为 gzip 魔数被无条件当成 tar.gz。
func TestExtractRawGzipSingleFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "2026-09-17-1.log.gz")
	writeRawGz(t, src, "第一行日志\n第二行日志\n")

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, "", nil, nil); err != nil {
		t.Fatalf("裸 gzip 应当能解压，实际报错: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "2026-09-17-1.log"))
	if err != nil {
		t.Fatalf("应产出 2026-09-17-1.log: %v", err)
	}
	if string(got) != "第一行日志\n第二行日志\n" {
		t.Errorf("内容应原样还原，实际 %q", string(got))
	}
}

// 同一个扩展名（.gz）下 tar.gz 仍要按归档解 —— 两者只能靠内容区分。
func TestExtractTarGzStillWorks(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "world.tar.gz")
	writeTarGz(t, src, map[string]string{"a.txt": "AAA", "sub/b.txt": "BBB"})

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, "", nil, nil); err != nil {
		t.Fatalf("tar.gz 应正常解压: %v", err)
	}
	for name, want := range map[string]string{"a.txt": "AAA", filepath.Join("sub", "b.txt"): "BBB"} {
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("缺文件 %s: %v", name, err)
		}
		if string(b) != want {
			t.Errorf("%s 内容应为 %q，实际 %q", name, want, string(b))
		}
	}
	// 关键：不能把 tar 归档当成裸 gzip 解成一个大文件
	if _, err := os.Stat(filepath.Join(dst, "world.tar")); err == nil {
		t.Error("tar.gz 被误当成裸 gzip 解出了单个文件")
	}
}

// 不含 .gz 后缀时的兜底：加 .out，绝不能覆盖源文件。
func TestExtractRawGzipNoSuffix(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "weird.dat")
	writeRawGz(t, src, "payload")

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, "", nil, nil); err != nil {
		t.Fatalf("应能解压: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "weird.dat.out")); err != nil {
		t.Errorf("无 .gz 后缀时应产出 <原名>.out: %v", err)
	}
	// 源文件必须原封不动
	b, err := os.ReadFile(src)
	if err != nil || len(b) == 0 || b[0] != 0x1f {
		t.Errorf("源文件不应被覆盖（应仍是 gzip 流），实际 err=%v", err)
	}
}

// 空 tar.gz（gzip 里只有一个空归档）仍然算归档，不该被当成裸 gzip。
func TestExtractEmptyTarGz(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "empty.tar.gz")
	writeTarGz(t, src, map[string]string{})

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), src, dst, "", nil, nil); err != nil {
		t.Fatalf("空 tar.gz 应正常解压（什么都不产出）: %v", err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("空归档不该产出文件，实际 %d 个", len(entries))
	}
}

// 受保护名单对"裸 gzip 单文件"同样生效：否则可以把 frpc.toml.gz 解出来绕过保护。
func TestExtractRawGzipRespectsFilter(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "frpc.toml.gz")
	writeRawGz(t, src, "[common]\ntoken = \"secret\"\n")

	dst := filepath.Join(dir, "out")
	filter := func(name string) bool { return name != "frpc.toml" }
	if err := Extract(context.Background(), src, dst, "", filter, nil); err != nil {
		t.Fatalf("被过滤掉不该报错: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "frpc.toml")); err == nil {
		t.Error("受保护文件名被解出来了 —— filter 对裸 gzip 没生效")
	}
}

// FormatFromName 现在认得 .gz（但解包时会按内容再判一次）。
func TestFormatFromNameGz(t *testing.T) {
	cases := map[string]string{
		"a.zip": "zip", "a.tar.gz": "tar.gz", "a.tgz": "tar.gz", "a.tar": "tar",
		"2026-09-17-1.log.gz": "gz", "a.txt": "",
	}
	for name, want := range cases {
		if got := FormatFromName(name); got != want {
			t.Errorf("FormatFromName(%q) = %q，期望 %q", name, got, want)
		}
	}
	if !strings.Contains(ErrUnsupported.Error(), "gz") {
		t.Error("不支持格式的提示里应包含 gz（现在它是支持的）")
	}
}
