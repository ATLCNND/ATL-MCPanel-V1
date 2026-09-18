package httpapi

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// —— 端口分配：跨实例避让（2026-09-17 建第二个实例时踩到的坑）——
//
// 用真实的 sqlite 临时库：这几个函数的行为完全由 SQL 决定（避开哪些端口、
// 哪些端口已被占），用 mock 反而测不出真问题。
func newPortTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, ddl := range []string{
		`CREATE TABLE nodes (id INTEGER PRIMARY KEY, name TEXT, ip TEXT)`,
		`CREATE TABLE instances (instance_id TEXT, node_id INTEGER, port INTEGER)`,
		`CREATE TABLE tunnels (remote_port INTEGER, frps_id INTEGER)`,
		`CREATE TABLE frps_servers (id INTEGER PRIMARY KEY, host TEXT, bind_port INTEGER, token TEXT,
		    port_start INTEGER, port_end INTEGER, display_domain TEXT)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("建表失败: %v", err)
		}
	}
	return &Server{db: db}
}

// portsInUseOnNode 要返回**全部**实例端口，而不是某个实例自己的。
func TestPortsInUseOnNodeReturnsAll(t *testing.T) {
	s := newPortTestServer(t)
	if _, err := s.db.Exec(
		`INSERT INTO instances (instance_id, node_id, port) VALUES ('a',1,25565),('b',1,25567),('c',2,25569)`); err != nil {
		t.Fatal(err)
	}

	got := s.portsInUseOnNode(1)
	if !got[25565] || !got[25567] {
		t.Errorf("节点 1 的端口应含 25565 与 25567，实际 %v", got)
	}
	if got[25569] {
		t.Errorf("25569 属于节点 2，不该出现在节点 1 的集合里：%v", got)
	}
}

// 同机时，分配公网端口必须避开该节点上所有实例端口 ——
// 这正是"第二个实例把第一个实例挤掉"的那个 bug。
func TestAllocAvoidsAllInstancePortsOnSameMachine(t *testing.T) {
	s := newPortTestServer(t)
	_, _ = s.db.Exec(`INSERT INTO frps_servers (id,host,bind_port,token,port_start,port_end,display_domain)
	                  VALUES (1,'127.0.0.1',7000,'tok',25565,25600,'')`)
	_, _ = s.db.Exec(`INSERT INTO nodes (id,name,ip) VALUES (1,'n1','127.0.0.1')`)
	// 已有实例 a 占 25565；现在给新实例 b 分配公网端口
	_, _ = s.db.Exec(`INSERT INTO instances (instance_id,node_id,port) VALUES ('a',1,25565),('b',1,25567)`)

	avoid := s.remotePortsToAvoid(1, "b")
	if !avoid[25565] {
		t.Fatalf("必须避开**别的实例**的端口 25565，实际 avoid=%v", avoid)
	}
	if !avoid[25567] {
		t.Errorf("也必须避开自己的端口 25567，实际 avoid=%v", avoid)
	}

	p, err := s.allocRemotePortAvoiding(1, 25565, 25600, avoid)
	if err != nil {
		t.Fatalf("分配失败: %v", err)
	}
	if avoid[p] {
		t.Errorf("分到的端口 %d 落在要避开的集合里 —— 会与实例抢绑定", p)
	}
	if p != 25566 {
		t.Errorf("应分到 25566（25565 被实例占、25566 空闲），实际 %d", p)
	}
}

// 不同机时不该避开任何端口（远端 frps 绑端口不会和实例冲突）。
func TestNoAvoidWhenLineIsRemote(t *testing.T) {
	s := newPortTestServer(t)
	_, _ = s.db.Exec(`INSERT INTO frps_servers (id,host,bind_port,token,port_start,port_end,display_domain)
	                  VALUES (1,'frp.example.com',7000,'tok',25565,25600,'')`)
	_, _ = s.db.Exec(`INSERT INTO nodes (id,name,ip) VALUES (1,'n1','10.0.0.5')`)
	_, _ = s.db.Exec(`INSERT INTO instances (instance_id,node_id,port) VALUES ('a',1,25565)`)

	avoid := s.remotePortsToAvoid(1, "a")
	if len(avoid) != 0 {
		t.Errorf("远端线路不该避开任何端口，实际 %v", avoid)
	}
	// 于是能分到 25565（远端 frps 上"公网端口 == 实例端口"是正常用法）
	p, err := s.allocRemotePortAvoiding(1, 25565, 25600, avoid)
	if err != nil || p != 25565 {
		t.Errorf("期望分到 25565，实际 %d（err=%v）", p, err)
	}
}

// 端口段里只剩被避开的那些时，仍要能开出端口（宁可复用，也不要"开不出来"）。
func TestAllocFallsBackWhenRangeExhausted(t *testing.T) {
	s := newPortTestServer(t)
	// 端口段只有两个：25565（要避开）与 25566（已被隧道占）
	_, _ = s.db.Exec(`INSERT INTO tunnels (frps_id, remote_port) VALUES (1, 25566)`)

	p, err := s.allocRemotePortAvoiding(1, 25565, 25566, map[int32]bool{25565: true})
	if err != nil {
		t.Fatalf("应回退到被避开的端口，而不是报「已用尽」: %v", err)
	}
	if p != 25565 {
		t.Errorf("应回退到 25565，实际 %d", p)
	}
}

// 端口段真的用尽时要报错，而不是返回 0 让调用方拿到一个无效端口。
func TestAllocReportsExhausted(t *testing.T) {
	s := newPortTestServer(t)
	_, _ = s.db.Exec(`INSERT INTO tunnels (frps_id, remote_port) VALUES (1, 25565),(1, 25566)`)

	p, err := s.allocRemotePortAvoiding(1, 25565, 25566, nil)
	if err == nil {
		t.Errorf("应报端口用尽，实际返回 p=%d", p)
	}
	if p != 0 {
		t.Errorf("失败时应返回 0，实际 %d", p)
	}
}

// 实例不存在时不该 panic，也不该误判成"同机"而乱避端口。
func TestRemotePortsToAvoidUnknownInstance(t *testing.T) {
	s := newPortTestServer(t)
	if got := s.remotePortsToAvoid(1, "不存在"); got != nil {
		t.Errorf("查不到的实例应返回 nil，实际 %v", got)
	}
}
