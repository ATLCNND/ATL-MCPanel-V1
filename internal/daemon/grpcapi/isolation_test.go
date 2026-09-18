package grpcapi

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
)

// 这是一条**真机验收**测试：它真的建系统用户、真的以那个身份跑进程、
// 真的去碰 /etc 与别的实例目录。跳过条件：非 root、或系统没有 useradd。
//
// 之所以非写不可：本次修的 T0（实例即 root）是"配置对不对"看不出来的 ——
// 代码里少写一行 Credential，编译、启动、跑起来全都正常，
// 只有 `id -u` 的输出才能证明修复成立。所以这条测试断言的就是那个输出。
//
// 三条不变量（对应内测报告的 POC）：
//  1. 实例进程的 uid **不是 0**；
//  2. 它**写不了 /etc**（报告里 tampered 的正是这一步）；
//  3. 它**读不到另一个实例的目录**（多租户隔离）。
func TestInstanceIsolationOnRealMachine(t *testing.T) {
	if !runas.IsRoot() {
		t.Skip("需要 root 才能创建系统用户并降权")
	}
	if _, err := os.Stat("/usr/sbin/useradd"); err != nil {
		if _, err2 := os.Stat("/sbin/useradd"); err2 != nil {
			t.Skip("系统没有 useradd")
		}
	}

	// 实例根目录要能让实例用户穿过去（生产里 install.sh 会设成 0711）
	base := t.TempDir()
	if err := os.Chmod(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o711); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// 用独立前缀 atl-t-（不是生产的 atl-i-），并在结束时清干净
	runner, err := runas.New("per-instance", "atl-t-")
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New(base, stateDir)
	reg.SetRunner(runner)

	mk := func(id string) *mcprocess.Instance {
		t.Helper()
		inst, err := reg.Create(registry.Meta{ID: id, Name: id})
		if err != nil {
			t.Fatalf("建实例 %s 失败: %v", id, err)
		}
		t.Cleanup(func() {
			_ = inst.Kill()
			reg.Delete(id)
			runner.Remove(id)
		})
		return inst
	}

	// A 实例：自报身份 + 试探 /etc 写入 + 读 /etc/shadow
	a := mk("iso-a")
	a.StartCommand = strings.Join([]string{
		"id -u > whoami.txt 2>&1",
		"id -un > whoami-name.txt 2>&1",
		"touch /etc/atl-isolation-probe 2>etc-write.txt; echo \"exit=$?\" >> etc-write.txt",
		"cat /etc/shadow > shadow.txt 2>&1",
		"echo ok",
	}, "; ")

	if err := a.Start(); err != nil {
		t.Fatalf("启动实例 A 失败: %v", err)
	}
	waitForFile(t, filepath.Join(a.Dir, "whoami.txt"), 15*time.Second)

	// ---- 断言 1：实例进程不是 root ----
	uidTxt := readFile(t, filepath.Join(a.Dir, "whoami.txt"))
	uid, err := strconv.Atoi(strings.TrimSpace(uidTxt))
	if err != nil {
		t.Fatalf("whoami.txt 内容不可解析: %q", uidTxt)
	}
	if uid == 0 {
		t.Fatal("实例进程以 root 运行 —— T0 未修复（这正是报告里的 POC）")
	}
	// 而且必须是那个专用用户，不是随便一个 uid
	id, err := runner.Resolve("iso-a")
	if err != nil {
		t.Fatalf("解析运行身份失败: %v", err)
	}
	if uint32(uid) != id.UID {
		t.Errorf("实例进程 uid 应为专用用户 %s(%d)，实际 %d", id.Username, id.UID, uid)
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(a.Dir, "whoami-name.txt"))); got != id.Username {
		t.Errorf("进程自报用户名应为 %s，实际 %q", id.Username, got)
	}

	// ---- 断言 2：写不了 /etc ----
	etcTry := readFile(t, filepath.Join(a.Dir, "etc-write.txt"))
	if !strings.Contains(etcTry, "exit=1") {
		t.Errorf("实例进程不该能往 /etc 写文件，实际: %q", etcTry)
	}
	if _, err := os.Stat("/etc/atl-isolation-probe"); err == nil {
		_ = os.Remove("/etc/atl-isolation-probe")
		t.Fatal("/etc 下被创建了探针文件 —— 实例仍能改宿主机（T0 未修复）")
	}

	// ---- 断言 2b：读不到 /etc/shadow ----
	shadow := readFile(t, filepath.Join(a.Dir, "shadow.txt"))
	if !strings.Contains(shadow, "Permission denied") && !strings.Contains(shadow, "权限不够") {
		t.Errorf("/etc/shadow 应读不到（Permission denied），实际: %q", firstLine(shadow))
	}

	// ---- 断言 3：跨实例不可读 ----
	// B 实例去读 A 的目录：同一个租户模型下这是另一台实例，必须被拒。
	b := mk("iso-b")
	b.StartCommand = "cat ../iso-a/whoami.txt > cross.txt 2>&1; ls ../iso-a > cross-ls.txt 2>&1; echo done"
	if err := b.Start(); err != nil {
		t.Fatalf("启动实例 B 失败: %v", err)
	}
	waitForFile(t, filepath.Join(b.Dir, "cross.txt"), 15*time.Second)

	cross := readFile(t, filepath.Join(b.Dir, "cross.txt"))
	if !strings.Contains(cross, "Permission denied") && !strings.Contains(cross, "权限不够") {
		t.Errorf("B 不该读到 A 的文件，实际: %q", firstLine(cross))
	}
	crossLs := readFile(t, filepath.Join(b.Dir, "cross-ls.txt"))
	if !strings.Contains(crossLs, "Permission denied") && !strings.Contains(crossLs, "权限不够") {
		t.Errorf("B 不该列出 A 的目录，实际: %q", firstLine(crossLs))
	}

	// ---- 断言 4：目录属主确实是实例用户 ----
	fi, err := os.Stat(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Uid != id.UID {
			t.Errorf("实例目录属主应为 %d，实际 %d", id.UID, st.Uid)
		}
	}

	// ---- 断言 5：root 信任的文件不在实例目录里 ----
	// frpc.toml / instance.json / daemon.pid 若留在实例目录，实例用户就能改写它们
	//（改 frpc 配置可把节点任意本地端口挂到自己的 frps 上，改 instance.json 可抹掉自己的配额）。
	for _, f := range []string{"instance.json", "daemon.pid", "frpc.toml", "tunnels.json"} {
		if _, err := os.Stat(filepath.Join(a.Dir, f)); err == nil {
			t.Errorf("%s 不该出现在实例目录里（它属于平台状态，必须在 state/frp 目录下）", f)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "iso-a", "instance.json")); err != nil {
		t.Errorf("元数据应在状态目录下: %v", err)
	}
}

// waitForFile 等文件出现（实例是异步进程，输出落盘需要时间）。
func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			// 再等一拍，确保内容写完（> 与 ; 之间可能有调度间隙）
			time.Sleep(200 * time.Millisecond)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待文件超时：%s", path)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(b)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
