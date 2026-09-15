package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// ============================================================================
// 信息暴露：普通用户不该看到公网服务器 / 节点的真实地址
// ============================================================================
//
// 背景（用户反馈）：创建实例时那一行「线路」的小字里显示了公网服务器的 IP。
// 界面上不显示只是第一道；**接口本身也不该发** —— 否则随手 curl 就能拿到。
// 所以这里断言到"响应体里根本没有那个字符串"这一层。

// TestMyPortsHidesFrpsHostFromNonAdmin 普通用户的 /api/my/ports 不带 frps_host。
func TestMyPortsHidesFrpsHostFromNonAdmin(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "pv1", "password": "pv123456", "role": "user"})
	userTok := loginAs(t, ts, "pv1", "pv123456")

	const secretIP = "203.0.113.9"
	frpsID := seedFrps(t, srv, secretIP, 25565, 25600)
	// seedFrps 把 host 拼进了线路名（"线路-<host>"），于是名字里就带 IP 了。
	// 但**线路名是管理员自己起的**、本来就打算展示给用户看，
	// 不在本次修复范围（要藏也是管理员自己改名）。这里先换成中性名，
	// 好让断言盯住真正的问题：host 字段本身有没有外泄。
	if _, err := srv.db.Exec(`UPDATE frps_servers SET name = ? WHERE id = ?`, "测试线路", frpsID); err != nil {
		t.Fatalf("改线路名失败: %v", err)
	}
	setPortQuota(t, srv, uidOf(t, srv, "pv1"), frpsID, 3)

	// ① 普通用户：拿到线路名与配额，但 host 为空
	_, mine := doJSONArr(t, ts, "GET", "/api/my/ports", userTok, nil)
	if len(mine) != 1 {
		t.Fatalf("普通用户应看到 1 条线路，实际 %d", len(mine))
	}
	if mine[0]["frps_host"] != "" {
		t.Errorf("普通用户不该拿到 frps_host，实际 %v", mine[0]["frps_host"])
	}
	if mine[0]["frps_name"] == "" {
		t.Errorf("线路名要保留（用户得知道选哪条）")
	}
	if got, _ := mine[0]["quota"].(float64); got != 3 {
		t.Errorf("配额要保留，实际 %v", mine[0]["quota"])
	}

	// ② 更严的一条：响应体里**根本不该出现**那个地址
	code, raw := call(t, ts, "GET", "/api/my/ports", userTok, nil)
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", code)
	}
	if strings.Contains(string(raw), secretIP) {
		t.Errorf("响应体里仍含公网服务器地址：%s", string(raw))
	}

	// ③ 管理员仍然看得到（排查问题要用）
	_, adminList := doJSONArr(t, ts, "GET", "/api/my/ports", adminTok, nil)
	found := false
	for _, l := range adminList {
		if l["frps_host"] == secretIP {
			found = true
		}
	}
	if !found {
		t.Errorf("总管理员应仍能看到线路地址，实际 %v", adminList)
	}
}

// TestMyNodesHidesIPFromNodeUser 节点用户拿到的节点列表不带 IP。
//
// 同一个道理：创建实例时的节点下拉框会显示 IP，而节点用户只需要
// 知道"能放到哪台机器上"（名字够了）。
func TestMyNodesHidesIPFromNodeUser(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	// seedInstance 会顺带建出 id=1 的节点
	seedInstance(t, srv, "inst1", 1)

	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "pv2", "password": "pv123456", "role": "user"})
	if code, body := doJSON(t, ts, "POST", "/api/node-users", adminTok,
		map[string]interface{}{"username": "pv2", "node_id": 1}); code != http.StatusOK {
		t.Fatalf("设为节点用户应成功，实际 %d body=%v", code, body)
	}
	// 角色写在 JWT 里，提权后要重新登录
	nodeTok := loginAs(t, ts, "pv2", "pv123456")

	code, raw := call(t, ts, "GET", "/api/my/nodes", nodeTok, nil)
	if code != http.StatusOK {
		t.Fatalf("节点用户读 /api/my/nodes 应 200，实际 %d", code)
	}
	if strings.Contains(string(raw), "127.0.0.1") {
		t.Errorf("节点用户不该看到节点 IP：%s", string(raw))
	}
	_, mine := doJSONArr(t, ts, "GET", "/api/my/nodes", nodeTok, nil)
	if len(mine) != 1 {
		t.Fatalf("应看到 1 个节点，实际 %d", len(mine))
	}
	if mine[0]["ip"] != "" {
		t.Errorf("ip 应为空，实际 %v", mine[0]["ip"])
	}
	if mine[0]["name"] == "" {
		t.Errorf("节点名要保留")
	}

	// 管理员照旧
	_, adminNodes := doJSONArr(t, ts, "GET", "/api/my/nodes", adminTok, nil)
	if len(adminNodes) != 1 || adminNodes[0]["ip"] == "" {
		t.Errorf("总管理员应仍能看到节点 IP，实际 %v", adminNodes)
	}
}
