package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 实例级日志分析开关（analysis_policy.go）。
//
// 这一组测试盯的是三件真机上踩过的事：
//   1. 两个内置平台必须能**分开**开关（早先一个 logshare_enabled 同时管两个，
//      结果管理员想"关公开分享、留 AI 分析"做不到，AI 看起来像坏了）；
//   2. 实例级三态必须真的影响"这一台实例能不能用某个平台"，
//      而不是只改界面文案；
//   3. 老部署（库里只有 logshare_enabled）升级上来的行为不变。

func TestBuiltinProvidersHaveSeparateSwitches(t *testing.T) {
	srv, _ := newTestServer(t)

	// 默认：两个都开
	for _, p := range srv.builtinProviders() {
		if !p.Enabled {
			t.Fatalf("默认应两个平台都启用，%s 却是关闭", p.Name)
		}
	}
	if !srv.LogShareAIEnabled() || !srv.MclogsEnabled() {
		t.Fatal("默认两个平台都应为启用")
	}

	// 只关 LogShare：mclo.gs 必须还在（这正是本次拆分要解决的问题）
	if err := srv.settingSet(settingLogShareAI, "false"); err != nil {
		t.Fatal(err)
	}
	if srv.LogShareAIEnabled() {
		t.Fatal("logshare_ai_enabled=false 之后 LogShare 应关闭")
	}
	if !srv.MclogsEnabled() {
		t.Fatal("只关 LogShare 不应连带关掉 mclo.gs")
	}
	if !srv.AnyThirdPartyEnabled() {
		t.Fatal("mclo.gs 还开着，总闸不应判定为全关")
	}

	// 两个都关：总闸才关
	if err := srv.settingSet(settingMclogsStore, "false"); err != nil {
		t.Fatal(err)
	}
	if srv.AnyThirdPartyEnabled() {
		t.Fatal("两个平台都关掉之后总闸应为关")
	}
}

func TestLegacySwitchStillControlsBoth(t *testing.T) {
	srv, _ := newTestServer(t)
	// 老部署：库里只有 logshare_enabled=false，没有两个新键
	if err := srv.settingSet(settingLogShareEnabled, "false"); err != nil {
		t.Fatal(err)
	}
	if srv.LogShareAIEnabled() || srv.MclogsEnabled() {
		t.Fatal("老键 logshare_enabled=false 时必须两个平台都关（升级前后行为一致）")
	}
	if err := srv.settingSet(settingLogShareEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	if !srv.LogShareAIEnabled() || !srv.MclogsEnabled() {
		t.Fatal("老键 logshare_enabled=true 时必须两个平台都开")
	}
	// 新键一旦写过，就以新键为准
	if err := srv.settingSet(settingMclogsStore, "false"); err != nil {
		t.Fatal(err)
	}
	if !srv.LogShareAIEnabled() || srv.MclogsEnabled() {
		t.Fatal("新键应覆盖老键：LogShare 开、mclo.gs 关")
	}
}

func TestInstancePolicyRoundTrip(t *testing.T) {
	srv, ts := newTestServer(t)
	token := loginAs(t, srv, ts, "poladmin", "pw")
	seedInstance(t, srv, "polinst1", 1)

	code, body := doJSON(t, ts, "GET", "/api/instances/polinst1/analysis-policy", token, nil)
	if code != http.StatusOK {
		t.Fatalf("读策略应 200，实际 %d %v", code, body)
	}
	if pol, _ := body["policy"].(map[string]any); len(pol) != 0 {
		t.Fatalf("新实例的策略应为空（全部跟随全局），实际 %v", pol)
	}

	code, body = doJSON(t, ts, "PUT", "/api/instances/polinst1/analysis-policy", token,
		map[string]any{"policy": map[string]string{platformMclogs: "off"}})
	if code != http.StatusOK {
		t.Fatalf("保存策略应 200，实际 %d %v", code, body)
	}
	if pol, _ := body["policy"].(map[string]any); pol[platformMclogs] != "off" {
		t.Fatalf("策略没保存上：%v", body["policy"])
	}

	// 落库之后再读一次（走的是另一条查询路径）
	got := srv.instanceAnalysisPolicy("polinst1")
	if got[platformMclogs] != "off" {
		t.Fatalf("重新读取策略失败：%v", got)
	}

	// 传空对象 = 清除例外
	code, _ = doJSON(t, ts, "PUT", "/api/instances/polinst1/analysis-policy", token,
		map[string]any{"policy": map[string]string{}})
	if code != http.StatusOK {
		t.Fatalf("清空策略应 200，实际 %d", code)
	}
	if got := srv.instanceAnalysisPolicy("polinst1"); len(got) != 0 {
		t.Fatalf("清空后不应再有例外：%v", got)
	}
}

func TestInstancePolicyOverridesGlobal(t *testing.T) {
	srv, _ := newTestServer(t)
	seedInstance(t, srv, "polinst2", 1)

	// 全局都关，但这台实例单独打开 LogShare
	if err := srv.settingSet(settingLogShareAI, "false"); err != nil {
		t.Fatal(err)
	}
	if err := srv.settingSet(settingMclogsStore, "false"); err != nil {
		t.Fatal(err)
	}
	if srv.instancePlatformAllowed("polinst2", platformLogShare) {
		t.Fatal("没有实例例外时应跟随全局（关）")
	}
	if err := srv.setInstanceAnalysisPolicy("polinst2", map[string]string{platformLogShare: "on"}); err != nil {
		t.Fatal(err)
	}
	if !srv.instancePlatformAllowed("polinst2", platformLogShare) {
		t.Fatal("实例显式 on 应覆盖全局关")
	}
	// 另一台实例不受影响
	seedInstance(t, srv, "polinst3", 1)
	if srv.instancePlatformAllowed("polinst3", platformLogShare) {
		t.Fatal("实例级开关不应影响别的实例")
	}

	// 反方向：全局开、这台实例关
	if err := srv.settingSet(settingLogShareAI, "true"); err != nil {
		t.Fatal(err)
	}
	if err := srv.setInstanceAnalysisPolicy("polinst2", map[string]string{platformLogShare: "off"}); err != nil {
		t.Fatal(err)
	}
	if srv.instancePlatformAllowed("polinst2", platformLogShare) {
		t.Fatal("实例显式 off 应覆盖全局开")
	}
	if !srv.instancePlatformAllowed("polinst3", platformLogShare) {
		t.Fatal("polinst3 没设例外，应跟随全局开")
	}
}

func TestFilterChainDropsInstanceDisabledPlatforms(t *testing.T) {
	srv, _ := newTestServer(t)
	seedInstance(t, srv, "polinst4", 1)
	if err := srv.setInstanceAnalysisPolicy("polinst4", map[string]string{platformMclogs: "off"}); err != nil {
		t.Fatal(err)
	}
	chain := srv.buildChainForTest()
	if len(chain) != 2 {
		t.Fatalf("测试前提：链上应有两个内置平台，实际 %d", len(chain))
	}
	got := srv.filterChainForInstance("polinst4", chain)
	if len(got) != 1 {
		t.Fatalf("关掉 mclo.gs 之后链上应只剩 1 个，实际 %d", len(got))
	}
	if got[0].Provider.Kind != analysis.KindLogShare {
		t.Fatalf("剩下的应是 logshare，实际 %s", got[0].Provider.Kind)
	}
	// 没有例外的实例不受影响
	if n := len(srv.filterChainForInstance("polinst4-none", chain)); n != 2 {
		t.Fatalf("没有策略的实例应保留整条链，实际 %d", n)
	}
}

// buildChainForTest 用内置提供方排一条链（测试专用，避免依赖界面顺序配置）。
func (s *Server) buildChainForTest() []analysis.ChainEntry {
	out := []analysis.ChainEntry{}
	for _, p := range s.builtinProviders() {
		if p.Enabled {
			out = append(out, analysis.ChainEntry{Provider: p})
		}
	}
	return out
}

// TestInstancePlatformStatusReflectsOverrides 界面拿到的三态是否与判定一致。
func TestInstancePlatformStatusReflectsOverrides(t *testing.T) {
	srv, _ := newTestServer(t)
	seedInstance(t, srv, "polinst5", 1)
	if err := srv.setInstanceAnalysisPolicy("polinst5", map[string]string{platformLogShare: "off"}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(srv.instancePlatformStatus("polinst5", 1))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]map[string]any{}
	for _, r := range rows {
		byKey[r["key"].(string)] = r
	}
	ls := byKey[platformLogShare]
	if ls == nil {
		t.Fatalf("状态里应含 logshare：%v", rows)
	}
	if ls["override"] != "off" || ls["effective"] != false {
		t.Fatalf("logshare 应被实例关掉：%v", ls)
	}
	mc := byKey[platformMclogs]
	if mc == nil || mc["override"] != "" || mc["effective"] != true {
		t.Fatalf("mclo.gs 应跟随全局（开）：%v", mc)
	}
}
