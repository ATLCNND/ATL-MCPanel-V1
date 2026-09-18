package httpapi

import (
	"net/http"
	"testing"
)

// 容器化开关**只能由总管理员控制**。
//
// 为什么这条要单独写测试：容器化隔离限制的正是实例里的进程，而实例 owner
// 能在启动脚本、控制台、插件里执行任意命令（这一点已实测：控制台那一行
// 由谁解释取决于实例进程，改成 `exec bash` 就是任意系统命令）。
// **如果 owner 自己能关掉隔离，那隔离就只是他的一句承诺** —— 随时可以撤回，
// 拿回宿主文件可见、宿主服务可达、宿主进程可见这些能力。
//
// 所以它是平台侧设置，与"每实例专用用户""实例目录 0700"同级；
// 不是"实例自己的事"（后者才是 public 端口那类 owner 可自助的操作）。
func TestContainerToggle_AdminOnly(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "ctrowner", "password": "ctr123456", "role": "user"})

	seedInstance(t, srv, "inst1", 1)
	ownID := uidOf(t, srv, "ctrowner")
	assignLevel(t, srv, "inst1", ownID, LevelOwner)
	ownTok := loginAs(t, ts, "ctrowner", "ctr123456")

	// ① 实例 owner（权限最高的租户角色）也必须被挡住
	code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/container", ownTok,
		map[string]bool{"enabled": true})
	if code != http.StatusForbidden {
		t.Fatalf("owner 修改容器化应被拒绝（403），实际 %d：%v", code, body)
	}

	// ② 连读都可以，但写不行 —— 读是运行信息，不敏感
	if code, _ := doJSON(t, ts, "GET", "/api/instances/inst1/container", ownTok, nil); code == http.StatusForbidden {
		t.Error("owner 应当能看到实例的容器化状态（只读）")
	}

	// ③ 未登录一律拒绝
	if code, _ := doJSON(t, ts, "PUT", "/api/instances/inst1/container", "",
		map[string]bool{"enabled": true}); code != http.StatusUnauthorized {
		t.Errorf("未登录修改容器化应返回 401，实际 %d", code)
	}

	// ④ 总管理员能通过权限这一关。
	//    这里不要求 200：测试环境没有可用的 Daemon 连接，
	//    管理员走到 Daemon 调用后会因为实例查不到而失败 ——
	//    关键是**它不是 403**（权限已放行）。
	code, body = doJSON(t, ts, "PUT", "/api/instances/inst1/container", adminTok,
		map[string]bool{"enabled": false})
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		t.Fatalf("总管理员修改容器化不该被权限拦下，实际 %d：%v", code, body)
	}
}

// TestContainerToggle_NotOwnerControllable 是对上面那条的"反向保险"：
// 万一以后有人把路由改回 requireAuth + LevelOwner，这里会立刻红。
func TestContainerToggle_NotOwnerControllable(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "ctrnode", "password": "ctr123456", "role": "nodeuser"})

	seedInstance(t, srv, "inst1", 1)
	nodeID := uidOf(t, srv, "ctrnode")
	nodeTok := loginAs(t, ts, "ctrnode", "ctr123456")

	// 节点用户（能管节点资源与端口，权限不低）同样不能开关隔离：
	// 容器化是节点级安全属性，只有总管理员能定。
	if code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/container", nodeTok,
		map[string]bool{"enabled": true}); code != http.StatusForbidden {
		t.Fatalf("节点用户修改容器化应被拒绝（403），实际 %d：%v", code, body)
	}
	_ = nodeID
}
