package runas

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := []struct {
		in     string
		want   Mode
		prefix string
		bad    bool
	}{
		{"", ModePerInstance, "atl-i-", false},
		{"per-instance", ModePerInstance, "atl-i-", false},
		{"current", ModeCurrent, "atl-i-", false},
		{"  current  ", ModeCurrent, "atl-i-", false}, // 容忍空白（配置里手写很容易带上）
		{"atl-shared", ModeShared, "atl-i-", false},
		{"rootless", ModeShared, "atl-i-", false},
		{"a b", 0, "", true},     // 含空格：一定是配错了，不能当用户名用
		{"/bin/sh", 0, "", true}, // 含路径分隔符
	}
	for _, c := range cases {
		got, pfx, err := ParseMode(c.in, "")
		if c.bad {
			if err == nil {
				t.Errorf("ParseMode(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMode(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want || pfx != c.prefix {
			t.Errorf("ParseMode(%q) = (%v,%q)，期望 (%v,%q)", c.in, got, pfx, c.want, c.prefix)
		}
	}
}

// 用户名从实例 ID 推导，必须只含合法字符且不超长。
//
// 实例 ID 本身已被注册表校验为安全目录名，这里再收一道：用户名会被
// useradd/userdel 当作参数，虽然我们用的是参数数组而非 shell，但
// "不把未过滤的外部输入递给系统命令"这条规矩不该因为"上游已经过滤过"而放弃。
func TestUsernameSanitize(t *testing.T) {
	m, err := New("per-instance", "atl-i-")
	if err != nil {
		t.Fatal(err)
	}
	got := m.username("Beta-01_x")
	if got != "atl-i-beta-01_x" {
		t.Errorf("大小写与合法字符应保留并转小写，实际 %q", got)
	}
	// 非法字符折成 '-'（实例 ID 理论上有 . 之类时不该直接把用户名搞崩）
	if got := m.username("a.b/c"); got != "atl-i-a-b-c" {
		t.Errorf("非法字符应折成连字符，实际 %q", got)
	}
	// 超长要截断（Linux 用户名上限 32）
	long := strings.Repeat("x", 60)
	if got := m.username(long); len(got) > 32 {
		t.Errorf("用户名应截断到 32 字符以内，实际 %d: %q", len(got), got)
	}
}

// 共用一个已存在的用户时：指向 root 必须被拒绝。
//
// 这条是"实例不允许跑成 root"的第二个入口 —— per-instance 模式不可能给出
// uid 0，但配置里手写 instance_user: root 就会。不做校验的话，
// 一个看起来"只是配置了个共用用户"的改动会把整个漏洞原样带回来。
func TestSharedUserRootRejected(t *testing.T) {
	m, err := New("root", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve("whatever"); err == nil {
		t.Fatal("instance_user 指向 root 时必须报错")
	} else if !strings.Contains(err.Error(), "root") {
		t.Errorf("错误信息里应点明 root，实际: %v", err)
	}
}

// current 模式在 Daemon 是 root 时必须拒绝解析。
//
// 这是整套修复里最要紧的一条断言：以 root 跑实例就是被修掉的那个漏洞，
// 而 current + root = 原样跑成 root。宁可实例起不来。
func TestCurrentModeRejectsRoot(t *testing.T) {
	m, err := New("current", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Resolve("i1")
	if IsRoot() {
		if err == nil {
			t.Fatalf("以 root 运行时 current 模式必须报错，实际拿到身份 %+v", id)
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("错误信息里应点明 root，实际: %v", err)
		}
		return
	}
	// 非 root：解析出当前用户，且 uid 一定不是 0
	if err != nil {
		t.Fatalf("非 root 环境下 current 模式应可用: %v", err)
	}
	if id.UID == 0 {
		t.Error("current 模式解析出的 uid 不应为 0")
	}
}

// CheckTraversable 只看**上级**目录（实例目录自己被 chown 给实例用户，不需要 o+x）。
func TestCheckTraversable(t *testing.T) {
	root := t.TempDir()
	// t.TempDir() 的叶子目录是 0700，正好当作"缺 o+x 的上级"
	instDir := filepath.Join(root, "instances", "i1")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if bad := CheckTraversable(instDir); bad == "" {
		t.Error("上级里有 0700 的目录，应被检出")
	}

	// 注意 Go 的 t.TempDir() 是 /tmp/<测试名>/<序号> 两层：只放开一层不够，
	// 必须把**到 /tmp 为止**的每一级都放开 —— 这恰恰是这个函数要查的东西。
	tmp := os.TempDir()
	for p := root; p != tmp && p != "/" && p != "."; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if bad := CheckTraversable(instDir); bad != "" {
		t.Errorf("全部上级可穿行时不该报错，实际 %s", bad)
	}
	// 实例目录自己 0700 也没关系（属主是实例用户）
	if err := os.Chmod(instDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if bad := CheckTraversable(instDir); bad != "" {
		t.Errorf("实例目录自身 0700 不该被判为故障（属主即实例用户），实际 %s", bad)
	}
}

func TestEnsureDirSetsMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("模式应为 0700，实际 %o", fi.Mode().Perm())
	}
	// 已存在时也要收紧（老装机上可能是更宽松的模式）
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("重复调用应收紧模式，实际 %o", fi.Mode().Perm())
	}
	// 幂等：目录已存在不该报错
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Errorf("重复调用不应报错: %v", err)
	}
}

// ChownTree 对不存在的路径要静默通过（调用点常在删除之后）。
func TestChownTreeMissingPath(t *testing.T) {
	id := &Identity{Username: "nobody-test", UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	if err := ChownTree(filepath.Join(t.TempDir(), "nope"), id); err != nil {
		t.Errorf("路径不存在不应报错: %v", err)
	}
	// nil 身份 = 不降权（非 root 的 current 模式），应直接返回
	if err := ChownTree(t.TempDir(), nil); err != nil {
		t.Errorf("身份为 nil 时应直接返回: %v", err)
	}
}

// 端到端：建用户 → 解析 → 删除。
//
// 只在 root 且系统有 useradd 时跑（单元测试环境通常不是 root，跳过即可）。
// 用独立前缀 atl-t-，并在结束时务必删掉 —— 测试留下的系统用户比测试失败更麻烦。
func TestEnsureAndRemoveRealUser(t *testing.T) {
	if !IsRoot() {
		t.Skip("需要 root 才能创建系统用户")
	}
	if _, err := exec.LookPath("useradd"); err != nil {
		t.Skip("系统没有 useradd")
	}
	m, err := New("per-instance", "atl-t-")
	if err != nil {
		t.Fatal(err)
	}
	const inst = "unittest-1"
	t.Cleanup(func() { m.Remove(inst) }) // 无论成败都要清掉

	dir := filepath.Join(t.TempDir(), "inst")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	id, err := m.Ensure(inst, dir)
	if err != nil {
		t.Fatalf("创建实例用户失败: %v", err)
	}
	if id.UID == 0 {
		t.Fatalf("实例用户的 uid 不能是 0（%+v）", id)
	}
	if id.Username != "atl-t-unittest-1" {
		t.Errorf("用户名应为 atl-t-unittest-1，实际 %s", id.Username)
	}
	// 幂等：再来一次不该失败
	if _, err := m.Ensure(inst, dir); err != nil {
		t.Errorf("重复 Ensure 应幂等: %v", err)
	}
	// 身份真的以该 uid 生效：用 setpriv 起个子进程，让它自报 uid
	if _, err := exec.LookPath("setpriv"); err == nil {
		out, err := exec.Command("setpriv",
			"--reuid", strconv.Itoa(int(id.UID)),
			"--regid", strconv.Itoa(int(id.GID)),
			"--clear-groups", "id", "-u").Output()
		if err != nil {
			t.Fatalf("setpriv 降权执行失败: %v", err)
		}
		if got := strings.TrimSpace(string(out)); got != strconv.Itoa(int(id.UID)) {
			t.Errorf("降权后 uid 应为 %d，实际 %s", id.UID, got)
		}
	}
	// 目录交给它之后，文件属主真的变了
	if err := ChownTree(dir, id); err != nil {
		t.Fatalf("chown 失败: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid != id.UID || st.Gid != id.GID {
			t.Errorf("目录属主应为 %d:%d，实际 %d:%d", id.UID, id.GID, st.Uid, st.Gid)
		}
	}

	m.Remove(inst)
	if _, err := m.resolveUser("atl-t-unittest-1"); err == nil {
		t.Error("Remove 之后用户应已不存在")
	}
}
