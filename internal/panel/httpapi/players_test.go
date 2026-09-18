package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// checkPlayerName：名字会被拼进控制台命令，**换行是注入**，必须拦死。
//
// 这条测试直接对着内测报告里的 POC 写：
//
//	{"type":"ops","name":"probeuser\nsay ATL_INJECTION_MARKER_7741"}
//
// 修复前，服务端会把它拼成 `op probeuser\nsay ...` 送进控制台，
// 而 MC 控制台按行执行 → 第二条命令真的被运行了。
func TestCheckPlayerName(t *testing.T) {
	ok := []string{"Steve", "Xx_ProGamer_xX", ".Steve", "*Steve", "a.b_c", "1.2.3.4", "2001:db8::1"}
	for _, s := range ok {
		if msg := checkPlayerName("whitelist", s); msg != "" {
			t.Errorf("%q 应通过，实际被拒：%s", s, msg)
		}
	}

	// 注入（换行 / 回车 / 制表 / NUL / DEL）一律拒绝
	bad := []string{
		"probeuser\nsay ATL_INJECTION_MARKER_7741",
		"x\nop y",
		"x\rsay z",
		"x\ty",
		"x\x00y",
		"x\x7fy",
		"",
	}
	for _, s := range bad {
		if msg := checkPlayerName("whitelist", s); msg == "" {
			t.Errorf("%q 应被拒（这是命令注入的入口）", s)
		}
	}

	// 基岩版玩家名可以带空格（经 Floodgate 接入）：校验层放行，
	// 由 consoleSafeName 决定"走不走控制台那条路"
	if msg := checkPlayerName("whitelist", ".Big Steve"); msg != "" {
		t.Errorf("带空格的基岩版玩家名应通过校验（注入只与换行有关），实际被拒：%s", msg)
	}

	// 超长
	if msg := checkPlayerName("whitelist", strings.Repeat("x", mcNameMaxLen+1)); msg == "" {
		t.Error("超长名字应被拒")
	}
	// ipbans 走 IP 规则：冒号合法、字母 g 不合法
	if msg := checkPlayerName("ipbans", "2001:db8::1"); msg != "" {
		t.Errorf("IPv6 应通过 ipbans 校验，实际被拒：%s", msg)
	}
	if msg := checkPlayerName("ipbans", "not-an-ip"); msg == "" {
		t.Error("非 IP 内容应被 ipbans 校验拒绝")
	}
}

// consoleSafeName：决定名字能不能拼进命令（不是安全边界）。
func TestConsoleSafeName(t *testing.T) {
	for _, s := range []string{"Steve", ".Steve", "*Steve", "a_b.c", "1.2.3.4"} {
		if !consoleSafeName(s) {
			t.Errorf("%q 应可拼进命令", s)
		}
	}
	for _, s := range []string{".Big Steve", "a-b", "玩家一", strings.Repeat("x", 33)} {
		if consoleSafeName(s) {
			t.Errorf("%q 不应被视为可拼进命令（空格/连字符/非 ASCII/超长）", s)
		}
	}
}

// 踢出走的是控制台路径：没有名单文件，所以不能被当成"未知名单类型"拒掉。
//
// 这里只断言**路由认得 kick**：真正的踢出效果依赖一个在跑的 Daemon，
// 单元测试里没有（会以 500 结束，但错误信息不能是"未知名单类型"）。
func TestKickIsKnownKind(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst-kick", 1)

	code, body := doJSON(t, ts, "POST", "/api/instances/inst-kick/players", adminTok,
		map[string]string{"type": "kick", "name": "Steve"})
	msg, _ := body["error"].(string)
	if code == http.StatusBadRequest && strings.Contains(msg, "未知名单类型") {
		t.Fatalf("kick 应被识别为已知动作，实际 400：%s", msg)
	}
	// 名字非法仍然要挡（在走控制台之前就拒绝）
	if code, body := doJSON(t, ts, "POST", "/api/instances/inst-kick/players", adminTok,
		map[string]string{"type": "kick", "name": "a\nop b"}); code != http.StatusBadRequest {
		t.Errorf("带换行的名字应 400，实际 %d body=%v", code, body)
	}

	// 报告里的原始 POC 形态（op + 换行注入）也必须被挡在**校验层**，
	// 而不是等到拼出命令之后
	if code, body := doJSON(t, ts, "POST", "/api/instances/inst-kick/players", adminTok,
		map[string]string{"type": "ops", "name": "probeuser\nsay ATL_INJECTION_MARKER_7741"}); code != http.StatusBadRequest {
		t.Errorf("注入载荷应 400，实际 %d body=%v", code, body)
	}
	if code, _ := doJSON(t, ts, "DELETE",
		"/api/instances/inst-kick/players?type=whitelist&name=x%0Asay%20INJECT", adminTok, nil); code != http.StatusBadRequest {
		t.Errorf("删除接口的注入载荷应 400，实际 %d", code)
	}
}

// OP 等级：1~4 之外一律拒绝，且应当在联系 Daemon 之前就拒绝。
func TestSetOpLevelValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst-op", 1)

	for _, lv := range []int{0, -1, 5, 99} {
		code, body := doJSON(t, ts, "POST", "/api/instances/inst-op/players/op-level", adminTok,
			map[string]any{"name": "Steve", "level": lv})
		if code != http.StatusBadRequest {
			t.Errorf("等级 %d 应 400，实际 %d body=%v", lv, code, body)
		}
	}
	code, body := doJSON(t, ts, "POST", "/api/instances/inst-op/players/op-level", adminTok,
		map[string]any{"name": "a\nop b", "level": 2})
	if code != http.StatusBadRequest {
		t.Errorf("名字带换行应 400，实际 %d body=%v", code, body)
	}

	// 有权限但节点的 Daemon 不可达：应当是"上游不可用"，而不是被当成参数错误。
	// （seedInstance 只写了库记录，没有真的 Daemon，所以这里必然连不上。）
	code, body = doJSON(t, ts, "POST", "/api/instances/inst-op/players/op-level", adminTok,
		map[string]any{"name": "Steve", "level": 2})
	if code == http.StatusBadRequest {
		t.Errorf("合法请求不该被判成参数错误，实际 %d body=%v", code, body)
	}
}

// setOpLevelJSON：不在名单里要一并加入，等级相同不改文件。
//
// 为什么"不在名单里就加入"：用户点的是"把这人设成 3 级"，
// 这个意图本身就包含"他要成为管理员"。分成"先加再改等级"两步只是多让人点一次。
func TestSetOpLevelJSON(t *testing.T) {
	// 已有条目：只改等级
	in := `[{"uuid":"11111111-1111-1111-1111-111111111111","name":"Steve","level":4,"bypassesPlayerLimit":false}]`
	out, changed, existed, err := setOpLevelJSON(in, "Steve", 2)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !changed || !existed {
		t.Fatalf("应识别为已存在且发生变化，实际 changed=%v existed=%v", changed, existed)
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("条目数不应变，实际 %d", len(parsed))
	}
	if lv, _ := parsed[0]["level"].(float64); int(lv) != 2 {
		t.Errorf("等级应为 2，实际 %v", parsed[0]["level"])
	}
	if parsed[0]["bypassesPlayerLimit"] != false {
		t.Errorf("不应丢掉原有字段，实际条目=%v", parsed[0])
	}

	// 名字大小写不敏感（游戏名不区分大小写，名单里可能存的是另一种写法）
	if _, changed, existed, _ := setOpLevelJSON(in, "steve", 3); !changed || !existed {
		t.Errorf("大小写不同的同名玩家应被识别为已存在，实际 changed=%v existed=%v", changed, existed)
	}

	// 等级没变：不改文件（避免无意义地重写名单、触发服务器的文件监视）
	if out2, changed, existed, _ := setOpLevelJSON(in, "Steve", 4); changed || !existed || out2 != in {
		t.Errorf("等级相同时不应改动：changed=%v existed=%v same=%v", changed, existed, out2 == in)
	}

	// 不在名单里：加入并带上等级
	out3, changed, existed, err := setOpLevelJSON(`[]`, "Alex", 3)
	if err != nil || !changed || existed {
		t.Fatalf("新增应 changed=true existed=false，实际 changed=%v existed=%v err=%v", changed, existed, err)
	}
	if err := json.Unmarshal([]byte(out3), &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("应新增 1 条，实际 %d", len(parsed))
	}
	if lv, _ := parsed[0]["level"].(float64); int(lv) != 3 {
		t.Errorf("新增条目等级应为 3，实际 %v", parsed[0]["level"])
	}
	if u, _ := parsed[0]["uuid"].(string); u == "" {
		t.Errorf("新增条目应带 UUID（离线模式推导），实际 %q", u)
	}
	if n, _ := parsed[0]["name"].(string); n != "Alex" {
		t.Errorf("新增条目名字应为 Alex，实际 %q", n)
	}

	// 空文件也不报错
	if _, changed, existed, err := setOpLevelJSON("", "Alex", 1); err != nil || !changed || existed {
		t.Errorf("空名单应能新增：changed=%v existed=%v err=%v", changed, existed, err)
	}

	// 关键回归点：bypassesPlayerLimit=true 必须活下来。
	// 用 playerEntry（只认 uuid/name/level）往返会把它抹成 false，
	// 表现就是"只改了个 OP 等级，那人却进不去满员服了"。
	inBypass := `[{"uuid":"22222222-2222-2222-2222-222222222222","name":"Bob","level":4,"bypassesPlayerLimit":true}]`
	outBypass, changed, _, err := setOpLevelJSON(inBypass, "Bob", 2)
	if err != nil || !changed {
		t.Fatalf("改等级应成功：changed=%v err=%v", changed, err)
	}
	var bp []map[string]any
	if err := json.Unmarshal([]byte(outBypass), &bp); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if bp[0]["bypassesPlayerLimit"] != true {
		t.Errorf("bypassesPlayerLimit 应保持 true，实际 %v（条目=%v）", bp[0]["bypassesPlayerLimit"], bp[0])
	}
	if lv, _ := bp[0]["level"].(float64); int(lv) != 2 {
		t.Errorf("等级应为 2，实际 %v", bp[0]["level"])
	}

	// 文件损坏时**拒绝**，绝不能当成空名单把现有内容覆盖掉
	if _, _, _, err := setOpLevelJSON(`[{"name":"Steve",]`, "Steve", 2); err == nil {
		t.Error("非法 JSON 应报错（否则会覆盖现有名单）")
	}
	if _, _, err := updatePlayerJSON("ops", `not json at all`, "Steve", true); err == nil {
		t.Error("updatePlayerJSON 遇到非法 JSON 也应报错")
	}
}
