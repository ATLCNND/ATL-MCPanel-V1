package grpcapi

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// level-name 不能当路径用（2026-10-01 安全审查的 MEDIUM 发现）。
//
// server.properties 在实例目录里、租户随手可改，而 level-name 会被拼成
// `<实例目录>/<level-name>/stats` 之后由 root 去 ReadDir/ReadFile。
// 于是 `level-name=../<别人的实例>/world` 就能让玩家总览接口枚举、解析别人的
// `world/stats/*.json`（玩家名、UUID、游戏时长）并把结果返回 —— 跨租户的信息
// 泄露，还会把 world_name 一起回显出去。
//
// 现在：非法值按默认值（world）处理，接口本身仍然成功（它只是只读总览，
// 没必要因为一个坏配置项整体失败）。
func TestGetPlayerOverviewRejectsLevelNameTraversal(t *testing.T) {
	const victimUUID = "11111111-2222-3333-4444-555555555555"

	srv, reg := newSecurityTestServer(t, "")
	victim, err := reg.Create(registry.Meta{ID: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	statsDir := filepath.Join(victim.Dir, "world", "stats")
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stats := `{"stats":{"minecraft:custom":{"minecraft:play_time":1200}}}`
	if err := os.WriteFile(filepath.Join(statsDir, victimUUID+".json"), []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}

	attacker, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	// 攻击者把自己实例里的 level-name 指到受害者的世界
	if err := os.WriteFile(filepath.Join(attacker.Dir, "server.properties"),
		[]byte("level-name=../victim/world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetPlayerOverview(context.Background(), &pb.InstanceRequest{InstanceId: "alpha"})
	if err != nil {
		t.Fatalf("GetPlayerOverview 返回错误: %v", err)
	}
	if !resp.Success {
		t.Fatalf("非法 level-name 不该让接口整体失败，实际: %s", resp.Error)
	}
	if resp.WorldName != defaultWorldName {
		t.Errorf("非法 level-name 应退回默认值 %q，实际 %q", defaultWorldName, resp.WorldName)
	}
	if len(resp.Players) != 0 {
		t.Errorf("不应返回任何玩家（那说明读了别人的 world/stats）：%v", resp.Players)
	}
	for _, p := range resp.Players {
		if p.Uuid == victimUUID {
			t.Errorf("读到了别的实例的玩家数据：%+v", p)
		}
	}
}

// 实例内的 world 目录是软链接指向别人 world 时，同样不能读（另一种写法）。
func TestGetPlayerOverviewRejectsSymlinkedWorld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上创建软链接需要额外权限，且 Daemon 只发布 Linux 版本")
	}
	const victimUUID = "99999999-8888-7777-6666-555555555555"

	srv, reg := newSecurityTestServer(t, "")
	victim, err := reg.Create(registry.Meta{ID: "victim"})
	if err != nil {
		t.Fatal(err)
	}
	statsDir := filepath.Join(victim.Dir, "world", "stats")
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stats := `{"stats":{"minecraft:custom":{"minecraft:play_time":2400}}}`
	if err := os.WriteFile(filepath.Join(statsDir, victimUUID+".json"), []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}

	attacker, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	// level-name 是合法（默认）的，但 world 这个目录本身是软链接
	if err := os.Symlink(filepath.Join(victim.Dir, "world"), filepath.Join(attacker.Dir, "world")); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetPlayerOverview(context.Background(), &pb.InstanceRequest{InstanceId: "alpha"})
	if err != nil {
		t.Fatalf("GetPlayerOverview 返回错误: %v", err)
	}
	if len(resp.Players) != 0 {
		t.Errorf("world 是软链接时不应读到别的实例的玩家数据：%v", resp.Players)
	}
	if resp.WorldName != defaultWorldName {
		t.Errorf("越界时应退回默认世界名，实际 %q", resp.WorldName)
	}
}

// 合法（自定义）的 level-name 不能被误伤 —— 加固最常见的事故就是把正常用法挡掉。
func TestGetPlayerOverviewAcceptsNormalLevelName(t *testing.T) {
	const uuid = "12121212-3434-5656-7878-909090909090"

	srv, reg := newSecurityTestServer(t, "")
	inst, err := reg.Create(registry.Meta{ID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.Dir, "server.properties"),
		[]byte("level-name=my_world\nwhite-list=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statsDir := filepath.Join(inst.Dir, "my_world", "stats")
	if err := os.MkdirAll(statsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stats := `{"stats":{"minecraft:custom":{"minecraft:play_time":600}}}`
	if err := os.WriteFile(filepath.Join(statsDir, uuid+".json"), []byte(stats), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetPlayerOverview(context.Background(), &pb.InstanceRequest{InstanceId: "alpha"})
	if err != nil {
		t.Fatalf("GetPlayerOverview 返回错误: %v", err)
	}
	if resp.WorldName != "my_world" {
		t.Errorf("合法的 level-name 应被采用，实际 %q", resp.WorldName)
	}
	if !resp.WhitelistEnabled {
		t.Error("white-list=true 应被解析出来")
	}
	found := false
	for _, p := range resp.Players {
		if p.Uuid == uuid {
			found = true
		}
	}
	if !found {
		t.Errorf("实例自己的 world/stats 没被读到：%v", resp.Players)
	}
}

// validWorldName 的取值边界（含分隔符、上跳、绝对路径一律不认）。
func TestValidWorldName(t *testing.T) {
	bad := []string{
		"",
		".",
		"..",
		"../victim/world",
		"a/../../b",
		"a\\b",
		"/etc",
		"world/../..",
	}
	for _, n := range bad {
		if validWorldName(n) {
			t.Errorf("%q 不该被当成合法的世界目录名", n)
		}
	}
	good := []string{"world", "world_nether", "my world", "世界", "world-2026"}
	for _, n := range good {
		if !validWorldName(n) {
			t.Errorf("%q 是合法的目录名，不该被拒绝", n)
		}
	}
	// 刻意从严：名字里只要出现 ".." 就拒绝。目录名里带连续两个点几乎只可能是
	// 构造出来的（真正的世界名不会这么写），而放行它就得额外推理"哪一段是上跳"，
	// 不如一刀切 —— 误伤的代价只是退回默认的 world。
	if validWorldName("a..b") {
		t.Error("含 .. 的名字应被拒绝（刻意的从严口径）")
	}
}
