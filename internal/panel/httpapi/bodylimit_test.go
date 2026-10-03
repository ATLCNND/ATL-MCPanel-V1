package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
// 请求体上限：登录（未鉴权）与编辑保存（已鉴权）
// ============================================================================
//
// 背景：整个面板此前只有 jars / noderesources / profile 三处用了 MaxBytesReader，
// 其余接口的请求体是**无上限**的，后果分两种，都很直接：
//
//   - POST /api/auth/login：`{"username":"<2GB>"}` 会在**限流之前**被解码成
//     一个 Go 字符串（json 解码器要先把整个字符串物化出来），
//     几个匿名请求就能把面板的常驻内存吃干 —— 限流跑在解码之后，拦不住它；
//   - POST /api/instances/{id}/file：编辑保存会把整份 content 读进内存再交给
//     Daemon，而这条路上原本连磁盘配额预检都没有（上传那条路是有的）。
//
// 这一组测试刻意**不经过网络**（httptest.NewRecorder 直接打 mux）：
// 服务端在客户端还在发送数据时截断请求体会关连接，真跑 TCP 的话
// 客户端可能先拿到 connection reset 而不是我们精心写好的那句提示 ——
// 那样测到的就是 TCP 的行为，而不是体限的行为。

// postRaw 直接把原始字节体发给 mux，返回状态码与响应体。
func postRaw(t *testing.T, srv *Server, path, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestLoginRejectsHugeBody 登录请求体超过上限要有明确响应，而不是先把它读进内存。
func TestLoginRejectsHugeBody(t *testing.T) {
	srv, ts := newTestServer(t)

	// 上限 64KB，这里来一个远超它的用户名（用户名是攻击者唯一能控制的长字段）
	huge, err := json.Marshal(map[string]string{
		"username": strings.Repeat("a", 128<<10),
		"password": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := postRaw(t, srv, "/api/auth/login", "", string(huge))
	if code != http.StatusBadRequest {
		t.Fatalf("超大请求体应 400，实际 %d body=%v", code, body)
	}

	// 正常大小的请求不受影响（体限不能误伤真实客户端）
	loginAs(t, ts, "admin", "admin123")
	code, body = doJSON(t, ts, "POST", "/api/auth/login", "",
		map[string]string{"username": "admin", "password": "admin123"})
	if code != http.StatusOK {
		t.Fatalf("正常登录不该被体限影响，实际 %d body=%v", code, body)
	}
}

// TestWriteFileRejectsHugeBody 编辑保存的请求体有上限（8 MiB），超了要 413。
//
// 这条断言刻意选在"超限"这一侧：拒绝必须发生在**调用 Daemon 之前**，
// 否则会先撞上"节点不可达"之类的错误，就测不到体限本身了。
func TestWriteFileRejectsHugeBody(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	huge, err := json.Marshal(map[string]string{
		"path":    "/server.properties",
		"content": strings.Repeat("x", maxWriteFileBytes+1024),
	})
	if err != nil {
		t.Fatal(err)
	}
	code, body := postRaw(t, srv, "/api/instances/inst1/file", adminTok, string(huge))
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过上限的保存应 413，实际 %d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "过大") {
		t.Errorf("拒绝原因应说明内容过大，实际 %q", msg)
	}
}
