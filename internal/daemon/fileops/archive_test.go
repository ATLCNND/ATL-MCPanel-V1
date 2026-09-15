package fileops

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkfile 写入一个文件（自动建目录）。
func mkfile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeZip 按给定条目生成一个 zip（名 → 内容）。
func writeZip(t *testing.T, p string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSafeJoinRejectsTraversal 是这组测试里最重要的一条：
// 压缩包是用户可控输入，越界写入等于让租户直接覆盖节点上的任意文件。
func TestSafeJoinRejectsTraversal(t *testing.T) {
	dst := t.TempDir()
	bad := []string{
		"../evil.txt",
		"../../evil.txt",
		"a/../../evil.txt",
		"a/b/../../../../evil",
		"..",
		"",
		"a/./../../evil",
	}
	for _, name := range bad {
		if got, err := safeJoin(dst, name); err == nil {
			t.Errorf("safeJoin(%q) 本应拒绝，却返回 %q", name, got)
		}
	}

	// 绝对路径不拒绝，而是**去掉前导斜杠**后落进目标目录。
	//
	// 这与 GNU tar 的行为一致（tar 解包时会把 "/" 前缀剥掉并告警），
	// 也是刻意的选择：直接拒绝会让一些用 `tar -P` 打出来的合法归档
	// 整个解不开，而剥前缀既能解出内容又不会写到目标目录之外。
	if got, err := safeJoin(dst, "/etc/passwd"); err != nil {
		t.Errorf("绝对路径应被去前缀接收，却报错: %v", err)
	} else if want := filepath.Join(dst, "etc", "passwd"); got != want {
		t.Errorf("safeJoin(/etc/passwd) = %q，期望 %q", got, want)
	}

	good := map[string]string{
		"a.txt":       "a.txt",
		"dir/b.txt":   "dir/b.txt",
		"./c.txt":     "c.txt",
		"a/./d.txt":   "a/d.txt",
		"a..b.txt":    "a..b.txt", // 文件名里含 ".." 但不是路径段，应放行
		"中文 名.txt": "中文 名.txt",
	}
	for name, wantRel := range good {
		got, err := safeJoin(dst, name)
		if err != nil {
			t.Errorf("safeJoin(%q) 意外失败: %v", name, err)
			continue
		}
		want := filepath.Join(dst, filepath.FromSlash(wantRel))
		if got != want {
			t.Errorf("safeJoin(%q) = %q，期望 %q", name, got, want)
		}
	}
}

// TestExtractRejectsZipSlip 端到端验证：含越界条目的压缩包必须失败，
// 且失败时**不能在父目录留下文件**。
func TestExtractRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	writeZip(t, zipPath, map[string]string{
		"ok.txt":       "fine",
		"../escaped.txt": "should not land here",
	})

	dst := filepath.Join(dir, "out")
	err := Extract(context.Background(), zipPath, dst, "zip", nil, nil)
	if err == nil {
		t.Fatal("含越界条目的压缩包本应解包失败")
	}
	if !strings.Contains(err.Error(), "越界") {
		t.Errorf("错误信息应说明是越界路径，实际: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "escaped.txt")); statErr == nil {
		t.Error("越界文件被写到了父目录")
	}
}

// TestExtractSymlinkSkipped 验证符号链接被跳过（防止借链接逃出目标目录）。
func TestExtractSymlinkSkipped(t *testing.T) {
	dir := t.TempDir()
	// 手工构造一个含符号链接的 tar
	tarPath := filepath.Join(dir, "link.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{
		Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "real.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "out")
	if err := Extract(context.Background(), tarPath, dst, "tar", nil, nil); err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "escape")); err == nil {
		t.Error("符号链接应被跳过，却被创建了")
	}
	if b, err := os.ReadFile(filepath.Join(dst, "real.txt")); err != nil || string(b) != "hi" {
		t.Errorf("普通文件应正常解出，得到 %q err=%v", string(b), err)
	}
}

// TestCompressExtractRoundTrip 验证打包 → 解包能还原目录结构与内容。
func TestCompressExtractRoundTrip(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz", "tar"} {
		t.Run(format, func(t *testing.T) {
			base := t.TempDir()
			src := filepath.Join(base, "world")
			mkfile(t, filepath.Join(src, "level.dat"), "LEVEL")
			mkfile(t, filepath.Join(src, "region", "r.0.0.mca"), strings.Repeat("x", 4096))
			mkfile(t, filepath.Join(src, "sub", "deep", "a.txt"), "deep")

			arc := filepath.Join(base, "out"+DefaultExt(format))
			if err := Compress(context.Background(), src, arc, format, nil, nil); err != nil {
				t.Fatalf("压缩失败: %v", err)
			}
			if fi, err := os.Stat(arc); err != nil || fi.Size() == 0 {
				t.Fatalf("压缩产物异常: %v", err)
			}
			// 不应残留 .part 临时文件
			if _, err := os.Stat(arc + ".part"); err == nil {
				t.Error("残留了 .part 临时文件")
			}

			out := filepath.Join(base, "restored")
			if err := Extract(context.Background(), arc, out, "", nil, nil); err != nil {
				t.Fatalf("解包失败: %v", err)
			}
			// 归档内保留顶层目录名，解包后应还原出 world/
			if b, err := os.ReadFile(filepath.Join(out, "world", "level.dat")); err != nil || string(b) != "LEVEL" {
				t.Errorf("level.dat 未还原: %q err=%v", string(b), err)
			}
			if b, err := os.ReadFile(filepath.Join(out, "world", "region", "r.0.0.mca")); err != nil || len(b) != 4096 {
				t.Errorf("region 文件未还原: len=%d err=%v", len(b), err)
			}
			if b, err := os.ReadFile(filepath.Join(out, "world", "sub", "deep", "a.txt")); err != nil || string(b) != "deep" {
				t.Errorf("深层文件未还原: %q err=%v", string(b), err)
			}
		})
	}
}

// TestCompressFilterSkips 验证过滤器生效 —— 面板内部凭据文件必须
// 在打包时被排除，否则用户压缩实例根目录就能把 frps 密钥下载走。
func TestCompressFilterSkips(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "inst")
	mkfile(t, filepath.Join(src, "server.properties"), "motd=hi")
	mkfile(t, filepath.Join(src, "frpc.toml"), "token = SECRET")

	arc := filepath.Join(base, "out.zip")
	filter := func(name string) bool {
		return filepath.Base(name) != "frpc.toml"
	}
	if err := Compress(context.Background(), src, arc, "zip", filter, nil); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.OpenReader(arc)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.Contains(f.Name, "frpc.toml") {
			t.Fatal("受保护文件被打进了压缩包")
		}
	}
}

// TestExtractFormatByMagic 验证按文件内容而非扩展名识别格式。
func TestExtractFormatByMagic(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "data")
	mkfile(t, filepath.Join(src, "a.txt"), "A")

	real := filepath.Join(base, "real.tar.gz")
	if err := Compress(context.Background(), src, real, "tar.gz", nil, nil); err != nil {
		t.Fatal(err)
	}
	// 故意改成一个误导性的扩展名
	misnamed := filepath.Join(base, "looks-like.zip")
	data, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(misnamed, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(base, "out")
	if err := Extract(context.Background(), misnamed, out, "zip", nil, nil); err != nil {
		t.Fatalf("应按魔数识别为 tar.gz，却失败: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "data", "a.txt")); err != nil || string(b) != "A" {
		t.Errorf("内容未还原: %q err=%v", string(b), err)
	}
}

// TestExtractRejectsOverDepth 验证目录层级上限（防御深度嵌套的压缩炸弹）。
func TestExtractRejectsOverDepth(t *testing.T) {
	base := t.TempDir()
	zipPath := filepath.Join(base, "deep.zip")
	deep := strings.Repeat("d/", maxExtractDepth+5) + "f.txt"
	writeZip(t, zipPath, map[string]string{deep: "x"})

	if err := Extract(context.Background(), zipPath, filepath.Join(base, "out"), "zip", nil, nil); err == nil {
		t.Fatal("超深路径本应被拒绝")
	}
}

// TestExtractGzipTarReadable 确认 gzip+tar 组合读写正常（覆盖 compress/gzip 分支）。
func TestExtractGzipTarReadable(t *testing.T) {
	base := t.TempDir()
	p := filepath.Join(base, "x.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte("hello")
	if err := tw.WriteHeader(&tar.Header{Name: "h.txt", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()

	out := filepath.Join(base, "out")
	if err := Extract(context.Background(), p, out, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "h.txt")); err != nil || string(b) != "hello" {
		t.Errorf("内容不对: %q err=%v", string(b), err)
	}
}

// TestCopyAndMove 验证复制/移动（含目录递归与覆盖保护）。
func TestCopyAndMove(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "src")
	mkfile(t, filepath.Join(src, "a.txt"), "A")
	mkfile(t, filepath.Join(src, "d", "b.txt"), "B")

	dst := filepath.Join(base, "copy")
	if err := Copy(context.Background(), src, dst, false, nil); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "d", "b.txt")); err != nil || string(b) != "B" {
		t.Errorf("目录复制不完整: %q err=%v", string(b), err)
	}
	// 已存在且不允许覆盖 → 必须报错，避免静默覆盖用户数据
	if err := Copy(context.Background(), src, dst, false, nil); err == nil {
		t.Error("目标已存在且未允许覆盖时本应报错")
	}
	// 允许覆盖 → 成功
	if err := Copy(context.Background(), src, dst, true, nil); err != nil {
		t.Errorf("允许覆盖时本应成功: %v", err)
	}

	moved := filepath.Join(base, "moved")
	if err := Move(context.Background(), src, moved, false, nil); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Error("移动后源目录应已不存在")
	}
	if b, err := os.ReadFile(filepath.Join(moved, "a.txt")); err != nil || string(b) != "A" {
		t.Errorf("移动结果不完整: %q err=%v", string(b), err)
	}
}

// TestCompressExcludesOwnOutput 回归测试：把压缩包写在源目录里时，
// 输出文件（及其 .part 临时文件）不能被塞进归档。
//
// 这个 bug 是在端到端验证里发现的：对目录 dir 执行「压缩到 dir/x.zip」后，
// 解压出来的内容里多了一个 x.zip.part。原因是打包过程中的 Walk 读到了
// 正在写入的输出文件，把"半个自己"也打进去了 —— 既浪费空间，
// 又让用户在对压缩包做完整性校验时一头雾水。
func TestCompressExcludesOwnOutput(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			base := t.TempDir()
			src := filepath.Join(base, "data")
			mkfile(t, filepath.Join(src, "a.txt"), "A")
			mkfile(t, filepath.Join(src, "b.txt"), "B")

			arc := filepath.Join(src, "self"+DefaultExt(format)) // 压缩包放在源目录里
			if err := Compress(context.Background(), src, arc, format, nil, nil); err != nil {
				t.Fatalf("压缩失败: %v", err)
			}

			out := filepath.Join(base, "out")
			if err := Extract(context.Background(), arc, out, "", nil, nil); err != nil {
				t.Fatalf("解包失败: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(out, "data"))
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			for _, n := range names {
				if strings.Contains(n, "self") {
					t.Errorf("归档内含输出文件自身：%v", names)
				}
			}
			if len(names) != 2 {
				t.Errorf("归档条目数应为 2，实际 %d：%v", len(names), names)
			}
		})
	}
}

// TestFormatFromName 验证扩展名推断。
func TestFormatFromName(t *testing.T) {
	cases := map[string]string{
		"a.zip":    "zip",
		"A.ZIP":    "zip",
		"a.tar.gz": "tar.gz",
		"a.tgz":    "tar.gz",
		"a.tar":    "tar",
		"a.txt":    "",
		"zip":      "",
	}
	for name, want := range cases {
		if got := FormatFromName(name); got != want {
			t.Errorf("FormatFromName(%q) = %q，期望 %q", name, got, want)
		}
	}
}
