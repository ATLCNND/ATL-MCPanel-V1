package httpapi

import (
	"net/http"
	"testing"
)

// ============================================================================
// 实例公网端口：权限（owner 可自助）与配额归属（记在实例归属者头上）
// ============================================================================

// seedFrps 插入一条线路（frps 服务端）。
func seedFrps(t *testing.T, srv *Server, host string, start, end int32) int64 {
	t.Helper()
	res, err := srv.db.Exec(
		`INSERT INTO frps_servers (name, host, bind_port, token, port_start, port_end)
		 VALUES (?, ?, 7000, 'test-token', ?, ?)`,
		"线路-"+host, host, start, end)
	if err != nil {
		t.Fatalf("插入线路失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取线路 id 失败: %v", err)
	}
	return id
}

// seedInstanceOwnedBy 插入实例并指定 created_by。
//
// 用来模拟"总管理员替别人建实例"：这时 created_by 是管理员，
// 而实例归属者另有其人 —— 正是配额归属判定最容易出错的情形。
func seedInstanceOwnedBy(t *testing.T, srv *Server, instanceID string, nodeID, createdBy int64) {
	t.Helper()
	seedInstance(t, srv, instanceID, nodeID)
	if _, err := srv.db.Exec(
		`UPDATE instances SET created_by = ? WHERE instance_id = ?`, createdBy, instanceID); err != nil {
		t.Fatalf("设置 created_by 失败: %v", err)
	}
}

// uidOf 按用户名取 id。
func uidOf(t *testing.T, srv *Server, username string) int64 {
	t.Helper()
	var id int64
	if err := srv.db.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&id); err != nil {
		t.Fatalf("找不到用户 %s: %v", username, err)
	}
	return id
}

// assignLevel 直接写实例授权（绕过 HTTP，便于构造各种级别）。
func assignLevel(t *testing.T, srv *Server, instanceID string, userID int64, level string) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO instance_assignments (instance_id, user_id, level) VALUES (?, ?, ?)
		 ON CONFLICT(instance_id, user_id) DO UPDATE SET level = excluded.level`,
		instanceID, userID, level); err != nil {
		t.Fatalf("授权失败: %v", err)
	}
}

// setPortQuota 直接写端口配额。
func setPortQuota(t *testing.T, srv *Server, userID, frpsID int64, quota int) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO node_user_ports (user_id, frps_id, quota) VALUES (?, ?, ?)
		 ON CONFLICT(user_id, frps_id) DO UPDATE SET quota = excluded.quota`,
		userID, frpsID, quota); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
}

// seedTunnel 直接写一条隧道（用于验证用量推导）。
func seedTunnel(t *testing.T, srv *Server, tunnelID, instanceID string, frpsID int64, remote int32) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, status)
		 VALUES (?, ?, ?, ?, 'tcp', 8123, ?, 'running')`,
		tunnelID, instanceID, frpsID, tunnelID, remote); err != nil {
		t.Fatalf("插入隧道失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 权限：owner 可以管端口，但**不能**删除实例 / 改到期
// ---------------------------------------------------------------------------

// TestPortPermission_OwnerAllowedCollabDenied 是这一项改动的核心断言。
//
// owner 能管端口是刻意的**放宽**（多端口模组要使用者自己配），
// 而 collab 必须被挡住 —— 开端口等于扩大对外暴露面。
func TestPortPermission_OwnerAllowedCollabDenied(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "own1", "password": "own12345", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "col1", "password": "col12345", "role": "user"})

	seedInstance(t, srv, "inst1", 1)
	ownID := uidOf(t, srv, "own1")
	colID := uidOf(t, srv, "col1")
	assignLevel(t, srv, "inst1", ownID, LevelOwner)
	assignLevel(t, srv, "inst1", colID, LevelCollab)

	cases := []struct {
		name string
		uid  int64
		role string
		want bool
	}{
		{"owner 可管端口", ownID, RoleUser, true},
		{"collab 不可管端口", colID, RoleUser, false},
		{"总管理员可管端口", 999999, RoleAdmin, true},
	}
	for _, c := range cases {
		if got := srv.canManageInstancePorts(c.uid, c.role, "inst1"); got != c.want {
			t.Errorf("%s: canManageInstancePorts = %v，期望 %v", c.name, got, c.want)
		}
	}

	// 关键对照：owner 能管端口，但**不能**走管理层（删除/到期）那条路
	if srv.canManageInstance(ownID, RoleUser, "inst1") {
		t.Error("owner 不应通过 canManageInstance（删除实例/改到期仍须节点级权限）")
	}
	if !srv.canManageInstancePorts(ownID, RoleUser, "inst1") {
		t.Error("owner 应能通过 canManageInstancePorts")
	}
}

// TestPortPermission_ViewerDenied 确认 viewer 依然什么都改不了。
func TestPortPermission_ViewerDenied(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "vw1", "password": "vw123456", "role": "user"})

	seedInstance(t, srv, "inst1", 1)
	vwID := uidOf(t, srv, "vw1")
	assignLevel(t, srv, "inst1", vwID, LevelViewer)

	if srv.canManageInstancePorts(vwID, RoleUser, "inst1") {
		t.Error("viewer 不应能管端口")
	}
	// 完全没有授权的用户也不行
	if srv.canManageInstancePorts(123456, RoleUser, "inst1") {
		t.Error("无授权用户不应能管端口")
	}
}

// ---------------------------------------------------------------------------
// 配额归属：优先 owner 授权，其次 created_by
// ---------------------------------------------------------------------------

// TestQuotaOwner_PrefersOwnerAssignment 覆盖"管理员替别人建实例"这个关键场景。
//
// 若按 created_by 算，配额会记在管理员头上 —— 而管理员不限量，
// 等于那台实例的端口永远不消耗任何人的配额，普通用户的配额也就永远用不上。
func TestQuotaOwner_PrefersOwnerAssignment(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "bob1", "password": "bob12345", "role": "user"})

	// created_by = 管理员，但 owner 授权给 bob1
	adminID := uidOf(t, srv, "admin")
	bobID := uidOf(t, srv, "bob1")
	seedInstanceOwnedBy(t, srv, "inst1", 1, adminID)
	assignLevel(t, srv, "inst1", bobID, LevelOwner)

	if got := srv.instanceQuotaOwner("inst1"); got != bobID {
		t.Errorf("配额归属者应为 owner 授权者 bob1(%d)，实际 %d", bobID, got)
	}
}

// TestQuotaOwner_FallsBackToCreatedBy 覆盖没有 owner 授权时的回退。
func TestQuotaOwner_FallsBackToCreatedBy(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "car1", "password": "car12345", "role": "user"})

	carID := uidOf(t, srv, "car1")
	// 只有 created_by，没有任何授权行
	seedInstanceOwnedBy(t, srv, "inst1", 1, carID)
	if got := srv.instanceQuotaOwner("inst1"); got != carID {
		t.Errorf("无 owner 授权时应回退到 created_by=%d，实际 %d", carID, got)
	}

	// created_by 为 0（v17 之前的老实例）→ 返回 0，调用方须退回按操作者算
	seedInstance(t, srv, "inst2", 1)
	if got := srv.instanceQuotaOwner("inst2"); got != 0 {
		t.Errorf("created_by=0 的老实例应返回 0，实际 %d", got)
	}
	// 实例不存在同样返回 0，不应 panic
	if got := srv.instanceQuotaOwner("不存在"); got != 0 {
		t.Errorf("实例不存在应返回 0，实际 %d", got)
	}
}

// TestPortUsage_CountsOwnedInstances 确认用量按"归属者"推导，
// 而不是按 created_by —— 否则管理员替建的实例，用量永远记不到用户头上。
func TestPortUsage_CountsOwnedInstances(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "dave1", "password": "dv123456", "role": "user"})

	adminID := uidOf(t, srv, "admin")
	daveID := uidOf(t, srv, "dave1")
	frpsID := seedFrps(t, srv, "10.0.0.1", 25565, 25600)

	// 管理员替 dave1 建了两台实例（created_by 都是管理员），owner 授权给 dave1
	for _, id := range []string{"d1", "d2"} {
		seedInstanceOwnedBy(t, srv, id, 1, adminID)
		assignLevel(t, srv, id, daveID, LevelOwner)
	}
	seedTunnel(t, srv, "d1-tcp-1", "d1", frpsID, 25565)
	seedTunnel(t, srv, "d2-tcp-1", "d2", frpsID, 25566)

	if used := srv.portUsage(daveID)[frpsID]; used != 2 {
		t.Errorf("dave1 应已用 2 个端口，实际 %d", used)
	}
	// 关键：用量不该记在管理员头上
	if used := srv.portUsage(adminID)[frpsID]; used != 0 {
		t.Errorf("管理员名下不应有用量（实例归属者是 dave1），实际 %d", used)
	}
}

// ---------------------------------------------------------------------------
// HTTP 端到端
// ---------------------------------------------------------------------------

// TestAddPort_PlainUserAsOwnerSucceeds 走完整的 HTTP 路径：
// 普通用户被授权为 owner + 拿到配额 → 能给自己那台实例开端口。
func TestAddPort_PlainUserAsOwnerSucceeds(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "erin1", "password": "er123456", "role": "user"})
	erinTok := loginAs(t, ts, "erin1", "er123456")

	adminID := uidOf(t, srv, "admin")
	erinID := uidOf(t, srv, "erin1")
	frpsID := seedFrps(t, srv, "10.0.0.9", 25565, 25600)

	seedInstanceOwnedBy(t, srv, "inst1", 1, adminID)
	assignLevel(t, srv, "inst1", erinID, LevelOwner)

	// ① 还没有配额 → 应当被拒，并且提示的是"你还没有配额"
	code, body := doJSON(t, ts, "POST", "/api/instances/inst1/ports", erinTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8123})
	if code != http.StatusForbidden {
		t.Fatalf("无配额时应 403，实际 %d body=%v", code, body)
	}

	// ② 管理员发配额（普通用户也必须能被发放 —— 这是本项改动之一）
	code, body = doJSON(t, ts, "POST", "/api/node-users/ports", adminTok,
		map[string]interface{}{"username": "erin1", "frps_id": frpsID, "quota": 1})
	if code != http.StatusOK {
		t.Fatalf("给普通用户发配额应成功，实际 %d body=%v", code, body)
	}

	// ③ 现在能开了
	code, body = doJSON(t, ts, "POST", "/api/instances/inst1/ports", erinTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8123})
	if code != http.StatusCreated {
		t.Fatalf("owner 有配额时应能开端口，实际 %d body=%v", code, body)
	}

	var n int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE instance_id = 'inst1'`).Scan(&n); err != nil {
		t.Fatalf("查隧道失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应插入 1 条隧道，实际 %d", n)
	}

	// ④ 配额用尽（quota=1，已用 1）→ 再开应 409
	code, _ = doJSON(t, ts, "POST", "/api/instances/inst1/ports", erinTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8124})
	if code != http.StatusConflict {
		t.Errorf("配额用尽应 409，实际 %d", code)
	}
}

// TestAddPort_CollabDenied 确认协作者即使实例可见也开不了端口。
func TestAddPort_CollabDenied(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "frank1", "password": "fr123456", "role": "user"})
	frankTok := loginAs(t, ts, "frank1", "fr123456")

	frpsID := seedFrps(t, srv, "10.0.0.8", 25565, 25600)
	seedInstance(t, srv, "inst1", 1)
	frankID := uidOf(t, srv, "frank1")
	assignLevel(t, srv, "inst1", frankID, LevelCollab)
	setPortQuota(t, srv, frankID, frpsID, 5) // 就算有配额也不该放行

	code, body := doJSON(t, ts, "POST", "/api/instances/inst1/ports", frankTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8123})
	if code != http.StatusForbidden {
		t.Errorf("collab 应 403，实际 %d body=%v", code, body)
	}
}

// TestListPorts_CanEditAndLines 确认列表接口把 can_edit 与可用线路一并给出，
// 且线路是按**实例归属者**算的（不是操作者的）。
func TestListPorts_CanEditAndLines(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "gina1", "password": "gi123456", "role": "user"})
	ginaTok := loginAs(t, ts, "gina1", "gi123456")

	adminID := uidOf(t, srv, "admin")
	ginaID := uidOf(t, srv, "gina1")
	frpsID := seedFrps(t, srv, "10.0.0.7", 25565, 25600)

	seedInstanceOwnedBy(t, srv, "inst1", 1, adminID)
	assignLevel(t, srv, "inst1", ginaID, LevelOwner)
	setPortQuota(t, srv, ginaID, frpsID, 3)

	code, body := doJSON(t, ts, "GET", "/api/instances/inst1/ports", ginaTok, nil)
	if code != http.StatusOK {
		t.Fatalf("读端口列表应 200，实际 %d", code)
	}
	if body["can_edit"] != true {
		t.Errorf("owner 的 can_edit 应为 true，实际 %v", body["can_edit"])
	}
	if body["charging_self"] != true {
		t.Errorf("归属者本人操作时 charging_self 应为 true，实际 %v", body["charging_self"])
	}
	if got, _ := body["remaining"].(float64); got != 3 {
		t.Errorf("剩余应为 3，实际 %v", body["remaining"])
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("应返回 1 条可用线路，实际 %d (%v)", len(lines), body["lines"])
	}
	first, _ := lines[0].(map[string]any)
	if got, _ := first["available"].(float64); got != 3 {
		t.Errorf("线路可用数应为 3，实际 %v", first["available"])
	}

	// 管理员看同一实例：不限量，且所有线路都列出
	code, body = doJSON(t, ts, "GET", "/api/instances/inst1/ports", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("管理员读端口列表应 200，实际 %d", code)
	}
	if got, _ := body["remaining"].(float64); got != -1 {
		t.Errorf("管理员 remaining 应为 -1（不限），实际 %v", body["remaining"])
	}
}

// TestPortGrant_PlainUserAllowed 确认「必须是节点用户」的限制已去掉。
func TestPortGrant_PlainUserAllowed(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "hank1", "password": "hk123456", "role": "user"})
	frpsID := seedFrps(t, srv, "10.0.0.6", 25565, 25600)

	// 普通用户（不是节点用户）也应能拿到配额
	code, body := doJSON(t, ts, "POST", "/api/node-users/ports", adminTok,
		map[string]interface{}{"username": "hank1", "frps_id": frpsID, "quota": 2})
	if code != http.StatusOK {
		t.Fatalf("普通用户应能获得端口配额，实际 %d body=%v", code, body)
	}

	// 总管理员仍然被拒绝（他不受限，配额没有意义）
	code, _ = doJSON(t, ts, "POST", "/api/node-users/ports", adminTok,
		map[string]interface{}{"username": "admin", "frps_id": frpsID, "quota": 2})
	if code != http.StatusBadRequest {
		t.Errorf("给总管理员发配额应 400，实际 %d", code)
	}
}

// TestAddPort_QuotaChargedToInstanceOwner 节点用户替别人的实例开端口时，
// 扣的是**实例归属者**的配额，不是节点用户自己的。
//
// 这是修掉的一个真实漏洞：用量按实例归属者推导，
// 若扣操作者的配额，"用量永远不涨" → 节点用户可以无限制地给别人的实例开端口。
func TestAddPort_QuotaChargedToInstanceOwner(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "nu1", "password": "nu123456", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "ivy1", "password": "iv123456", "role": "user"})

	adminID := uidOf(t, srv, "admin")
	ivyID := uidOf(t, srv, "ivy1")
	frpsID := seedFrps(t, srv, "10.0.0.5", 25565, 25600)

	// 实例归属者是 ivy1
	seedInstanceOwnedBy(t, srv, "inst1", 1, adminID)
	assignLevel(t, srv, "inst1", ivyID, LevelOwner)

	// 走真实流程把 nu1 设为节点用户：这个接口会**同时**提升角色并写 node_users
	if code, body := doJSON(t, ts, "POST", "/api/node-users", adminTok,
		map[string]interface{}{"username": "nu1", "node_id": 1}); code != http.StatusOK {
		t.Fatalf("设为节点用户应成功，实际 %d body=%v", code, body)
	}
	// 必须在提权**之后**登录：角色是写在 JWT 里的，
	// 先登录拿到的令牌里还是 user，权限判定的第一关就过不去
	nuTok := loginAs(t, ts, "nu1", "nu123456")
	nuID := uidOf(t, srv, "nu1")

	// nu1 自己有配额 → 但归属者 ivy1 没有 → 必须被拒
	setPortQuota(t, srv, nuID, frpsID, 5)
	code, body := doJSON(t, ts, "POST", "/api/instances/inst1/ports", nuTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8123})
	if code != http.StatusForbidden {
		t.Fatalf("归属者无配额时应 403（不能扣操作者的配额），实际 %d body=%v", code, body)
	}

	// 给归属者发配额后即可开通
	setPortQuota(t, srv, ivyID, frpsID, 1)
	code, body = doJSON(t, ts, "POST", "/api/instances/inst1/ports", nuTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8123})
	if code != http.StatusCreated {
		t.Fatalf("归属者有配额后应能开通，实际 %d body=%v", code, body)
	}

	// 用量记在归属者头上，且不记在操作者头上
	if used := srv.portUsage(ivyID)[frpsID]; used != 1 {
		t.Errorf("归属者应已用 1 个端口，实际 %d", used)
	}
	if used := srv.portUsage(nuID)[frpsID]; used != 0 {
		t.Errorf("操作者名下不应有用量，实际 %d", used)
	}

	// 归属者配额已用尽 → 再开应 409（证明扣的确实是归属者的额度）
	code, _ = doJSON(t, ts, "POST", "/api/instances/inst1/ports", nuTok,
		map[string]interface{}{"frps_id": frpsID, "local_port": 8124})
	if code != http.StatusConflict {
		t.Errorf("归属者配额用尽应 409，实际 %d", code)
	}
}
