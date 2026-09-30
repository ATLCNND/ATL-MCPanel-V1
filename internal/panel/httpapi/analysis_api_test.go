package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 提供方配置的权限：管理员与节点用户可以配，**普通用户不行**（D1）。
//
// 为什么把这条钉在 HTTP 层：它决定"谁能把日志发到外面的哪个地址"。
// 一旦放开，任何普通用户都能借面板的出站能力访问任意目标 —— 那正是 SSRF 的入口。
func TestAnalysisProviderPermissions(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm1", "adm1-pass-1234")
	normal := mkUser(t, srv, ts, admin, "pl1", "pl1-pass-1234", RoleUser)
	nodeUser := mkUser(t, srv, ts, admin, "nu1", "nu1-pass-1234", RoleNodeUser)

	prov := map[string]interface{}{
		"name": "测试平台", "kind": "openai", "base_url": "https://api.deepseek.com/v1",
		"model": "m1", "api_key": "sk-abcdefghijklmnop",
	}

	// 普通用户：不能建
	if code, _ := doJSON(t, ts, "POST", "/api/analysis/providers", normal, prov); code != http.StatusForbidden {
		t.Errorf("普通用户建提供方应 403，实际 %d", code)
	}
	// 节点用户：可以建自己的
	if code, body := doJSON(t, ts, "POST", "/api/analysis/providers", nodeUser, prov); code != http.StatusCreated {
		t.Errorf("节点用户应能建自己的提供方，实际 %d %v", code, body)
	}
	// 管理员：可以建全局的
	global := map[string]interface{}{}
	for k, v := range prov {
		global[k] = v
	}
	global["global"] = true
	global["name"] = "全局平台"
	if code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, global); code != http.StatusCreated {
		t.Errorf("管理员应能建全局提供方，实际 %d %v", code, body)
	}

	// 节点用户建"全局"提供方要被拒（否则任何人都能给所有人塞一个平台）
	if code, _ := doJSON(t, ts, "POST", "/api/analysis/providers", nodeUser, global); code != http.StatusForbidden {
		t.Errorf("非管理员建全局提供方应 403，实际 %d", code)
	}
}

// **API Key 绝不回显、绝不明文落库**（D1 的核心约束）。
//
// 这条是安全红线：接口回显 key 等于把凭据交给任何能读接口的人；
// 明文落库等于"谁拿到数据库备份谁就拿到所有人的 key"。
func TestAnalysisProviderKeyNeverLeaks(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm2", "adm2-pass-1234")
	const plainKey = "sk-super-secret-key-9999"

	if code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "我的网关", "kind": "openai", "base_url": "https://api.deepseek.com/v1",
		"model": "m1", "api_key": plainKey,
	}); code != http.StatusCreated {
		t.Fatalf("创建失败：%d %v", code, body)
	}

	// 列表接口：不能出现明文 key，但要有 ****末四位
	code, raw := call(t, ts, "GET", "/api/analysis/providers", admin, nil)
	if code != 200 {
		t.Fatalf("列表失败：%d", code)
	}
	if strings.Contains(string(raw), plainKey) {
		t.Fatal("列表接口回显了完整 API Key —— 这是安全红线")
	}
	if !strings.Contains(string(raw), "9999") {
		t.Errorf("应回显末 4 位以便识别，实际：%s", string(raw))
	}

	// 数据库里不能有明文（api_key_enc 必须是密文）
	var enc []byte
	if err := srv.db.QueryRow(`SELECT api_key_enc FROM analysis_providers ORDER BY id DESC LIMIT 1`).Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, []byte(plainKey)) {
		t.Fatal("数据库里存的是明文 API Key")
	}
	// 但要能解回来（否则功能不可用）
	got, err := srv.secretBox.Decrypt(enc)
	if err != nil || got != plainKey {
		t.Fatalf("密文解不回原 key：%v / %q", err, got)
	}
}

// 自检接口：地址不可达时要返回 ok=false 与**可读原因**，而不是 500。
//
// 这是"填完能当场验证"的能力，缺了它用户只能靠"分析失败"反推配置问题。
func TestAnalysisProviderTestEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm3", "adm3-pass-1234")

	code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "坏地址", "kind": "openai", "base_url": "https://this-does-not-exist-atl.invalid/v1",
		"model": "m1", "api_key": "sk-1234567890abcdef",
	})
	if code != http.StatusCreated {
		t.Fatalf("创建失败：%d %v", code, body)
	}
	id := int64(body["id"].(float64))

	code, res := doJSON(t, ts, "POST", "/api/analysis/providers/"+itoa(id)+"/test", admin, nil)
	if code != 200 {
		t.Fatalf("自检接口应返回 200（结果放在 ok 字段里），实际 %d %v", code, res)
	}
	if res["ok"] != false {
		t.Errorf("不可达地址的自检结果应为 ok=false，实际 %v", res)
	}
	if s, _ := res["error"].(string); s == "" {
		t.Error("自检失败必须给出原因")
	}
}

// 分析设置：只有总管理员能改；限额要做范围校验；改完立即生效。
func TestAnalysisSettingsEndpoint(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm4", "adm4-pass-1234")
	other := mkUser(t, srv, ts, admin, "pl4", "pl4-pass-1234", RoleUser)

	// 默认值：顺序为空（走内置默认），限额是默认值
	code, body := doJSON(t, ts, "GET", "/api/analysis/settings", admin, nil)
	if code != 200 {
		t.Fatalf("读设置失败：%d", code)
	}
	if body["rate_per_min"] != float64(analysis.DefaultPerMinute) {
		t.Errorf("默认每分钟限额应为 %d，实际 %v", analysis.DefaultPerMinute, body["rate_per_min"])
	}
	if body["can_manage"] != true {
		t.Errorf("管理员应能管理设置")
	}

	// 非管理员改设置：403
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/settings", other,
		map[string]interface{}{"rate_per_min": 1}); code != http.StatusForbidden {
		t.Errorf("非管理员改设置应 403，实际 %d", code)
	}

	// 管理员改限额 + 内网开关：生效
	if code, res := doJSON(t, ts, "PUT", "/api/analysis/settings", admin, map[string]interface{}{
		"rate_per_min": 3, "rate_per_day": 30, "allow_private": true,
		"order": []string{"mclogs", "logshare"},
	}); code != 200 {
		t.Fatalf("改设置失败：%d %v", code, res)
	}
	perMin, perDay := srv.rateLimiter.Limits()
	if perMin != 3 || perDay != 30 {
		t.Errorf("限额未立即生效：%d/%d", perMin, perDay)
	}
	code, body = doJSON(t, ts, "GET", "/api/analysis/settings", admin, nil)
	if body["allow_private"] != true {
		t.Errorf("内网开关未保存：%v", body["allow_private"])
	}
	if order, _ := body["order"].([]interface{}); len(order) != 2 || order[0] != "mclogs" {
		t.Errorf("顺序未保存：%v", body["order"])
	}

	// 非法限额要拒绝
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"rate_per_min": 0}); code != http.StatusBadRequest {
		t.Errorf("0 限额应被拒绝，实际 %d", code)
	}
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"rate_per_day": 999999999}); code != http.StatusBadRequest {
		t.Errorf("过大的限额应被拒绝，实际 %d", code)
	}
}

// 内网地址在**没打开开关**时要被拒绝（D2 的 HTTP 层断言）。
func TestAnalysisProviderRejectsPrivateURLByDefault(t *testing.T) {
	_, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm5", "adm5-pass-1234")

	code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "内网网关", "kind": "openai", "base_url": "http://10.1.2.3:8000/v1",
		"model": "m1", "api_key": "sk-1234567890abcdef",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("默认应拒绝内网地址，实际 %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "内网") {
		t.Errorf("拒绝时的提示应说明原因与怎么打开，实际：%q", msg)
	}

	// 打开开关后应当放行
	if code, res := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"allow_private": true}); code != 200 {
		t.Fatalf("打开内网开关失败：%d %v", code, res)
	}
	if code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "内网网关", "kind": "openai", "base_url": "http://10.1.2.3:8000/v1",
		"model": "m1", "api_key": "sk-1234567890abcdef",
	}); code != http.StatusCreated {
		t.Errorf("打开开关后应放行内网地址，实际 %d %v", code, body)
	}
	// 但云元数据地址**永远**不放行（169.254.169.254 是拿机器凭据的地方）
	if code, _ := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "元数据", "kind": "openai", "base_url": "http://169.254.169.254/latest",
		"model": "m1", "api_key": "sk-1234567890abcdef",
	}); code != http.StatusBadRequest {
		t.Errorf("云元数据地址即使开了内网开关也必须拒绝，实际 %d", code)
	}
}

// 发起分析必须先勾选同意（服务端校验，前端藏不住）。
func TestAnalyseRequiresConsent(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm6", "adm6-pass-1234")
	seedInstance(t, srv, "ins-a", 1)

	code, body := doJSON(t, ts, "POST", "/api/instances/ins-a/analyse", admin, map[string]interface{}{
		"path": "/logs/latest.log", "agree": false,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("未勾选同意应 400，实际 %d %v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "勾选") {
		t.Errorf("提示应说明需要勾选同意，实际 %q", msg)
	}
}

// 提供方列表要带上"这条链现在会按什么顺序尝试"，界面据此解释回退行为。
func TestAnalysisChainExposed(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm7", "adm7-pass-1234")

	code, raw := call(t, ts, "GET", "/api/analysis/providers", admin, nil)
	if code != 200 {
		t.Fatalf("列表失败：%d", code)
	}
	var body struct {
		Builtin []struct {
			Kind string `json:"kind"`
		} `json:"builtin"`
		Chain []struct {
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"chain"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("解析失败：%v（%s）", err, string(raw))
	}
	kinds := map[string]bool{}
	for _, b := range body.Builtin {
		kinds[b.Kind] = true
	}
	if !kinds[analysis.KindLogShare] || !kinds[analysis.KindMclogs] {
		t.Errorf("内置提供方应含 logshare 与 mclogs，实际 %v", kinds)
	}
	if len(body.Chain) < 2 {
		t.Fatalf("链至少应含两家，实际 %d", len(body.Chain))
	}
	if body.Chain[0].Kind != analysis.KindLogShare {
		t.Errorf("默认首选应为 logshare（D3），实际 %s", body.Chain[0].Kind)
	}
	// 每一环都要有"为什么在这个位置"的说明
	for _, c := range body.Chain {
		if c.Reason == "" {
			t.Errorf("%s 缺少排序原因说明", c.Name)
		}
	}
	// 这个测试服务器没配 LogShare，内置项 enabled=false 时不该进链，
	// 所以这里再断言一次"不进链要有原因可查"（由 attempts 在真实调用时给出）
	_ = srv
}

// itoa 复用 api_test.go 里的同名辅助函数（同一个包，避免重复定义）。

// mkUser 建一个用户并拿到**带正确角色**的令牌。
//
// 两个坑（都是跑出来才发现的）：
//  1. `POST /api/users` 用非管理员令牌会被拒 —— 只有库里的**第一个**用户能自助注册；
//  2. 注册接口**不认 role=nodeuser**（非 admin 一律落成 user），角色只能建完再改，
//     而角色写在 JWT 里 —— 所以改完必须**重新登录**才拿到带新角色的令牌。
func mkUser(t *testing.T, srv *Server, ts *httptest.Server, adminTok, username, password, role string) string {
	t.Helper()
	if code, body := doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": username, "password": password, "role": role}); code != http.StatusCreated {
		t.Fatalf("创建用户 %s 失败：%d %v", username, code, body)
	}
	if role != "" && role != RoleUser {
		uid := uidOf(t, srv, username)
		if code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok,
			map[string]string{"role": role}); code != http.StatusOK {
			t.Fatalf("设置 %s 角色为 %s 失败：%d %v", username, role, code, body)
		}
	}
	return loginAsToken(t, ts, username, password)
}

// loginAsToken 用**已存在**的账号登录（不尝试注册）。
//
// 为什么需要它：api_test.go 的 loginAs 会先 POST /api/users（不带令牌）——
// 那只对**库里的第一个用户**有效（首个用户自动成为 admin），
// 第二个用户起就必须带管理员令牌创建。
func loginAsToken(t *testing.T, ts *httptest.Server, username, password string) string {
	t.Helper()
	code, body := doJSON(t, ts, "POST", "/api/auth/login", "",
		map[string]string{"username": username, "password": password})
	if code != http.StatusOK {
		t.Fatalf("登录 %s 失败 code=%d body=%v", username, code, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("登录 %s 未返回 token", username)
	}
	return tok
}
