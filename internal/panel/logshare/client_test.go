package logshare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 这些测试用本地假服务器固定"我们期望对方怎么回"，
// 真实接口的事实（路径、字段、token 语义）在包注释里记着，
// 这里只保证解析与拼接不会把对方的响应读坏。

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, 5*time.Second), srv
}

func TestUpload_SendsFieldsAndParsesResult(t *testing.T) {
	var got uploadReq
	var gotPath, gotCT string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotCT = r.URL.Path, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"id":"A3DoFae","url":"https://logshare.cn/A3DoFae","token":"tk-1"}`))
	})

	res, err := c.Upload(context.Background(), "atl-mcpanel/0.9.13", "log line",
		[]UploadFile{{Name: "crash.txt", Content: "boom"}})
	if err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
	if gotPath != "/log" || gotCT != "application/json" {
		t.Fatalf("请求不对: path=%s ct=%s", gotPath, gotCT)
	}
	if got.Source != "atl-mcpanel/0.9.13" || got.Content != "log line" || len(got.Files) != 1 {
		t.Fatalf("请求体不对: %+v", got)
	}
	if res.ID != "A3DoFae" || res.Token != "tk-1" {
		t.Fatalf("响应解析不对: %+v", res)
	}
}

func TestUpload_ErrorPaths(t *testing.T) {
	t.Run("限流要提示稍后重试", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		_, err := c.Upload(context.Background(), "s", "c", nil)
		if err == nil || !strings.Contains(err.Error(), "限流") || !strings.Contains(err.Error(), "60") {
			t.Fatalf("限流错误提示不对: %v", err)
		}
	})

	t.Run("对方返回 error 字段要透出", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"error":"日志内容为空"}`))
		})
		_, err := c.Upload(context.Background(), "s", "c", nil)
		if err == nil || !strings.Contains(err.Error(), "日志内容为空") {
			t.Fatalf("错误文本不对: %v", err)
		}
	})

	t.Run("没有 id 要当成失败", func(t *testing.T) {
		// 没有 id 就既存不了 token 也删不掉，必须报错而不是静默成功
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"success":true}`))
		})
		_, err := c.Upload(context.Background(), "s", "c", nil)
		if err == nil || !strings.Contains(err.Error(), "没有 id") {
			t.Fatalf("缺 id 应报错: %v", err)
		}
	})
}

func TestDelete_SendsBearerToken(t *testing.T) {
	var auth, path, method string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path, method = r.Header.Get("Authorization"), r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	if err := c.Delete(context.Background(), "A3DoFae", "tk-1"); err != nil {
		t.Fatalf("删除应成功: %v", err)
	}
	if method != http.MethodDelete || path != "/log/A3DoFae" {
		t.Fatalf("请求不对: %s %s", method, path)
	}
	if auth != "Bearer tk-1" {
		t.Fatalf("必须带上传时返回的 token，实际 %q", auth)
	}
}

func TestStreamAI_ParsesEvents(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(": ping\n\n" +
			"event: status\ndata: {\"type\":\"thinking\",\"delta\":\"嗯\"}\n\n" +
			"event: status\ndata: {\"type\":\"content\",\"delta\":\"答案\"}\n\n" +
			"event: done\ndata: {\"ok\":true}\n\n"))
	})

	var got []AIEvent
	err := c.StreamAI(context.Background(), "A3DoFae", func(ev AIEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("流应正常结束: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应收到 3 个事件（注释行不算），实际 %d: %+v", len(got), got)
	}
	if got[0].Event != "status" || !strings.Contains(got[0].Data, "thinking") {
		t.Fatalf("第一个事件不对: %+v", got[0])
	}
	if got[2].Event != "done" {
		t.Fatalf("最后应是 done: %+v", got[2])
	}
}

func TestStreamAI_MultilineDataJoined(t *testing.T) {
	// 回归：规范允许一个事件的 JSON 被拆到多个 data 行。
	// 按行回调会把半截 JSON 丢给前端，前端 JSON.parse 全失败
	// （症状是"分析跑完了但页面空白"），所以必须按规范 \n 连接后再回调一次。
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: status\ndata: {\"type\":\"content\",\ndata: \"delta\":\"半截\"}\n\n"))
	})

	var got []AIEvent
	if err := c.StreamAI(context.Background(), "x", func(ev AIEvent) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("流应正常结束: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("多行 data 应合成一个事件，实际 %d 个: %+v", len(got), got)
	}
	var v struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(got[0].Data), &v); err != nil {
		t.Fatalf("合成后的 data 应是完整 JSON: %v（%q）", err, got[0].Data)
	}
	if v.Delta != "半截" {
		t.Fatalf("内容不对: %+v", v)
	}
}

func TestStreamAI_StopsOnDoneMarker(t *testing.T) {
	// 对方发 [DONE] 之后不该再等连接关闭：否则用户要盯着转圈等 TCP 超时
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\ndata: 不该被读到\n\n"))
	})
	if err := c.StreamAI(context.Background(), "x", func(AIEvent) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("应正常结束: %v", err)
	}
	if calls != 1 {
		t.Fatalf("[DONE] 之后应停止，实际回调 %d 次", calls)
	}
}

func TestStreamAI_ErrorStatus(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"日志不存在或已过期"}`))
	})
	err := c.StreamAI(context.Background(), "gone", func(AIEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("错误信息不对: %v", err)
	}
}

func TestGetMetaAndInsights(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/log/A1":
			_, _ = w.Write([]byte(`{"id":"A1","lines":120,"expires":1790000000}`))
		case "/insights/A1":
			_, _ = w.Write([]byte(`{"summary":"OOM"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	m, err := c.GetMeta(context.Background(), "A1")
	if err != nil {
		t.Fatalf("读元信息失败: %v", err)
	}
	if m.Lines != 120 || m.Expires != 1790000000 {
		t.Fatalf("元信息不对: %+v", m)
	}

	ins, err := c.Insights(context.Background(), "A1")
	if err != nil {
		t.Fatalf("读结构化分析失败: %v", err)
	}
	if !strings.Contains(string(ins), "OOM") {
		t.Fatalf("结构化结果不对: %s", ins)
	}
}

func TestNew_DefaultsAndBaseTrim(t *testing.T) {
	c := New("https://api.logshare.cn/v1/", 0)
	if c.base != "https://api.logshare.cn/v1" {
		t.Fatalf("base 应去掉结尾斜杠: %s", c.base)
	}
	if c.http.Timeout <= 300*time.Second {
		t.Fatalf("超时应留有余量（对方 AI 要跑几分钟），实际 %v", c.http.Timeout)
	}
	if def := New("", 0); def.base != "https://api.logshare.cn/v1" {
		t.Fatalf("空 base 应回落到官方地址: %s", def.base)
	}
}
