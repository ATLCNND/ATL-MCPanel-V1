package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
// GET /api/instances/{id}/logshare/ai/{logshare_id} 的三条硬约束
// ============================================================================
//
// 这条路是"取 AI 结论"的 SSE 入口，三条约束各自对应一个真实缺口：
//
//  1. **logshare_id 必须属于 {id} 这台实例**。{id} 的授权只证明"你能看这台实例"，
//     而对方的日志 id 是**公开的**（分享链接里就有）。少了绑定，任何有 collab
//     的人都能借自己的实例去读别人租户那份日志的 AI 结论（IDOR）。
//  2. **命中缓存不再起上游分析**：结论已经落库（logshare_analyses），
//     "再看一次"不该再花一次对方（或管理员配的）额度。
//  3. **按用户限流**：这条路进来就会起一次上游 AI 调用。
//     此前整个面板只有 /api/instances/{id}/analyse 在计数，这里等于
//     一个不限速的免费 AI 中转。

// seedLogShareUpload 直接写一条上传记录（避开真实上传：上传会打第三方接口）。
func seedLogShareUpload(t *testing.T, srv *Server, instanceID, logshareID string) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO logshare_uploads (instance_id, logshare_id) VALUES (?, ?)`,
		instanceID, logshareID); err != nil {
		t.Fatalf("插入上传记录失败: %v", err)
	}
}

// seedLogShareAnalysis 直接写一条已落库的结论（模拟"分析已经跑完"）。
func seedLogShareAnalysis(t *testing.T, srv *Server, instanceID, logshareID, content string) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO logshare_analyses (instance_id, logshare_id, content) VALUES (?, ?, ?)`,
		instanceID, logshareID, content); err != nil {
		t.Fatalf("插入结论失败: %v", err)
	}
}

// logShareAIUser 建一个普通用户，并给他 instA 的 collab（取 AI 结论的最低级别）。
func logShareAIUser(t *testing.T, srv *Server, ts *httptest.Server, adminTok, username string) (string, int64) {
	t.Helper()
	if code, body := doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": username, "password": "ls123456", "role": "user"}); code != http.StatusCreated {
		t.Fatalf("创建用户 %s 失败：%d %v", username, code, body)
	}
	tok := loginAsToken(t, ts, username, "ls123456")
	uid := uidOf(t, srv, username)
	seedInstance(t, srv, "instA", 1)
	assignLevel(t, srv, "instA", uid, LevelCollab)
	return tok, uid
}

// TestLogShareAIRequiresUploadOwnedByInstance IDOR 的正面回归：
// 拿**别的实例**上传的 logshare_id 来取流，必须 404。
//
// 这个 id 就是对方日志的公开 id —— 攻击者不需要任何内部信息，
// 只要自己在别的租户的分享链接里见过它。
func TestLogShareAIRequiresUploadOwnedByInstance(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	userTok, _ := logShareAIUser(t, srv, ts, adminTok, "ls1")

	// 另一台实例（本用户没有任何权限）上传的日志
	seedInstance(t, srv, "instB", 1)
	seedLogShareUpload(t, srv, "instB", "LS-OTHER-TENANT")

	code, body := doJSON(t, ts, "GET", "/api/instances/instA/logshare/ai/LS-OTHER-TENANT", userTok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("不属于本实例的 logshare_id 应 404，实际 %d body=%v", code, body)
	}
	if _, ok := srv.aiRuns.runs["instA/LS-OTHER-TENANT"]; ok {
		t.Error("越权请求不得起上游分析")
	}

	// 完全没有上传记录（乱猜的 id）同样 404，而且与"有记录但不属于本实例"无异
	code, body = doJSON(t, ts, "GET", "/api/instances/instA/logshare/ai/LS-NOT-EXIST", userTok, nil)
	if code != http.StatusNotFound {
		t.Fatalf("不存在的 logshare_id 应 404，实际 %d body=%v", code, body)
	}
}

// TestLogShareAIServesCachedAnalysis 已落库的结论直接回放，不再起一次上游分析。
func TestLogShareAIServesCachedAnalysis(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	userTok, _ := logShareAIUser(t, srv, ts, adminTok, "ls2")

	seedLogShareUpload(t, srv, "instA", "LS-MINE")
	seedLogShareAnalysis(t, srv, "instA", "LS-MINE", "这是上次的结论")

	code, raw := call(t, ts, "GET", "/api/instances/instA/logshare/ai/LS-MINE", userTok, nil)
	if code != http.StatusOK {
		t.Fatalf("命中缓存应 200，实际 %d（%s）", code, raw)
	}
	if !strings.Contains(string(raw), "event: content") {
		t.Errorf("应以 SSE 回放结论，实际：%s", raw)
	}
	if !strings.Contains(string(raw), "这是上次的结论") {
		t.Errorf("回放的内容不对：%s", raw)
	}
	// 关键：命中缓存**不得**触发一次新的上游分析（否则白耗对方额度）
	if _, ok := srv.aiRuns.runs["instA/LS-MINE"]; ok {
		t.Error("命中缓存时不应起上游分析")
	}
}

// TestLogShareAIIsRateLimited 用光额度后必须 429，而不是照旧去起上游分析。
func TestLogShareAIIsRateLimited(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	userTok, uid := logShareAIUser(t, srv, ts, adminTok, "ls3")

	// 有上传记录、但**没有**落库的结论 → 走"起一次上游分析"那条路
	seedLogShareUpload(t, srv, "instA", "LS-UNCACHED")

	// 把该用户的额度用光（与 handler 用的是同一把 key：u:<id>）
	exhausted := false
	for i := 0; i < 100 && !exhausted; i++ {
		if ok, _, _ := srv.rateLimiter.Allow("u:" + itoa(uid)); !ok {
			exhausted = true
		}
	}
	if !exhausted {
		t.Fatal("限流器没有拒绝任何一次调用（默认每分钟 6 次，这里调了 100 次）")
	}

	code, body := doJSON(t, ts, "GET", "/api/instances/instA/logshare/ai/LS-UNCACHED", userTok, nil)
	if code != http.StatusTooManyRequests {
		t.Fatalf("额度用尽时应 429，实际 %d body=%v", code, body)
	}
	if _, ok := srv.aiRuns.runs["instA/LS-UNCACHED"]; ok {
		t.Error("被限流时不得起上游分析")
	}
}
