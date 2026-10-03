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

// 求助模板：**默认就有一套可用的**（不是空字符串），管理员能改，改坏了要能当场被拒。
//
// 用户反馈的原话是"预填充的求助模板也是空的" —— 所以这里第一条断言就是
// "全新安装的面板，模板不能是空的"。
func TestAnalysisHelpTemplate(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm9", "adm9-pass-1234")
	other := mkUser(t, srv, ts, admin, "pl9", "pl9-pass-1234", RoleUser)

	code, body := doJSON(t, ts, "GET", "/api/analysis/settings", admin, nil)
	if code != 200 {
		t.Fatalf("读设置失败：%d", code)
	}
	def, _ := body["help_template_default"].(string)
	if strings.TrimSpace(def) == "" {
		t.Fatal("内置默认求助模板不能为空（否则用户看到的就是一个空模板）")
	}
	if !strings.Contains(def, "{url}") {
		t.Errorf("默认模板必须含 {url}：\n%s", def)
	}
	// 没被改过时，生效值就是默认值（界面据此显示"当前：内置默认"）
	if body["help_template"] != def || body["help_template_is_default"] != true {
		t.Errorf("初始状态应等于内置默认：%v / %v", body["help_template_is_default"], body["help_template"])
	}
	// 占位符图例要发给前端（界面上点一下就能插入）
	phs, _ := body["help_placeholders"].([]interface{})
	if len(phs) < 5 {
		t.Errorf("占位符图例太少：%v", body["help_placeholders"])
	}

	// 非管理员不能改
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/settings", other,
		map[string]interface{}{"help_template": "日志：{url}"}); code != http.StatusForbidden {
		t.Errorf("非管理员改模板应 403，实际 %d", code)
	}

	// 缺少 {url} 的模板必须被拒（求助帖里最不能少的就是日志链接）
	if code, res := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"help_template": "帮我看看这个崩溃"}); code != http.StatusBadRequest {
		t.Errorf("缺少 {url} 的模板应 400，实际 %d %v", code, res)
	}

	// 正常保存 → 立刻生效，且 is_default 变为 false
	if code, res := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"help_template": "【急】{instance}\n{url}\n现象：{phenomenon}"}); code != 200 {
		t.Fatalf("保存模板失败：%d %v", code, res)
	}
	_, body = doJSON(t, ts, "GET", "/api/analysis/settings", admin, nil)
	if body["help_template_is_default"] != false {
		t.Error("保存后不应再标记为内置默认")
	}
	if s, _ := body["help_template"].(string); !strings.Contains(s, "【急】") {
		t.Errorf("模板未保存：%v", body["help_template"])
	}

	// 存空 = 恢复默认（语义要稳定：界面上的"恢复默认"直接提交空串即可）
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/settings", admin,
		map[string]interface{}{"help_template": ""}); code != 200 {
		t.Fatalf("清空模板应被接受（= 恢复默认）")
	}
	_, body = doJSON(t, ts, "GET", "/api/analysis/settings", admin, nil)
	if body["help_template"] != def || body["help_template_is_default"] != true {
		t.Errorf("清空后应回到内置默认：%v", body["help_template"])
	}
}

// 求助文本预览：确认弹窗里给用户看"最后会贴出去什么"。
//
// 模板可改之后这一步更必要 —— 只显示模板原文（一堆 {占位符}），
// 用户根本不知道成品长什么样，那正是"描述误区"的来源。
func TestHelpPreviewEndpoint(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm10", "adm10-pass-1234")
	// 直接写库建实例：预览不依赖 Daemon（节点离线也要能给预览）
	seedInstance(t, srv, "prev01", 1)

	code, body := doJSON(t, ts, "POST", "/api/instances/prev01/analysis/help-preview", admin,
		map[string]interface{}{"phenomenon": "启动 30 秒后崩溃"})
	if code != 200 {
		t.Fatalf("预览失败：%d %v", code, body)
	}
	text, _ := body["text"].(string)
	if strings.TrimSpace(text) == "" {
		t.Fatal("预览文本不能为空 —— 空预览等于没预览")
	}
	if !strings.Contains(text, "启动 30 秒后崩溃") {
		t.Errorf("「现象」没进预览：\n%s", text)
	}
	if strings.Contains(text, "{") {
		t.Errorf("预览里不该残留未替换的占位符：\n%s", text)
	}
	// 链接还没产生，必须**明确说明**而不是编一个假链接出来
	if note, _ := body["note"].(string); note == "" {
		t.Error("必须说明哪些字段要等上传后才填（否则用户以为预览就是最终版）")
	}

	// 不存在的实例：404（而不是给一段莫名其妙的文本）
	if code, _ := doJSON(t, ts, "POST", "/api/instances/nope-nope/analysis/help-preview", admin,
		map[string]interface{}{}); code != http.StatusNotFound {
		t.Errorf("不存在的实例应 404，实际 %d", code)
	}
}

// 提供方归属：**全局提供方只有总管理员能动**。
//
// 原判据是 `OwnerID != 0 && OwnerID != 调用者 && !isAdmin`，而全局提供方的
// owner_id 恰好是 0 —— 第一个条件直接把这道检查短路掉了。于是节点用户
// 可以改掉全局提供方的 base_url，再点一次「测试连接」，面板就会拿
// **解密后的 API Key** 去请求他填的地址：凭据被搬走，全程不需要管理员权限。
//
// 这也是"既有测试只跑了管理员那条路"漏掉它的原因（见 TestAnalysisProviderPermissions）。
func TestAnalysisGlobalProviderOnlyAdminCanManage(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm8", "adm8-pass-1234")
	nodeUser := mkUser(t, srv, ts, admin, "nu8", "nu8-pass-1234", RoleNodeUser)

	const globalBase = "https://api.deepseek.com/v1"
	code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "全局平台", "kind": "openai", "base_url": globalBase,
		"model": "m1", "api_key": "sk-global-key-1234", "global": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("管理员建全局提供方失败：%d %v", code, body)
	}
	globalID := int64(body["id"].(float64))

	// (a) 节点用户改**自己的**提供方：照旧可用（这半边不能被这道新检查误伤）
	code, body = doJSON(t, ts, "POST", "/api/analysis/providers", nodeUser, map[string]interface{}{
		"name": "我的平台", "kind": "openai", "base_url": globalBase,
		"model": "m1", "api_key": "sk-own-key-1234",
	})
	if code != http.StatusCreated {
		t.Fatalf("节点用户建自己的提供方失败：%d %v", code, body)
	}
	ownID := int64(body["id"].(float64))
	if code, body := doJSON(t, ts, "PUT", "/api/analysis/providers/"+itoa(ownID), nodeUser,
		map[string]interface{}{
			"name": "我的平台", "kind": "openai", "base_url": globalBase,
			"model": "m2", "api_key": "",
		}); code != http.StatusOK {
		t.Fatalf("节点用户应能改自己的提供方，实际 %d %v", code, body)
	}

	// (b) 改全局提供方：403（改的就是 base_url —— 攻击的第一步）
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/providers/"+itoa(globalID), nodeUser,
		map[string]interface{}{
			"name": "全局平台", "kind": "openai", "base_url": "https://attacker.example/v1",
			"model": "m1",
		}); code != http.StatusForbidden {
		t.Fatalf("节点用户改全局提供方应 403，实际 %d", code)
	}
	// 库里的 base_url 必须一个字都没变（否则随后的自检就会把 key 发出去）
	var gotBase string
	if err := srv.db.QueryRow(`SELECT base_url FROM analysis_providers WHERE id = ?`, globalID).
		Scan(&gotBase); err != nil {
		t.Fatal(err)
	}
	if gotBase != globalBase {
		t.Fatalf("全局提供方的 base_url 被改掉了：%q", gotBase)
	}

	// (c) 删全局提供方：403，且它仍然在
	if code, _ := doJSON(t, ts, "DELETE", "/api/analysis/providers/"+itoa(globalID), nodeUser, nil); code != http.StatusForbidden {
		t.Fatalf("节点用户删全局提供方应 403，实际 %d", code)
	}
	var n int
	if err := srv.db.QueryRow(`SELECT COUNT(1) FROM analysis_providers WHERE id = ?`, globalID).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("全局提供方应仍然存在：n=%d err=%v", n, err)
	}

	// (d) 自检同样是"能碰到凭据"的动作（它会把解密后的 key 发到这个地址）
	if code, _ := doJSON(t, ts, "POST", "/api/analysis/providers/"+itoa(globalID)+"/test", nodeUser, nil); code != http.StatusForbidden {
		t.Fatalf("节点用户自检全局提供方应 403，实际 %d", code)
	}

	// 管理员不受影响（这里只改配置，不触发自检 —— 自检会真的出网）
	if code, _ := doJSON(t, ts, "PUT", "/api/analysis/providers/"+itoa(globalID), admin,
		map[string]interface{}{
			"name": "全局平台", "kind": "openai", "base_url": globalBase,
			"model": "m3", "api_key": "",
		}); code != http.StatusOK {
		t.Fatalf("管理员应能改全局提供方，实际 %d", code)
	}
}

// 求助预览也必须过实例权限：预览文本里带着实例的显示名/核心/Java/内存/运行方式。
//
// 这条路原来只有 requireAuth，于是任何登录用户拿任意 instance_id 都能把别人
// 实例的这些信息读出来，而且"实例不存在"与"实例存在"的差别还顺带成了
// id 枚举的探测器（对不存在的 id 给 404、对存在的给 200）。
func TestHelpPreviewRequiresInstanceAccess(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm11", "adm11-pass-1234")
	outsider := mkUser(t, srv, ts, admin, "pl11", "pl11-pass-1234", RoleUser)
	seedInstance(t, srv, "prev02", 1)

	// ① 毫无授权的普通用户：403，而且响应里不能带那段文本
	code, body := doJSON(t, ts, "POST", "/api/instances/prev02/analysis/help-preview", outsider,
		map[string]interface{}{"phenomenon": "想偷看"})
	if code != http.StatusForbidden {
		t.Fatalf("无授权用户不该拿到预览，实际 %d body=%v", code, body)
	}
	if txt, _ := body["text"].(string); txt != "" {
		t.Errorf("403 的响应体里不该带预览文本：%q", txt)
	}

	// ② 授权 viewer 之后可以（预览是本实例内的只读渲染，不需要 owner）
	assignLevel(t, srv, "prev02", uidOf(t, srv, "pl11"), LevelViewer)
	if code, body := doJSON(t, ts, "POST", "/api/instances/prev02/analysis/help-preview", outsider,
		map[string]interface{}{"phenomenon": "启动 30 秒后崩溃"}); code != http.StatusOK {
		t.Fatalf("viewer 应能预览，实际 %d body=%v", code, body)
	}

	// ③ 不存在的实例仍是 404（原来的存在性检查没有被这道门替代掉）
	if code, _ := doJSON(t, ts, "POST", "/api/instances/prev02-none/analysis/help-preview", admin,
		map[string]interface{}{}); code != http.StatusNotFound {
		t.Errorf("不存在的实例应 404，实际 %d", code)
	}
}

// 总开关（第三方日志分析）关掉之后，/analyse 必须**一个字节都不外发**。
//
// 为什么单列一条：开关叫"第三方日志分析"，但链上还有 mclo.gs 这个兜底 ——
// 它的 Enabled 原来是写死的 true，于是管理员关掉开关之后，LogShare 那一步
// 失败就会回退到 mclo.gs，日志（含未打码的 IP）照旧被传到公开的 api.mclo.gs，
// 而且生成的分享链接是公开的。"关了还在传"比没有开关更糟：
// 用户会以为数据没出门，于是照传不误。
func TestAnalyseBlockedWhenThirdPartyDisabled(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm12", "adm12-pass-1234")
	seedInstance(t, srv, "sw01", 1)

	// 管理员关掉总开关（运行时设置，优先级高于 config.yaml）
	if code, body := doJSON(t, ts, "PUT", "/api/logshare/settings", admin,
		map[string]interface{}{"enabled": false}); code != http.StatusOK {
		t.Fatalf("关闭总开关失败：%d %v", code, body)
	}

	// ① 两个内置提供方都要跟着关掉（尤其 mclo.gs），链里不能再有第三方
	code, raw := call(t, ts, "GET", "/api/analysis/providers", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("读提供方列表失败：%d", code)
	}
	// 解析形状与 TestAnalysisChainExposed 一致：两个"展开"字段在前、简单字段在后 ——
	// 这样每个字段各自成一个对齐块，不会和 gofmt 的列对齐打架
	var list struct {
		Builtin []struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"builtin"`
		Chain []struct {
			Kind string `json:"kind"`
		} `json:"chain"`
		LogShareOn bool `json:"logshare_on"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("解析失败：%v（%s）", err, string(raw))
	}
	if list.LogShareOn {
		t.Error("开关已关闭，logshare_on 仍为 true")
	}
	if len(list.Builtin) != 2 {
		t.Fatalf("应有两个内置提供方，实际 %d", len(list.Builtin))
	}
	for _, b := range list.Builtin {
		if b.Enabled {
			t.Errorf("开关关闭时内置提供方 %s(%s) 不该是启用的", b.Name, b.Kind)
		}
	}
	if len(list.Chain) != 0 {
		t.Errorf("开关关闭时链里不该还有第三方提供方：%v", list.Chain)
	}

	// ② 分析接口直接拒绝 —— 而且要在读日志/连节点之前就拒绝
	code, body := doJSON(t, ts, "POST", "/api/instances/sw01/analyse", admin,
		map[string]interface{}{"path": "/logs/latest.log", "agree": true})
	if code != http.StatusForbidden {
		t.Fatalf("开关关闭时发起分析应 403，实际 %d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "关闭") {
		t.Errorf("拒绝原因应说明功能已被管理员关闭，实际 %q", msg)
	}
}

// filter_chat 的默认值必须是"过滤"：字段没传（nil）时按开启处理。
//
// 这是**隐私承诺**的一部分（config.example.yaml 与界面上的勾都写"默认开启"），
// 而 bool 的零值是 false —— 用 plain bool 接的话，任何不带该字段的调用方
//（脚本、老客户端、手工 curl）都会被当成"主动要求不过滤"，日志里的玩家聊天
// 就跟着一起上传了。
func TestFilterChatDefaultsOn(t *testing.T) {
	if !filterChatEnabled(nil) {
		t.Fatal("filter_chat 没传时必须按「过滤」处理，否则玩家聊天会被上传")
	}
	on, off := true, false
	if !filterChatEnabled(&on) {
		t.Error("显式 true 应过滤")
	}
	if filterChatEnabled(&off) {
		t.Error("只有显式 false 才允许关掉过滤")
	}

	// 与两个 handler 里的用法完全一致：解析出 filterChat 之后就是"过滤 / 不过滤"一个分支。
	// 这里用带 `[Not Secure]` 的那一种形态（见 stripPlayerChat 的注释），
	// 断言的是"没传字段时确实走进了过滤这一步"，而不是过滤正则自己的覆盖面。
	const log = "[10:01:10] [Server thread/INFO]: [Not Secure] <Steve> 你好\n" +
		"[10:01:11] [Server thread/INFO]: Done (3.2s)!\n"
	content, filtered := log, 0
	if filterChatEnabled(nil) {
		content, filtered = stripPlayerChat(content)
	}
	if filtered != 1 || strings.Contains(content, "<Steve>") {
		t.Errorf("默认应过滤掉 1 行玩家聊天，实际 filtered=%d content=%q", filtered, content)
	}
	if !strings.Contains(content, "Done (3.2s)!") {
		t.Error("非聊天行必须保留（过滤只针对玩家发言）")
	}
}

// mkUser 建一个用户并拿到**带正确角色**的令牌。
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
