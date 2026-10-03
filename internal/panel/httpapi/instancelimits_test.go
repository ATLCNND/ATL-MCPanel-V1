package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// ============================================================================
// 实例资源上限的修改（PUT /api/instances/{id}/limits）
// ============================================================================

// seedLimits 直接写库设置三个上限（绕过 HTTP），便于构造"改之前"的状态。
func seedLimits(t *testing.T, srv *Server, instanceID string, cpu int, mem string, diskMB int64) {
	t.Helper()
	if _, err := srv.db.Exec(
		`UPDATE instances SET cpu_quota = ?, mem_limit = ?, disk_limit_mb = ? WHERE instance_id = ?`,
		cpu, mem, diskMB, instanceID); err != nil {
		t.Fatalf("写入初始上限失败: %v", err)
	}
}

// limitsOf 读出三个上限。COALESCE 是必要的：老实例上这两列可能是 NULL。
func limitsOf(t *testing.T, srv *Server, instanceID string) (cpu int, mem string, diskMB int64) {
	t.Helper()
	if err := srv.db.QueryRow(
		`SELECT cpu_quota, COALESCE(mem_limit, ''), COALESCE(disk_limit_mb, 0)
		 FROM instances WHERE instance_id = ?`, instanceID).Scan(&cpu, &mem, &diskMB); err != nil {
		t.Fatalf("读上限失败: %v", err)
	}
	return cpu, mem, diskMB
}

// countLimitAudits 数一数资源上限相关的审计条数（用来断言"空操作不留痕"）。
func countLimitAudits(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.db.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'update_instance_limits'`).Scan(&n); err != nil {
		t.Fatalf("读审计条数失败: %v", err)
	}
	return n
}

// TestInstanceLimits_OwnerCanUpdate 覆盖本功能的主诉求：
// 一台三个上限都留成"不限制"的实例（节点用户建实例时的默认写法），
// 终于能被改住 —— 而且库里真的变了。
func TestInstanceLimits_OwnerCanUpdate(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	ownTok := mkUser(t, srv, ts, adminTok, "own1", "own12345", RoleUser)

	seedInstance(t, srv, "inst1", 1)
	seedLimits(t, srv, "inst1", 0, "", 0) // 建实例时的默认：三项都不限制
	assignLevel(t, srv, "inst1", uidOf(t, srv, "own1"), LevelOwner)

	code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", ownTok, map[string]any{
		"cpu_quota": 150, "mem_limit": "4G", "disk_limit_mb": 10240,
	})
	if code != http.StatusOK {
		t.Fatalf("owner 改自己实例的上限应 200，实际 %d body=%v", code, body)
	}
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 150 || mem != "4G" || disk != 10240 {
		t.Errorf("库里的上限应为 150/4G/10240，实际 %d/%q/%d", cpu, mem, disk)
	}
	// 响应要把新值带回（界面据此回显），并说明生效时机
	if body["restart_required"] != true {
		t.Errorf("改了 CPU / 内存后 restart_required 应为 true，实际 %v", body["restart_required"])
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "启动") {
		t.Errorf("响应必须说明何时生效（文案应含「启动」），实际 %q", msg)
	}

	// 审计要能看出"谁把哪个上限从多少改成了多少"：只留新值的话，
	// 翻旧日志时根本分不清是被放开了还是被收紧了。
	var detail string
	if err := srv.db.QueryRow(
		`SELECT detail FROM audit_logs WHERE action = 'update_instance_limits' ORDER BY id DESC LIMIT 1`).
		Scan(&detail); err != nil {
		t.Fatalf("应记录审计日志: %v", err)
	}
	if !strings.Contains(detail, "→") || !strings.Contains(detail, "不限制") {
		t.Errorf("审计详情应带上旧值 → 新值，实际 %q", detail)
	}
}

// TestInstanceLimits_ViewerCollabAndStrangerDenied 是本项改动的权限底线：
// 只有"实例 owner / 该节点的节点用户 / 总管理员"能改；collab 与 viewer、
// 以及完全无关的用户都必须被挡在门外。
//
// 为什么 collab 也不行：资源上限挤占的是**同节点上其它实例**（CPU 抢满时
// 最先受害的是邻居），不是"被授权启停 + 看控制台"的人该决定的事。
func TestInstanceLimits_ViewerCollabAndStrangerDenied(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	vwTok := mkUser(t, srv, ts, adminTok, "vw1", "vw123456", RoleUser)
	colTok := mkUser(t, srv, ts, adminTok, "col1", "col12345", RoleUser)
	outTok := mkUser(t, srv, ts, adminTok, "out1", "out12345", RoleUser)

	seedInstance(t, srv, "inst1", 1)
	seedLimits(t, srv, "inst1", 200, "3G", 5120)
	assignLevel(t, srv, "inst1", uidOf(t, srv, "vw1"), LevelViewer)
	assignLevel(t, srv, "inst1", uidOf(t, srv, "col1"), LevelCollab)

	cases := []struct{ name, tok string }{
		{"viewer", vwTok},
		{"collab", colTok},
		{"无任何授权的用户", outTok},
	}
	for _, c := range cases {
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", c.tok,
			map[string]any{"mem_limit": "1G"})
		if code != http.StatusForbidden {
			t.Errorf("%s 改资源上限应 403，实际 %d body=%v", c.name, code, body)
		}
	}
	// 被拒的请求一个字段都不该动
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 200 || mem != "3G" || disk != 5120 {
		t.Errorf("被拒的请求不该改动任何字段，实际 %d/%q/%d", cpu, mem, disk)
	}
}

// TestInstanceLimits_NodeUserOfThatNodeCanUpdate 覆盖授权的另一侧：
// 实例登记在别人名下（比如管理员替用户建的），但该节点的节点用户要能改 ——
// 收/放资源上限正是他对那台机器负责时该做的事。
func TestInstanceLimits_NodeUserOfThatNodeCanUpdate(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	mkUser(t, srv, ts, adminTok, "nu1", "nu123456", RoleUser)

	seedInstance(t, srv, "inst1", 1)
	seedLimits(t, srv, "inst1", 0, "", 0)

	// 走真实流程提权为节点用户（这个接口会同时改角色并写 node_users）
	if code, body := doJSON(t, ts, "POST", "/api/node-users", adminTok,
		map[string]any{"username": "nu1", "node_id": 1}); code != http.StatusOK {
		t.Fatalf("设为节点用户应成功，实际 %d body=%v", code, body)
	}
	// 必须在提权**之后**登录：角色写在 JWT 里（见 mkUser 的注释）
	nuTok := loginAsToken(t, ts, "nu1", "nu123456")

	code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", nuTok,
		map[string]any{"mem_limit": "1536m"})
	if code != http.StatusOK {
		t.Fatalf("该节点的节点用户应能改上限，实际 %d body=%v", code, body)
	}
	if _, mem, _ := limitsOf(t, srv, "inst1"); mem != "1536m" {
		t.Errorf("mem_limit 应为 1536m，实际 %q", mem)
	}
}

// TestInstanceLimits_InvalidValuesRejected 覆盖校验。
//
// 重点是 mem_limit：这个字符串会被 Daemon 以 root 原样写进 cgroup 的
// memory.max（见 daemon/cgroup/v2.go 的 v2SetMemoryLimit），所以"看着像内存"
// 不够，必须严格到"带空格、带分号、带路径、两个 token"一律拒掉。
func TestInstanceLimits_InvalidValuesRejected(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	bad := []struct {
		name string
		body map[string]any
	}{
		{"带空格", map[string]any{"mem_limit": "4 G"}},
		{"两个 token", map[string]any{"mem_limit": "4G 8G"}},
		{"命令注入", map[string]any{"mem_limit": "4G;rm -rf /"}},
		{"路径穿越", map[string]any{"mem_limit": "../../etc/passwd"}},
		{"小数", map[string]any{"mem_limit": "1.5G"}},
		{"负数", map[string]any{"mem_limit": "-1G"}},
		{"非数字", map[string]any{"mem_limit": "abc"}},
		{"i 后缀", map[string]any{"mem_limit": "1GiB"}},
		{"换行", map[string]any{"mem_limit": "4G\n1G"}},
		{"位数超出 int64", map[string]any{"mem_limit": "99999999999999999999999"}},
		{"数值过大", map[string]any{"mem_limit": "4096G"}},
		{"CPU 为负", map[string]any{"cpu_quota": -1}},
		{"CPU 超上限", map[string]any{"cpu_quota": maxCPUQuotaPct + 1}},
		{"磁盘为负", map[string]any{"disk_limit_mb": -1}},
		{"磁盘超上限", map[string]any{"disk_limit_mb": maxDiskLimitMB + 1}},
		{"一个字段都没带", map[string]any{}},
	}
	for _, c := range bad {
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok, c.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d body=%v", c.name, code, body)
		}
	}
	// 全程库都不该被动过（seedInstance 的默认值就是三项都不限制）
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 0 || mem != "" || disk != 0 {
		t.Errorf("被拒的请求不该改动库，实际 %d/%q/%d", cpu, mem, disk)
	}
}

// TestInstanceLimits_AcceptsCgroupSizeForms 把"接受哪些写法"钉住：
// 这些都是内核 memparse 认的形式，拒掉任何一个都会让用户莫名其妙
//（"4G 能填、4GB 就说我不合法？"）。
func TestInstanceLimits_AcceptsCgroupSizeForms(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	for _, v := range []string{"512M", "4G", "1536m", "512MB", "512B", "1073741824", "1T"} {
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok,
			map[string]any{"mem_limit": v})
		if code != http.StatusOK {
			t.Fatalf("mem_limit=%q 应被接受，实际 %d body=%v", v, code, body)
		}
		// 存的是用户写的那串字面量本身：换算交给 cgroup / docker，面板不替它们改格式
		if _, mem, _ := limitsOf(t, srv, "inst1"); mem != v {
			t.Errorf("mem_limit 应原样存下 %q，实际 %q", v, mem)
		}
	}
}

// TestInstanceLimits_PartialUpdateKeepsOtherFields 覆盖最容易出事那条路：
// 只改 cpu_quota 时另外两列必须**原样不动**。
//
// 用值类型而不是指针的话，这里会顺手把没传的字段洗成零值 ——
// 也就是"只想改一个核数，结果悄悄把内存与磁盘的限制都放开了"。
func TestInstanceLimits_PartialUpdateKeepsOtherFields(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)
	seedLimits(t, srv, "inst1", 200, "3G", 5120)

	code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok,
		map[string]any{"cpu_quota": 400})
	if code != http.StatusOK {
		t.Fatalf("部分更新应 200，实际 %d body=%v", code, body)
	}
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 400 || mem != "3G" || disk != 5120 {
		t.Errorf("只改 cpu_quota 时另外两列应保持 3G/5120，实际 %d/%q/%d", cpu, mem, disk)
	}

	// 显式传空串 = 主动放开内存上限：这是**有意义的取值**，不是"没传这个字段"。
	// 指针存在的意义就在这条路径上（用值类型两者根本区分不开）。
	code, body = doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok,
		map[string]any{"mem_limit": ""})
	if code != http.StatusOK {
		t.Fatalf("显式清空 mem_limit 应 200，实际 %d body=%v", code, body)
	}
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 400 || mem != "" || disk != 5120 {
		t.Errorf("清空 mem_limit 后应只有它变空，实际 %d/%q/%d", cpu, mem, disk)
	}

	// 值没变（用户点了两次保存）：不该写库、也不该记审计，
	// 否则审计日志会被这种空操作刷满，反而盖住真正的改动。
	before := countLimitAudits(t, srv)
	code, body = doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok,
		map[string]any{"cpu_quota": 400})
	if code != http.StatusOK {
		t.Fatalf("值未变化也应 200，实际 %d body=%v", code, body)
	}
	if body["restart_required"] != false {
		t.Errorf("值未变化时 restart_required 应为 false，实际 %v", body["restart_required"])
	}
	if after := countLimitAudits(t, srv); after != before {
		t.Errorf("值未变化时不该新增审计（%d → %d）", before, after)
	}
}

// TestInstanceLimits_AdminCanClearAll 覆盖"把限制全部放开"这组取值：
// 0 / 空串都是合法输入（= 不限制），不能因为"看起来像没填"就被当成非法。
func TestInstanceLimits_AdminCanClearAll(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)
	seedLimits(t, srv, "inst1", 400, "4G", 20480)

	code, body := doJSON(t, ts, "PUT", "/api/instances/inst1/limits", adminTok, map[string]any{
		"cpu_quota": 0, "mem_limit": "", "disk_limit_mb": 0,
	})
	if code != http.StatusOK {
		t.Fatalf("放开全部限制应 200，实际 %d body=%v", code, body)
	}
	if cpu, mem, disk := limitsOf(t, srv, "inst1"); cpu != 0 || mem != "" || disk != 0 {
		t.Errorf("三项都应被清空，实际 %d/%q/%d", cpu, mem, disk)
	}
}

// TestInstanceLimits_UnknownInstance 实例不存在应 404（而不是 500）：
// 授权判定对总管理员是直接放行的，所以这里能走到存在性检查。
func TestInstanceLimits_UnknownInstance(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	code, body := doJSON(t, ts, "PUT", "/api/instances/nope/limits", adminTok,
		map[string]any{"cpu_quota": 100})
	if code != http.StatusNotFound {
		t.Errorf("不存在的实例应 404，实际 %d body=%v", code, body)
	}
}
