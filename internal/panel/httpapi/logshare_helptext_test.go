package httpapi

import (
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 历史记录必须能展示**上传当时**的求助文本（底账 §十二 第 1 条）。
//
// 背景：求助文本是「管理员可改的模板 + 当时的实例环境 + 当时填的现象」拼出来的，
// 用户是照着它去社区提问的。以前这个字段根本没存 —— 从「分析记录」点开 mclo.gs 那条，
// 求助文本是**空的**，用户回头找不到自己当时贴出去的那段话。
//
// 两条口径：
//  1. 有快照 → **原样返回**（哪怕现在模板已经改过，也不能拿新模板去覆盖历史）；
//  2. 没快照（早于本功能的旧记录）→ 按当前环境重建，并标 `help_reconstructed`，
//     让界面说清"这不是原件"。
func TestLogShareHelpTextSnapshot(t *testing.T) {
	srv, _ := newTestServer(t)
	seed := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := srv.db.Exec(q, args...); err != nil {
			t.Fatalf("准备数据失败: %v", err)
		}
	}
	seed(`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth) VALUES (9, 'n1', '127.0.0.1', 'root', '')`)
	seed(`INSERT INTO instances (instance_id, node_id, name) VALUES ('inst-h', 9, '求助快照实例')`)

	// ① 有快照的那条（正常情况下新记录都是这样）
	const snapshot = "【求助】服务器崩了，附上当时的求助文本\n链接：https://api.mclo.gs/1/log/abc"
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-h", RemoteID: "abc", URL: "https://api.mclo.gs/1/log/abc",
		SourcePath: "logs/latest.log", Size: 1024, Lines: 100, Errors: 3,
		ProviderKind: analysis.KindMclogs, HelpText: snapshot,
	}); id == 0 {
		t.Fatal("插入上传记录失败")
	}

	// ② 没有快照的那条（模拟升级前的老记录）
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-h", RemoteID: "old", URL: "https://api.mclo.gs/1/log/old",
		SourcePath: "logs/latest.log", Size: 512, Lines: 50, Errors: 1,
		ProviderKind: analysis.KindMclogs,
	}); id == 0 {
		t.Fatal("插入旧记录失败")
	}

	// ③ AI 类提供方：不该有求助文本（它们没有"去社区求助"这一步）
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-h", SourcePath: "logs/latest.log",
		ProviderKind: analysis.KindOpenAI,
	}); id == 0 {
		t.Fatal("插入 AI 记录失败")
	}

	list := srv.logShareHistory("inst-h")
	if len(list) != 3 {
		t.Fatalf("应有 3 条记录，实际 %d", len(list))
	}
	byID := map[string]logShareRecord{}
	for _, it := range list {
		byID[it.LogShareID] = it
	}

	got, ok := byID["abc"]
	if !ok {
		t.Fatal("找不到有快照的那条")
	}
	if got.HelpText != snapshot {
		t.Errorf("有快照时必须原样返回，实际 %q", got.HelpText)
	}
	if got.HelpReconstructed {
		t.Error("有快照的记录不该被标成「重建」")
	}

	old, ok := byID["old"]
	if !ok {
		t.Fatal("找不到旧记录那条")
	}
	if strings.TrimSpace(old.HelpText) == "" {
		t.Error("旧记录应回落到「按当前环境重建」，不能是空串（否则界面又是一片空白）")
	}
	if !old.HelpReconstructed {
		t.Error("旧记录必须标 help_reconstructed，界面要说明这不是当时的原文")
	}
	// 重建用的是**当前**模板：至少要带上模板里的固定文案，否则等于没重建
	if !strings.Contains(old.HelpText, "链接") && !strings.Contains(old.HelpText, "https://api.mclo.gs/1/log/old") {
		t.Errorf("重建的文本看不出是求助文本：%q", old.HelpText)
	}

	ai, ok := byID[""]
	if !ok {
		t.Fatal("找不到 AI 那条")
	}
	if ai.HelpText != "" {
		t.Errorf("AI 类提供方不该有求助文本，实际 %q", ai.HelpText)
	}
	if ai.HelpReconstructed {
		t.Error("AI 类提供方不该被标成重建")
	}

	// provider_kind 必须随历史返回：界面上"这条到底传给了谁"全靠它
	// （以前根本不在结构体里，那条标签一直是空的）
	if got.ProviderKind != analysis.KindMclogs {
		t.Errorf("mclo.gs 那条的 provider_kind 应为 %q，实际 %q", analysis.KindMclogs, got.ProviderKind)
	}
	if ai.ProviderKind != analysis.KindOpenAI {
		t.Errorf("AI 那条的 provider_kind 应为 %q，实际 %q", analysis.KindOpenAI, ai.ProviderKind)
	}
}
