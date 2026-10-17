package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// 本文件守的是 2026-10-17 在真机上暴露的一个**静默变空白**的缺陷。
//
// 现象（节点 0.9.19、浏览器实测）：进「分析平台」页，「内置提供方」两张卡片
// **都没有名字**、**都挂着"已停用"**、类型胶囊是空的，而且两条说明文字都是
// mclo.gs 那一句；同一页「我配置的平台」那一栏同理全空。可它对应的接口
// `GET /api/analysis/providers` 返回的数据是**完全正常**的
// （`Enabled: true`、`Name: "LogShare"`、`logshare_on: true`）。
//
// 根因：该接口的 `builtin` / `custom` 两个数组是**直接序列化 analysis.Provider**，
// 而那个结构体当时没有 json tag —— 线上字段名因此是 Go 的字段名
//（`Name` / `Kind` / `Enabled` / `BaseURL`），前端（web/src/api.ts 的
// AnalysisProvider）读的却是 `name` / `kind` / `enabled` / `base_url`。
// 于是前端读到的**每一个字段都是 undefined**：
//   · `p.name` undefined → 名字位置空着；
//   · `!p.enabled` → 恒真 → 两张卡都显示"已停用"；
//   · `p.kind === 'logshare'` 不成立 → 说明文字全部走了 else（mclo.gs）那一支；
//   · `KIND_LABEL[p.kind]` → undefined → 类型胶囊空白。
//
// 为什么原来那两条测试（TestAnalysisChainExposed、TestAnalyseBlockedWhenThirdPartyDisabled）
// 拦不住：Go 的 encoding/json **反序列化是大小写不敏感的**，测试里声明成
// `json:"kind"` 的字段照样能被响应里的 `"Kind"` 填上 —— 服务端测试全绿，
// 浏览器里全是 undefined。**这不是"测试写少了"，是"测试用的匹配规则比浏览器宽"**。
//
// 所以这里的判据刻意换成两条浏览器同款的严格规则：
//   ① 键名集合**逐字钉死**（多一个少一个都算失败）；
//   ② 任何键名都不许以大写字母开头（PascalCase 字段名不许出现在响应里）。
//
// 反向验证（2026-10-17）：把 analysis/provider.go 里的 json tag 全删掉之后再跑本用例，
// 实测失败，报的就是「builtin[0]/builtin[1]/custom[0] 的键名集合不对」三条；
// 还原后 sha256 与备份一致、复跑通过。tag 数量 14 → 0 → 14。

// wireKeys 取一个 JSON 对象的所有键（已排序，便于逐字比较）。
func wireKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("这不是一个 JSON 对象：%v（%s）", err, string(raw))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestAnalysisProvidersWireKeys(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, srv, ts, "adm13", "adm13-pass-1234")

	// 造一条自配平台，让 custom 数组非空（它的元素是同一个结构体，同一个坑）
	if code, body := doJSON(t, ts, "POST", "/api/analysis/providers", admin, map[string]interface{}{
		"name": "自配平台A", "kind": "openai",
		"base_url": "https://api.example.com/v1", "model": "m1",
		"api_key": "sk-test-wire-1234", "timeout_sec": 300,
	}); code != http.StatusCreated {
		t.Fatalf("创建自配平台失败：%d %v", code, body)
	}

	code, raw := call(t, ts, "GET", "/api/analysis/providers", admin, nil)
	if code != http.StatusOK {
		t.Fatalf("读提供方列表失败：%d", code)
	}

	// 用 RawMessage 接住每个元素，才能看**线上真实的键名** ——
	// 这正是原测试做不到的地方：一旦声明成带 tag 的结构体，
	// 大小写不敏感的反序列化就把问题抹平了。
	var resp struct {
		Builtin []json.RawMessage `json:"builtin"`
		Custom  []json.RawMessage `json:"custom"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析失败：%v（%s）", err, string(raw))
	}
	if len(resp.Builtin) != 2 {
		t.Fatalf("内置提供方应为 2 个，实际 %d", len(resp.Builtin))
	}
	if len(resp.Custom) != 1 {
		t.Fatalf("自配平台应为 1 个，实际 %d", len(resp.Custom))
	}

	// 前端 AnalysisProvider 需要的字段集合（api.ts:1114）。
	// api_key **不在**其中：它在任何响应里都不该出现（Provider 上是 json:"-"）。
	want := []string{
		"base_url", "enabled", "id", "key_hint", "kind",
		"max_bytes", "model", "name", "owner_id", "prompt", "timeout_sec",
	}

	for _, tc := range []struct {
		what string
		raw  json.RawMessage
	}{
		{"builtin[0]", resp.Builtin[0]},
		{"builtin[1]", resp.Builtin[1]},
		{"custom[0]", resp.Custom[0]},
	} {
		got := wireKeys(t, tc.raw)
		if len(got) != len(want) {
			t.Errorf("%s 的键名集合不对（前端读 snake_case，字段名对不上就是 undefined）\n  实际: %v\n  应有: %v\n  原文: %s",
				tc.what, got, want, string(tc.raw))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s 的键名集合不对\n  实际: %v\n  应有: %v\n  原文: %s",
					tc.what, got, want, string(tc.raw))
				break
			}
		}
		// 机械规则：响应里不许出现 PascalCase 键（那就是"裸序列化 Go 结构体"的味道）
		for _, k := range got {
			if k != "" && k[0] >= 'A' && k[0] <= 'Z' {
				t.Errorf("%s 里出现了大写开头的键 %q —— 前端读不到它（原文 %s）", tc.what, k, string(tc.raw))
			}
		}
	}

	// 光有键名还不够：键名对了但值是空的，界面照样是空白。
	// 这里按前端的读法（snake_case）取一次值。
	var vals struct {
		Builtin []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Enabled bool   `json:"enabled"`
		} `json:"builtin"`
	}
	if err := json.Unmarshal(raw, &vals); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	seen := map[string]bool{}
	for _, b := range vals.Builtin {
		if b.Name == "" {
			t.Errorf("内置提供方没有名字（界面会显示成空白卡片）")
		}
		if b.Kind == "" {
			t.Errorf("%s 没有 kind（类型胶囊会空白、说明文字会走 mclo.gs 那一支）", b.Name)
		}
		if !b.Enabled {
			t.Errorf("%s 显示为已停用，但总开关是开着的（真机上就是这样：两张卡都挂「已停用」）", b.Name)
		}
		seen[b.Kind] = true
	}
	if !seen["logshare"] || !seen["mclogs"] {
		t.Errorf("内置提供方的 kind 应分别可读为 logshare / mclogs，实际 %v", seen)
	}
}
