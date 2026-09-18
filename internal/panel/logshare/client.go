// Package logshare 接入 https://logshare.cn 的日志分析 API（第三方免费服务）。
//
// 接口事实来自 2026-09-18 的真实调用，不是文档转述：
//   - 公共 API，**无需认证**；HTTP 会被重定向到 HTTPS
//   - 上传：POST {base}/log，JSON body {source, content, files[]}
//     → 返回 {id, url, raw, token}
//   - 元信息：GET {base}/log/{id}（含 expires，默认保留 15 天）
//   - 结构化：GET {base}/insights/{id}
//   - AI 分析：GET {base}/ai/{id}，**SSE 流式**（多轮工具调用，读超时建议 ≥300s）
//   - 删除：DELETE {base}/log/{id}，**必须带上传时返回的 token**（丢了就删不掉）
//   - 服务端会自动把 IPv4/IPv6 打码，但**玩家名与聊天内容不过滤**
//
// 这个包只负责"与对方说话"，不认识实例、不认识数据库：
// 由 httpapi 负责取日志、存 token、鉴权。
package logshare

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client LogShare API 客户端。
type Client struct {
	base string
	http *http.Client
}

// New 创建客户端。base 形如 https://api.logshare.cn/v1。
//
// timeout 是"一次分析最多等多久"（默认 300 秒）：官方明确建议读超时 ≥300 秒，
// 因为 AI 会多轮调用工具，可能持续数十秒到几分钟。
func New(base string, timeout time.Duration) *Client {
	if base == "" {
		base = "https://api.logshare.cn/v1"
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		// 单次请求的超时统一由 ctx 控制；这里再留 30 秒余量兜底，
		// 避免"连接建立后对方不再发数据"时永久挂住。
		http: &http.Client{Timeout: timeout + 30*time.Second},
	}
}

// UploadFile 上传的附加文件（如 crash-report）。
type UploadFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// UploadResult 上传结果。
type UploadResult struct {
	ID    string `json:"id"`
	URL   string `json:"url"`
	Raw   string `json:"raw"`
	Token string `json:"token"`
}

type uploadReq struct {
	Source  string       `json:"source"`
	Content string       `json:"content"`
	Files   []UploadFile `json:"files,omitempty"`
}

type apiError struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Upload 提交日志。
//
// 刻意**不手动设置 Accept-Encoding**：一旦自己设了请求头，net/http 就不再
// 自动解压响应，而对方默认用 gzip 回 —— 拿到的是二进制而不是 JSON。
// 交给 Transport 自动处理最省事。
func (c *Client) Upload(ctx context.Context, source, content string, files []UploadFile) (*UploadResult, error) {
	body, err := json.Marshal(uploadReq{Source: source, Content: content, Files: files})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/log", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接日志分析服务失败：%w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("日志分析服务限流（Retry-After: %s），请稍后重试",
			orDefault(resp.Header.Get("Retry-After"), "未提供"))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上传失败（HTTP %d）：%s", resp.StatusCode, apiErrText(data))
	}

	var out UploadResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析上传响应失败：%w", err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("上传响应里没有 id：%s", truncate(string(data), 300))
	}
	return &out, nil
}

// Meta 日志元信息。
type Meta struct {
	ID      string `json:"id"`
	Size    int64  `json:"size"`
	Lines   int    `json:"lines"`
	Created int64  `json:"created"`
	Expires int64  `json:"expires"`
	Source  string `json:"source"`
	Files   []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

// GetMeta 读取元信息（主要是拿 expires，用于界面提示"何时自动消失"）。
func (c *Client) GetMeta(ctx context.Context, id string) (*Meta, error) {
	data, code, err := c.get(ctx, "/log/"+id)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("读取日志元信息失败（HTTP %d）", code)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Insights 结构化分析结果。
//
// 原样透传 JSON 给前端、不做二次映射：对方的结构会演进，
// 我们在中间翻译只会变成两头都要改。
func (c *Client) Insights(ctx context.Context, id string) (json.RawMessage, error) {
	data, code, err := c.get(ctx, "/insights/"+id)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("读取结构化分析失败（HTTP %d）", code)
	}
	return json.RawMessage(data), nil
}

// Delete 删除云端副本。**必须带 token**（上传时返回的那个）。
func (c *Client) Delete(ctx context.Context, id, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/log/"+id, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("删除失败（HTTP %d）：%s", resp.StatusCode, apiErrText(data))
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, nil
}

// AIEvent AI 分析 SSE 的一个事件（原样转发给浏览器）。
type AIEvent struct {
	// Event SSE 的 event 字段（status / done …）
	Event string
	// Data 原始 data 行（JSON 字符串，交给前端解析）
	Data string
}

// StreamAI 拉取 AI 分析流并逐事件回调。
//
// 为什么逐事件回调而不是"等完整再返回"：官方说多轮工具分析可能持续
// 数十秒到几分钟，一次给完会让用户盯着空白等很久；而流里既有思考增量
// 也有最终答案，实时渲染才有价值。
//
// 解析刻意保持**最小**：只按 SSE 规范切 event/data 行、原样透传 data，
// 不去理解里面的 JSON 结构（type/choices/delta 是对方的实现细节，
// 我们跟着解析就会一直修）。
//
// 但"切事件"这件事必须按规范做：**同一事件的多个 data 行要用 \n 连接后
// 一次性回调**。按行回调看起来更"实时"，实际会把一段长 JSON 拆成几段
// 不完整的 JSON —— 前端 JSON.parse 全部失败，表现为"分析跑了但什么都没显示"，
// 而且只在答案较长时才复现，很难归因。
func (c *Client) StreamAI(ctx context.Context, id string, onEvent func(AIEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/ai/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("AI 分析失败（HTTP %d）：%s", resp.StatusCode, apiErrText(body))
	}

	sc := bufio.NewScanner(resp.Body)
	// SSE 单行可能很长（思考增量或答案片段），默认 64KB 会截断
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var event string
	var dataLines []string

	// flush 把一个完整事件交给调用方。
	// 返回 errStreamDone 表示对方已宣告结束（done 事件或 [DONE]）。
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if err := onEvent(AIEvent{Event: event, Data: data}); err != nil {
			return err
		}
		if event == "done" || data == "[DONE]" {
			return errStreamDone
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// 空行 = 事件边界
			if err := flush(); err != nil {
				return doneOrErr(err)
			}
			event = ""
		case strings.HasPrefix(line, ":"):
			// 注释行（心跳），忽略
		case strings.HasPrefix(line, "event:"):
			// 兜底：遇到 event 行说明上一个事件该结束了（万一对方漏了空行）
			if err := flush(); err != nil {
				return doneOrErr(err)
			}
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil { // 结尾没空行时的兜底
		return doneOrErr(err)
	}
	return sc.Err()
}

// errStreamDone 内部哨兵：不是错误，只是"流正常结束"。
var errStreamDone = errors.New("logshare: stream done")

func doneOrErr(err error) error {
	if errors.Is(err, errStreamDone) {
		return nil
	}
	return err
}

// apiErrText 从错误响应里取出最有用的那句话。
func apiErrText(data []byte) string {
	var e apiError
	if json.Unmarshal(data, &e) == nil {
		if e.Error != "" {
			return truncate(e.Error, 300)
		}
		if e.Message != "" {
			return truncate(e.Message, 300)
		}
	}
	return truncate(strings.TrimSpace(string(data)), 300)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
