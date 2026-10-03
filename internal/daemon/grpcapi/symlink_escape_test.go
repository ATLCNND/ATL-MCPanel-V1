package grpcapi

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/safepath"
)

// 软链接逃逸必须被拒绝。
//
// 2026-10-01 安全审查的核心发现：实例目录里的内容**完全由租户控制**
//（文件管理能建文件，start.sh 与插件能建软链接），而原来的 resolvePath 只做
// 词法检查（Clean + 前缀比较）。于是 `ln -s /etc/cron.d/x pwn` 之后，
// 文件管理对 `pwn` 的读写会穿过检查落到实例目录之外 —— 而 Daemon 是 root，
// os.WriteFile / os.ReadFile 又都跟随软链接，结果就是租户可以往节点任意路径
// 写文件（= 拿 root），也可以读走别的租户的凭据或 /etc/shadow。
//
// 这条用例锁住三件事：目录级软链接、文件级软链接、以及"用软链接指向实例内
// 受保护文件"这种绕过保护名单的写法。最后还确认一次正常用法没被误伤 ——
// 安全修复最常见的副作用就是把合法路径一起挡掉。
func TestResolvePath_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	// t.TempDir 在部分平台会经过软链接（如 macOS 的 /var -> /private/var），
	// 先把基准解析成真实路径，免得下面的断言测的是平台差异而不是被测逻辑。
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(root, "instances", "beta01")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{inst, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1) 目录级软链接：实例内的 escape -> 实例外目录
	if err := os.Symlink(outside, filepath.Join(inst, "escape")); err != nil {
		t.Fatal(err)
	}
	if got, err := resolvePath(inst, "escape/secret.txt"); err == nil {
		t.Errorf("目录软链接逃逸未被拒绝：resolvePath = %s（应报错）", got)
	}

	// 2) 文件级软链接：实例内的 pwn -> 实例外文件
	if err := os.Symlink(secret, filepath.Join(inst, "pwn")); err != nil {
		t.Fatal(err)
	}
	if got, err := resolvePath(inst, "pwn"); err == nil {
		t.Errorf("文件软链接逃逸未被拒绝：resolvePath = %s（应报错）", got)
	}

	// 3) 指向实例内**受保护文件**的软链接：没有越界，但必须照样挡住，
	//    否则 `ln -s frpc.toml alias.toml` 就读到了 frps 的 auth token。
	if err := os.WriteFile(filepath.Join(inst, "frpc.toml"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("frpc.toml", filepath.Join(inst, "alias.toml")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePath(inst, "alias.toml"); err == nil {
		t.Error("通过软链接读受保护文件未被拒绝（应按解析后的相对路径再判一次保护名单）")
	}

	// 4) 正常用法不能误伤：实例内指向实例内的软链接、以及普通路径都要放行
	if err := os.MkdirAll(filepath.Join(inst, "plugins"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("plugins", filepath.Join(inst, "pl")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePath(inst, "pl/x.jar"); err != nil {
		t.Errorf("实例内互指的软链接被误伤：%v", err)
	}
	if _, err := resolvePath(inst, "plugins/x.jar"); err != nil {
		t.Errorf("普通路径被误伤：%v", err)
	}
	// 目标还不存在（写新文件）也要放行 —— EvalSymlinks 对不存在的路径会报
	// ENOENT，如果直接用它就必须把"不存在的尾段"接回去，这里锁住那条逻辑。
	if _, err := resolvePath(inst, "plugins/new/deep/file.yml"); err != nil {
		t.Errorf("写入尚不存在的深层文件被误伤：%v", err)
	}
}

// 写操作必须拒绝软链接目标（O_NOFOLLOW），而不是跟着链接写出去。
func TestOpenNoFollow_RefusesSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Daemon 只发布 Linux 版本")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	f, err := safepath.OpenNoFollow(link, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err == nil {
		f.Close()
		t.Fatal("以 O_NOFOLLOW 打开软链接竟然成功了：检查到打开之间的 TOCTOU 窗口仍然敞开")
	}
	if !safepath.IsSymlinkRefusal(err) {
		t.Errorf("期望被识别为「目标不可跟随」，实际 %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "original" {
		t.Errorf("目标文件被改写了：%s", string(b))
	}
}
