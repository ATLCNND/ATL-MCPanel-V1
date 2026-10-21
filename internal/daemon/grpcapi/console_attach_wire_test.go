package grpcapi

import (
	"encoding/json"
	"testing"
)

// 这是一条**跨进程线格式**：前端 attachPayloadFor 序列化 → 面板原样转发
// → Daemon 反序列化。键名写错不会有编译期报错，只会"静默不生效"
// （用户看到的就是"设置改了没反应"），所以这里把键名逐字钉死。
//
// 与 analysis_wire_test.go 同一套思路，理由也相同：Go 的 json 反序列化
// 对键名大小写不敏感，**看值的测试拦不住**这类缺陷。
func TestConsoleAttachWireHistory(t *testing.T) {
	// 前端真实产生的形状（见 web/src/components/ConsoleTab.tsx 的 attachPayloadFor）
	// 规则里刻意带一条中文（用户真实用的例子是 `Task|任务`）：
	// 这条链路要保证非 ASCII 原样穿过 URL 编码 → gRPC → RE2。
	raw := `{"history":1000,"spam":{"rules":[` +
		`{"id":"hb.players","pattern":"There are [0-9]+ of a max of","flags":"i","block":false},` +
		`{"id":"c1","pattern":"Task|任务","flags":"i","block":false}]}}`

	var opts consoleAttachOpts
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if opts.History != 1000 {
		t.Errorf("history 键没被读到（实际 %d）；前端与 Daemon 的线格式已经不一致", opts.History)
	}
	if len(opts.Spam.Rules) != 2 {
		t.Fatalf("spam.rules 条数 = %d，期望 2", len(opts.Spam.Rules))
	}
	if got := opts.Spam.Rules[0].ID; got != "hb.players" {
		t.Errorf("规则 id = %q，期望 hb.players（spam.rules 的键名或字段名漂了）", got)
	}
	if opts.Spam.Rules[0].Block {
		t.Error("block 应为 false")
	}
	if got := opts.Spam.Rules[1].Pattern; got != "Task|任务" {
		t.Errorf("中文规则原样穿过失败：%q", got)
	}
}

// 老前端（或面板缓存里的旧 bundle）只发 spam、不带 history ——
// 必须仍然能解析，且 history 为 0（= 没提要求 → 走默认 500，行为与改动前一致）。
func TestConsoleAttachWireLegacyPayload(t *testing.T) {
	raw := `{"spam":{"rules":[{"id":"c1","pattern":"Task|任务","flags":"i","block":false}]}}`
	var opts consoleAttachOpts
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if opts.History != 0 {
		t.Errorf("没带 history 时应为 0，实际 %d", opts.History)
	}
	if len(opts.Spam.Rules) != 1 {
		t.Errorf("老格式的规则没解析出来，条数 = %d", len(opts.Spam.Rules))
	}
	if got := resolveConsoleHistory(opts.History); got != consoleHistoryLines {
		t.Errorf("老客户端应退回默认 %d 行，实际 %d", consoleHistoryLines, got)
	}
}

// 空 data（不带任何参数）也必须安全：不 panic、退回默认。
func TestConsoleAttachWireEmpty(t *testing.T) {
	var opts consoleAttachOpts
	if err := json.Unmarshal([]byte(""), &opts); err == nil {
		t.Log("空串解析不报错也可以接受")
	}
	if got := resolveConsoleHistory(opts.History); got != consoleHistoryLines {
		t.Errorf("空参数应退回默认 %d 行，实际 %d", consoleHistoryLines, got)
	}
}

func TestResolveConsoleHistory(t *testing.T) {
	cases := []struct {
		name string
		want int
		exp  int
	}{
		{"没提要求（老客户端）", 0, consoleHistoryLines},
		{"负数按没提要求", -5, consoleHistoryLines},
		{"低于下限被抬起", 1, consoleHistoryMin},
		{"下限边界", consoleHistoryMin, consoleHistoryMin},
		{"正常值原样", 1000, 1000},
		{"上限边界", consoleHistoryMax, consoleHistoryMax},
		// 往上一版滑块最大是 50000、而这里只认到 5000，结果滑块后半段"拖了没反应"。
		// 现在两端都是 consoleHistoryMax（前端 RETAIN_MAX/REPLAY_MAX 与之同值），
		// 这条仍然保留：它保证**将来任何一端单独抬高**时，另一端至少不会越界。
		{"超过上限被压住（前端滑块与这里同值）", 50000, consoleHistoryMax},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveConsoleHistory(c.want); got != c.exp {
				t.Errorf("resolveConsoleHistory(%d) = %d，期望 %d", c.want, got, c.exp)
			}
		})
	}
}
