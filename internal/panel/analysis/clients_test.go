package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 流式解析：要把 content 与 reasoning_content 分开，并在 [DONE] 结束。
//
// 为什么值得单测：真实的平台分片形态并不统一（有的只发 content、有的把思考放在
// reasoning_content、有的干脆在一个分片里给完整 message），而这些差异只在
// 真机上才会暴露 —— 单测把三种形态都钉住。
func TestOpenAIStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization 头不对：%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		write := func(s string) { fmt.Fprint(w, s); fl.Flush() }

		write("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先看内存\"}}]}\n\n")
		write("data: {\"choices\":[{\"delta\":{\"content\":\"堆内存不足\"}}]}\n\n")
		write(": 这是注释行，应被忽略\n\n")
		write("data: {\"choices\":[{\"delta\":{\"content\":\"，建议调大 -Xmx\"}}]}\n\n")
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "sk-test", Model: "m"})
	var thinking, content []string
	err := c.AnalyzeStream(context.Background(), "sys", "user", func(ev AIEvent) error {
		switch ev.Event {
		case "thinking":
			thinking = append(thinking, ev.Data)
		case "content":
			content = append(content, ev.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("流式分析失败: %v", err)
	}
	if strings.Join(thinking, "") != "先看内存" {
		t.Errorf("思考内容解析不对：%v", thinking)
	}
	if strings.Join(content, "") != "堆内存不足，建议调大 -Xmx" {
		t.Errorf("正文解析不对：%v", content)
	}
}

// base_url 少了 /v1 要自动补上：绝大多数平台是这个路径，
// 少了它会 404，而报错原文（"404 page not found"）很难让人联想到原因。
func TestOpenAIAutoV1Suffix(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_ = c.AnalyzeStream(context.Background(), "s", "u", func(AIEvent) error { return nil })
	if gotPath != "/v1/chat/completions" {
		t.Errorf("未自动补 /v1，实际请求路径 %q", gotPath)
	}

	// 已经带 /v1 的不要再补一次（否则会变成 /v1/v1/...）
	c2 := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "m"})
	_ = c2.AnalyzeStream(context.Background(), "s", "u", func(AIEvent) error { return nil })
	if gotPath != "/v1/chat/completions" {
		t.Errorf("重复补了 /v1，实际 %q", gotPath)
	}
}

// 报错要带上状态码、对方原文与"该往哪查"的提示。
//
// 这是配错时唯一能救命的东西：401/404/400 的处理方式完全不同，
// 全都笼统报"分析失败"的话，用户只能盲试。
func TestOpenAIErrorMessages(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   []string
	}{
		{http.StatusUnauthorized, `{"error":{"message":"Invalid API key"}}`, []string{"401", "Invalid API key", "API Key"}},
		{http.StatusNotFound, `{"error":{"message":"model not found"}}`, []string{"404", "model not found", "/v1"}},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, []string{"429", "rate limited", "限流"}},
		{http.StatusBadRequest, `{"message":"bad request"}`, []string{"400", "bad request"}},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		cli := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
		err := cli.AnalyzeStream(context.Background(), "s", "u", func(AIEvent) error { return nil })
		srv.Close()
		if err == nil {
			t.Errorf("HTTP %d 应当返回错误", c.status)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("HTTP %d 的错误信息应包含 %q，实际：%v", c.status, w, err)
			}
		}
	}
}

// 平台在流里返回 error 字段时要立刻停下并报出来（而不是当作正常结束）。
func TestOpenAIStreamErrorFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"insufficient quota\"}}\n\n")
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	err := c.AnalyzeStream(context.Background(), "s", "u", func(AIEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "insufficient quota") {
		t.Errorf("流里的错误帧应当被报出来，实际：%v", err)
	}
}

// Ping：连通时返回耗时、配置错时返回可读原因。
func TestOpenAIPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] != false {
			t.Errorf("自检应当用非流式请求，实际 stream=%v", body["stream"])
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"pong"}}]}`)
	}))
	defer srv.Close()

	c := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	d, err := c.Ping(context.Background())
	if err != nil {
		t.Fatalf("自检应当成功：%v", err)
	}
	if d <= 0 || d > 30*time.Second {
		t.Errorf("耗时不合理：%v", d)
	}

	// 缺 model：应当在发请求之前就报错（省一次无意义的网络往返）
	c2 := NewOpenAIClient(OpenAIConfig{BaseURL: srv.URL, APIKey: "k"})
	if _, err := c2.Ping(context.Background()); err == nil {
		t.Error("缺模型名时自检应当报错")
	}
}

// mclo.gs：上传/原文/删除/limits 的形态都要按实测的接口来。
//
// 特别钉住删除要 **Authorization: Bearer**（实测 ?token= 等四种写法都是 400）——
// 这条如果写错，表现是"云端副本删不掉"，而日志里的错误信息只有一句 Missing token。
func TestMclogsClient(t *testing.T) {
	var deletedWith string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/1/log" && r.Method == "POST":
			_ = r.ParseForm()
			if r.Form.Get("content") == "" {
				t.Error("上传必须带上 content 字段")
			}
			if r.Form.Get("source") != "atl-mcpanel/test" {
				t.Errorf("source 应原样带上，实际 %q", r.Form.Get("source"))
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "id": "abc123", "url": "https://mclo.gs/abc123",
				"raw": "https://api.mclo.gs/1/raw/abc123", "token": "tok-1",
				"lines": 42, "errors": 7, "size": 1234,
				"expires": time.Now().Add(90 * 24 * time.Hour).Unix(),
			})
		case r.URL.Path == "/1/log/abc123" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": "abc123", "lines": 42, "errors": 7})
		case r.URL.Path == "/1/log/abc123" && r.Method == "DELETE":
			deletedWith = r.Header.Get("Authorization")
			if deletedWith != "Bearer tok-1" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "Missing token."})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
		case r.URL.Path == "/1/limits":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true, "storageTime": 7776000, "maxLength": 10485760, "maxLines": 25000,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewMclogsClient(srv.URL)
	ctx := context.Background()

	res, err := c.Upload(ctx, "some log", "atl-mcpanel/test")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if res.ID != "abc123" || res.Errors != 7 || res.Lines != 42 {
		t.Errorf("上传结果解析不对：%+v", res)
	}
	if res.RetentionDays() < 89 || res.RetentionDays() > 91 {
		t.Errorf("保留期换算不对（应约 90 天）：%d", res.RetentionDays())
	}

	if _, err := c.GetMeta(ctx, "abc123"); err != nil {
		t.Errorf("读元信息失败: %v", err)
	}
	if err := c.Delete(ctx, "abc123", "tok-1"); err != nil {
		t.Errorf("删除失败: %v", err)
	}
	if deletedWith != "Bearer tok-1" {
		t.Errorf("删除必须用 Bearer 头，实际 %q", deletedWith)
	}
	// 没有 token 时要在本地就拦住（省一次必然失败的请求）
	if err := c.Delete(ctx, "abc123", ""); err == nil {
		t.Error("缺 token 时应当直接报错")
	}

	lim, err := c.GetLimits(ctx)
	if err != nil {
		t.Fatalf("读 limits 失败: %v", err)
	}
	if lim.RetentionDays() != 90 || lim.MaxLengthMB() != 10 {
		t.Errorf("limits 换算不对：%d 天 / %d MB", lim.RetentionDays(), lim.MaxLengthMB())
	}
}

// 对方返回 success=false 时要把它的 error 原文带出来。
func TestMclogsClientServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false, "error": "Content is too long",
		})
	}))
	defer srv.Close()

	c := NewMclogsClient(srv.URL)
	_, err := c.Upload(context.Background(), "log", "")
	if err == nil || !strings.Contains(err.Error(), "Content is too long") {
		t.Errorf("应把对方的错误原文带出来，实际：%v", err)
	}
}
