package httpapi

import (
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 界面「分析记录」里的求助文本（底账 §十二 第 1 条）。
//
// ⚠️ 这条用例落在 `analysisHistoryRecords` 上，不是 `/logshare` 那个接口 ——
// 前端 `listAnalysisHistory` 走的是 `GET /api/instances/{id}/analysis`。
// 2026-11-07 第一次就把快照加在了另一个端点上（`/logshare`），
// 接口测试全绿、浏览器一打开历史仍然是空白：**测试点必须与被测的那条链路一致**。
//
// 口径：
//  1. 有快照 → 原样返回（模板后来改过也不能覆盖历史）；
//  2. 没快照（早于本功能的旧记录）→ 按当前环境重建并标 help_reconstructed；
//  3. AI 类提供方（logshare / openai）既没有求助文本，也不该被标重建。
func TestAnalysisHistoryHelpTextSnapshot(t *testing.T) {
	srv, _ := newTestServer(t)
	seed := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := srv.db.Exec(q, args...); err != nil {
			t.Fatalf("准备数据失败: %v", err)
		}
	}
	seed(`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth) VALUES (9, 'n1', '127.0.0.1', 'root', '')`)
	seed(`INSERT INTO instances (instance_id, node_id, name) VALUES ('inst-a', 9, '快照实例')`)

	const snapshot = "【求助】崩溃了，这是当时贴出去的原文\n链接：https://mclo.gs/abc"
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-a", RemoteID: "abc", URL: "https://mclo.gs/abc",
		RawURL: "https://api.mclo.gs/1/raw/abc", SourcePath: "logs/latest.log",
		Size: 1024, Lines: 100, Errors: 3,
		ProviderKind: analysis.KindMclogs, HelpText: snapshot,
	}); id == 0 {
		t.Fatal("插入有快照的记录失败")
	}
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-a", RemoteID: "old", URL: "https://mclo.gs/old",
		RawURL: "https://api.mclo.gs/1/raw/old", SourcePath: "logs/latest.log",
		Size: 512, Lines: 50, Errors: 1, ProviderKind: analysis.KindMclogs,
	}); id == 0 {
		t.Fatal("插入旧记录失败")
	}
	if id := srv.insertUploadRecord(uploadRecord{
		InstanceID: "inst-a", RemoteID: "ai", SourcePath: "logs/latest.log",
		ProviderKind: analysis.KindLogShare,
	}); id == 0 {
		t.Fatal("插入 AI 记录失败")
	}

	list := srv.analysisHistoryRecords("inst-a")
	if len(list) != 3 {
		t.Fatalf("应有 3 条记录，实际 %d", len(list))
	}
	byID := map[string]analysisRecord{}
	for _, it := range list {
		byID[it.LogShareID] = it
	}

	got := byID["abc"]
	if got.HelpText != snapshot {
		t.Errorf("有快照时必须原样返回，实际 %q", got.HelpText)
	}
	if got.HelpReconstructed {
		t.Error("有快照的记录不该被标成「重建」")
	}

	old := byID["old"]
	if strings.TrimSpace(old.HelpText) == "" {
		t.Error("旧记录应回落到「按当前环境重建」，不能是空串（否则界面上又是空白）")
	}
	if !old.HelpReconstructed {
		t.Error("旧记录必须标 help_reconstructed，界面要说明这不是当时的原文")
	}
	if !strings.Contains(old.HelpText, "https://mclo.gs/old") {
		t.Errorf("重建的文本里应带上这条记录的链接：%q", old.HelpText)
	}

	ai := byID["ai"]
	if ai.HelpText != "" {
		t.Errorf("AI 类提供方不该有求助文本，实际 %q", ai.HelpText)
	}
	if ai.HelpReconstructed {
		t.Error("AI 类提供方不该被标成重建")
	}
}
