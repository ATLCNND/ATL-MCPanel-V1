package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/logshare"
)

// ============================================================================
// 删除云端副本：本地结论一起删 + 总开关关掉后**照样能删**
// ============================================================================

// fakeLogShare 一个只认 DELETE /log/<id> 的假 LogShare 服务。
//
// 为什么需要它：删远端副本这条路本来会真的打 api.logshare.cn（对方要 token 才给删），
// 而单元测试不该往第三方服务上发请求 —— 所以把客户端指向本机的假服务，
// 这样"远端删除成功之后本地做了什么"才是可断言的行为。
func fakeLogShare(t *testing.T) *httptest.Server {
	t.Helper()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/log/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(fake.Close)
	return fake
}

// seedLogShareUploadWithToken 写一条带 token 的上传记录（删远端副本必须带 token）。
func seedLogShareUploadWithToken(t *testing.T, srv *Server, instanceID, logshareID, token string) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT INTO logshare_uploads (instance_id, logshare_id, token) VALUES (?, ?, ?)`,
		instanceID, logshareID, token); err != nil {
		t.Fatalf("插入上传记录失败: %v", err)
	}
}

// countLogShareAnalyses 数本地还留着几行结论。
func countLogShareAnalyses(t *testing.T, srv *Server, instanceID, logshareID string) int {
	t.Helper()
	var n int
	if err := srv.db.QueryRow(
		`SELECT COUNT(1) FROM logshare_analyses WHERE instance_id = ? AND logshare_id = ?`,
		instanceID, logshareID).Scan(&n); err != nil {
		t.Fatalf("查本地结论失败: %v", err)
	}
	return n
}

// uploadMarkedDeleted 读某条上传记录是否已被标记删除。
func uploadMarkedDeleted(t *testing.T, srv *Server, instanceID, logshareID string) bool {
	t.Helper()
	var deleted bool
	if err := srv.db.QueryRow(
		`SELECT deleted_at IS NOT NULL FROM logshare_uploads WHERE instance_id = ? AND logshare_id = ?`,
		instanceID, logshareID).Scan(&deleted); err != nil {
		t.Fatalf("查上传记录失败: %v", err)
	}
	return deleted
}

// TestLogShareDeleteRemovesLocalConclusion "删除我的数据"必须**本地也算数**。
//
// 只把远端副本删掉是不够的：logshare_analyses.content 通常整段引用日志原文
//（玩家名、聊天、绝对路径），留在库里等于"要求第三方删掉、自己留一份"，
// 而且此后每个能看这台实例的人（collab 就行）都能从历史接口读到它。
func TestLogShareDeleteRemovesLocalConclusion(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "instA", 1)
	// 指向本地假服务：删远端副本要打第三方接口
	srv.logShare = logshare.New(fakeLogShare(t).URL, 5*time.Second)

	seedLogShareUploadWithToken(t, srv, "instA", "LS-DEL", "tok-del")
	seedLogShareAnalysis(t, srv, "instA", "LS-DEL",
		"结论里引用了原文：[10:01:10] <Steve> 你好")

	code, body := doJSON(t, ts, "DELETE", "/api/instances/instA/logshare/LS-DEL", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("删除应成功，实际 %d body=%v", code, body)
	}
	if n := countLogShareAnalyses(t, srv, "instA", "LS-DEL"); n != 0 {
		t.Errorf("本地结论应一并删除，实际仍留着 %d 行", n)
	}
	if !uploadMarkedDeleted(t, srv, "instA", "LS-DEL") {
		t.Error("上传记录应被标记为已删除（deleted_at）")
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "本地") {
		t.Errorf("提示应说明本地结论也删了，实际 %q", msg)
	}
}

// TestLogShareDeleteWorksWhenFeatureDisabled 总开关关掉之后**照样能删**。
//
// handleSetLogShareSettings 向用户承诺过"已上传的云端副本仍可删除"，
// 而删除是隐私方向的操作 —— 关掉开关之后用户更需要把以前传出去的删干净。
// 原来的实现在这里返回 503（功能未启用），把承诺变成了假话。
// 客户端是常驻对象（见 NewServer），所以关掉开关不影响删除。
func TestLogShareDeleteWorksWhenFeatureDisabled(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "instA", 1)
	srv.logShare = logshare.New(fakeLogShare(t).URL, 5*time.Second)

	if code, body := doJSON(t, ts, "PUT", "/api/logshare/settings", adminTok,
		map[string]interface{}{"enabled": false}); code != http.StatusOK {
		t.Fatalf("关闭总开关失败：%d %v", code, body)
	}
	if srv.LogShareEnabled() {
		t.Fatal("总开关没生效（仍是启用状态）")
	}

	seedLogShareUploadWithToken(t, srv, "instA", "LS-OFF", "tok-off")
	seedLogShareAnalysis(t, srv, "instA", "LS-OFF", "开关关闭前传出去的结论")

	code, body := doJSON(t, ts, "DELETE", "/api/instances/instA/logshare/LS-OFF", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("开关关闭时也应能删掉已传出去的副本，实际 %d body=%v", code, body)
	}
	if n := countLogShareAnalyses(t, srv, "instA", "LS-OFF"); n != 0 {
		t.Errorf("本地结论应一并删除，实际仍留着 %d 行", n)
	}
	if !uploadMarkedDeleted(t, srv, "instA", "LS-OFF") {
		t.Error("上传记录应被标记为已删除（deleted_at）")
	}
}
