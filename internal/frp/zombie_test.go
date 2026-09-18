package frp

import (
	"os"
	"path/filepath"
	"testing"
)

// 僵尸隧道的核心回归：清残留文件时 **tunnels.json 必须一起删**。
//
// 它保存着隧道定义，只要文件还在，下次实例启动 LoadFromDisk 就会把
// "面板里已经删掉的隧道"重新拉起来，继续在 frps 上占端口 ——
// 结果是实例自己绑不上端口（Failed to bind），而面板上完全看不出原因。
func TestCleanInstanceFilesRemovesMeta(t *testing.T) {
	dir := t.TempDir()

	// 造出穿透会产生的三个文件，外加一个无关文件（绝不能被误删）
	must := []string{configPath(dir), metaPath(dir), pidFilePath(dir)}
	for _, f := range must {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(keep, []byte("motd=hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	worldDir := filepath.Join(dir, "world")
	if err := os.MkdirAll(worldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	CleanInstanceFiles(dir)

	for _, f := range must {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s 应被删除，实际仍存在（err=%v）", filepath.Base(f), err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("无关文件被误删了：%v", err)
	}
	if _, err := os.Stat(worldDir); err != nil {
		t.Errorf("目录被误删了：%v", err)
	}
}

// 空目录参数应为安全的空操作（删除路径上 dir 可能取不到）。
func TestCleanInstanceFilesEmptyDirIsNoop(t *testing.T) {
	CleanInstanceFiles("") // 不该 panic、不该删任何东西
}

// 移除实例时，内存里的定义要消失，且不能影响其它实例。
func TestRemoveInstanceDropsDefinitions(t *testing.T) {
	m := NewManager("frpc")
	dirA, dirB := t.TempDir(), t.TempDir()

	m.byInst["a"] = &instanceFRP{
		server:  Server{Host: "1.2.3.4", BindPort: 7000},
		tunnels: map[string]Tunnel{"a-tcp-25570": {TunnelID: "a-tcp-25570", LocalPort: 25565, RemotePort: 25570}},
		dir:     dirA,
	}
	m.byInst["b"] = &instanceFRP{
		server:  Server{Host: "1.2.3.4", BindPort: 7000},
		tunnels: map[string]Tunnel{"b-tcp-25571": {TunnelID: "b-tcp-25571", LocalPort: 25565, RemotePort: 25571}},
		dir:     dirB,
	}
	// 给 a 造出残留文件
	for _, f := range []string{configPath(dirA), metaPath(dirA), pidFilePath(dirA)} {
		_ = os.WriteFile(f, []byte("x"), 0o600)
	}

	m.RemoveInstance("a")

	if m.HasTunnels("a") {
		t.Error("移除后 a 的隧道定义应已清空")
	}
	if !m.HasTunnels("b") {
		t.Error("b 的隧道定义不该被牵连")
	}
	if _, err := os.Stat(metaPath(dirA)); !os.IsNotExist(err) {
		t.Error("a 的 tunnels.json 应被删除")
	}

	// 对不存在的实例调用应安全
	m.RemoveInstance("nope")
}

// 移除**最后一条**隧道时，同样要把 tunnels.json 清掉（这是原实现漏掉的那一步）。
func TestRemoveLastTunnelClearsMetaFile(t *testing.T) {
	m := NewManager("frpc")
	dir := t.TempDir()

	st := &instanceFRP{
		server: Server{Host: "1.2.3.4", BindPort: 7000},
		dir:    dir,
		tunnels: map[string]Tunnel{
			"i-tcp-25570": {TunnelID: "i-tcp-25570", LocalPort: 25565, RemotePort: 25570},
		},
	}
	m.byInst["i"] = st
	// 先把定义持久化出来（模拟正常运行时留下的文件）
	if err := m.persist(dir, st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(metaPath(dir)); err != nil {
		t.Fatalf("前置条件不成立，tunnels.json 没写出来: %v", err)
	}

	if err := m.Remove("i", dir, "i-tcp-25570"); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}

	if _, err := os.Stat(metaPath(dir)); !os.IsNotExist(err) {
		t.Error("移除最后一条隧道后 tunnels.json 仍存在 —— 这正是僵尸隧道的成因：" +
			"下次实例启动 LoadFromDisk 会把已删除的隧道重新拉起来占住端口")
	}
	if _, err := os.Stat(configPath(dir)); !os.IsNotExist(err) {
		t.Error("frpc.toml 应被删除")
	}
}
