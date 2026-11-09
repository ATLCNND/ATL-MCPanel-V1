package httpapi

import (
	"net/http"
	"testing"
)

// 回归：实例列表必须把 template 与 spec **读对**。
//
// 这组用例是拿真机测出来的 bug 补的：给列表 SQL 加列时，`i.template, i.spec`
// 插在 `i.status` 之后，而 rows.Scan 里却插在 status 之前，于是
//   status → Template（Normalize 成 minecraft）
//   template → Spec
//   spec → Status（随后又被 reconcileInstanceStatus 覆盖回真实的 running/stopped）
// 三个字段一起串位，而面板上表现出来只是"所有实例都变成 Minecraft 类型"，
// 极难从日志看出根因。列顺序与 Scan 顺序必须逐个对应，所以这里直接钉住
// "读回来的值等于写进去的值"。
func TestListInstancesReadsTemplateAndSpec(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, srv, ts, "admkind", "admkind-pass-1234")
	seedInstance(t, srv, "kindmc01", 1)
	seedInstance(t, srv, "kinddk01", 1)
	seedInstance(t, srv, "kindcs01", 1)

	const dockerSpec = `{"image":"nginx:alpine","ports":[{"host":18080,"container":80}]}`
	if _, err := srv.db.Exec(
		`UPDATE instances SET template = 'docker', spec = ? WHERE instance_id = 'kinddk01'`,
		dockerSpec); err != nil {
		t.Fatalf("写入 docker 规格失败：%v", err)
	}
	if _, err := srv.db.Exec(
		`UPDATE instances SET template = 'console' WHERE instance_id = 'kindcs01'`); err != nil {
		t.Fatalf("写入 console 类型失败：%v", err)
	}

	code, list := doJSONArr(t, ts, "GET", "/api/instances", admin, nil)
	if code != http.StatusOK || len(list) != 3 {
		t.Fatalf("拉实例列表失败：%d，%d 条", code, len(list))
	}
	byID := map[string]map[string]interface{}{}
	for _, item := range list {
		id, _ := item["instance_id"].(string)
		byID[id] = item
	}

	mc, ok := byID["kindmc01"]
	if !ok {
		t.Fatal("列表里没有 kindmc01")
	}
	if mc["template"] != "minecraft" {
		t.Errorf("minecraft 实例的 template = %v，应为 minecraft", mc["template"])
	}
	if !hasCap(mc, "rcon") || !hasCap(mc, "players") {
		t.Errorf("minecraft 实例应带 rcon/players 能力：%v", mc["capabilities"])
	}

	dk, ok := byID["kinddk01"]
	if !ok {
		t.Fatal("列表里没有 kinddk01")
	}
	if dk["template"] != "docker" {
		t.Errorf("docker 实例的 template = %v，应为 docker（列顺序与 Scan 顺序可能又串位了）", dk["template"])
	}
	if dk["spec"] != dockerSpec {
		t.Errorf("docker 实例的 spec = %v，应为写入的那份 JSON", dk["spec"])
	}
	for _, c := range []string{"image", "ports", "volumes", "env", "console", "files", "backup", "logs"} {
		if !hasCap(dk, c) {
			t.Errorf("docker 实例缺少能力 %q：%v", c, dk["capabilities"])
		}
	}
	if hasCap(dk, "rcon") || hasCap(dk, "players") {
		t.Errorf("docker 实例不该有 MC 理解层能力：%v", dk["capabilities"])
	}

	cs, ok := byID["kindcs01"]
	if !ok {
		t.Fatal("列表里没有 kindcs01")
	}
	if cs["template"] != "console" {
		t.Errorf("console 实例的 template = %v，应为 console", cs["template"])
	}
	if !hasCap(cs, "script") || hasCap(cs, "image") {
		t.Errorf("console 实例应有 script、无 image：%v", cs["capabilities"])
	}
	if _, ok := cs["spec"]; ok {
		t.Errorf("console 实例没有启动规格，不该输出 spec 字段：%v", cs["spec"])
	}

	// status 必须还是它自己的值：串位时它会短暂变成 spec 的 JSON
	if dk["status"] == dockerSpec {
		t.Errorf("status 被 spec 覆盖了 —— 列顺序与 Scan 顺序串位")
	}
}

func hasCap(item map[string]interface{}, want string) bool {
	raw, ok := item["capabilities"].([]interface{})
	if !ok {
		return false
	}
	for _, v := range raw {
		if s, ok := v.(string); ok && s == want {
			return true
		}
	}
	return false
}
