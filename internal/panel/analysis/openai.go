package analysis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// OpenAI 兼容平台客户端。
//
// 为什么只做这一种协议（D1c 的结论）：自建网关（vLLM / LiteLLM / One-API / new-api）、
// 免费额度平台、商业 API（DeepSeek / OpenAI / 通义 / 智谱…）**都能**用同一套
// `POST {base}/chat/completions` + `Authorization: Bearer <key>` + SSE 描述清楚。
// 先做一种覆盖绝大多数需求，其余平台等真有需求再加适配器。
type OpenAIClient struct {
	base    string
	apiKey  string
	model   string
	prompt  string
	timeout time.Duration
	http    *http.Client
}

// OpenAIConfig 创建 OpenAI 兼容客户端所需的参数。
type OpenAIConfig struct {
	BaseURL string
	APIKey  string
	Model   string
	Prompt  string // 可选：自定义系统提示词
	Timeout time.Duration
	// AllowPrivateURL 是否允许目标在内网（见 ssrf.go；管理员显式打开）
	AllowPrivateURL bool
}

// NewOpenAIClient 创建客户端。
func NewOpenAIClient(cfg OpenAIConfig) *OpenAIClient {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	// 用户填 https://api.deepseek.com 时补上 /v1：绝大多数平台都是这个路径，
	// 少了它会 404，而报错原文（"404 page not found"）很难让人联想到"少了 /v1"。
	if base != "" && !strings.HasSuffix(base, "/v1") && !strings.Contains(base, "/v1/") &&
		!strings.HasSuffix(base, "/chat/completions") {
		base += "/v1"
	}
	t := cfg.Timeout
	if t <= 0 {
		t = 300 * time.Second
	}
	return &OpenAIClient{
		base:    base,
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		prompt:  cfg.Prompt,
		timeout: t,
		http: &http.Client{
			Timeout:       t + 30*time.Second, // 留 30 秒余量兜底，见 logshare.Client 的同类注释
			CheckRedirect: RedirectGuard(cfg.AllowPrivateURL),
		},
	}
}

// AIEvent 流式分析里的一帧，与 LogShare 那条链路保持同构，前端可以共用渲染逻辑。
type AIEvent struct {
	Event string `json:"event"` // status / thinking / content / done / error
	Data  string `json:"data"`
}

// AnalyzeStream 发起一次流式分析，逐帧回调。
//
// 错误分两类，且**必须区分**（这是调用方最需要的两条信息）：
//   - 配置错（key 无效、模型名不对、base_url 写错）→ 带 HTTP 状态码与对方原文，
//     用户看到就能改对，不该被"网络错误"这种笼统说法糊过去；
//   - 网络/超时 → 说明是链路问题，与配置无关。
func (c *OpenAIClient) AnalyzeStream(ctx context.Context, system, user string, emit func(AIEvent) error) error {
	if c.base == "" {
		return errors.New("未配置平台地址（base_url）")
	}
	if c.model == "" {
		return errors.New("未配置模型名（model）")
	}
	if c.prompt != "" && strings.TrimSpace(system) == "" {
		system = c.prompt
	}

	body := map[string]interface{}{
		"model":  c.model,
		"stream": true,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求分析平台失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 错误原文一定要带回来：401=key 错、404=路径错（少 /v1）、400=模型名或参数错，
		// 这三种情况的处理方式完全不同，而它们的响应体里写着具体原因。
		return fmt.Errorf("%s", describeHTTPError(resp))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawContent := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"` // DeepSeek 等把思考放这里
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 单个分片解析失败不该打断整条流
		}
		if chunk.Error != nil && chunk.Error.Message != "" {
			return errors.New("平台返回错误：" + chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		d := chunk.Choices[0].Delta
		if d.ReasoningContent != "" {
			if err := emit(AIEvent{Event: "thinking", Data: d.ReasoningContent}); err != nil {
				return err
			}
		}
		if d.Content != "" {
			sawContent = true
			if err := emit(AIEvent{Event: "content", Data: d.Content}); err != nil {
				return err
			}
		}
		// 有些平台不用流式分片，只给一条完整 message
		if d.Content == "" && chunk.Choices[0].Message.Content != "" {
			sawContent = true
			if err := emit(AIEvent{Event: "content", Data: chunk.Choices[0].Message.Content}); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil && !sawContent {
		return fmt.Errorf("读取响应流失败：%w", err)
	}
	return nil
}

// Ping 连通性自检：发一条极短的非流式请求，只回答"能不能通、报什么错、用了多久"。
//
// 为什么必须有这个接口：key 写错、base_url 少了 /v1、模型名不对、网关要求
// max_tokens……这些只有当场看到错误原文才能改对；否则用户是在"传完几 MB 日志"
// 之后才发现配错了（见 docs/ANALYSIS-PLUGGABLE.md 3.2）。
func (c *OpenAIClient) Ping(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	if c.base == "" {
		return 0, errors.New("未配置平台地址（base_url）")
	}
	// 先在本地拦一道：缺模型名时发出去也一定被平台拒，
	// 而"参数或模型名不被接受"这种远端报错不如一句"未配置模型名"好懂。
	if c.model == "" {
		return 0, errors.New("未配置模型名（model）")
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model":      c.model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return time.Since(start), fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Since(start), errors.New(describeHTTPError(resp))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return time.Since(start), nil
}

// describeHTTPError 把"非 200"的响应压成一句能照着改的话（含状态码与对方原文）。
func describeHTTPError(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(raw))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Error.Message != "" {
			msg = e.Error.Message
		} else if e.Message != "" {
			msg = e.Message
		}
	}
	hint := ""
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		hint = "（API Key 无效或没有权限）"
	case http.StatusNotFound:
		hint = "（地址不对：多数平台需要以 /v1 结尾，或模型名不存在）"
	case http.StatusTooManyRequests:
		hint = "（平台限流：稍后重试，或换一个模型/平台）"
	case http.StatusBadRequest:
		hint = "（参数或模型名不被接受）"
	}
	if msg == "" {
		msg = "（响应体为空）"
	}
	// 上游把请求头原样回显进错误体是常见做法（网关的"你的 Authorization: Bearer sk-… 无效"
	// 就是这个形状）。这段文字会一路显示到**任何租户**的界面上，而它可能是
	// 管理员配的全局平台的 key —— 所以先把疑似凭据抹掉再外传
	//（2026-10-01 安全审查 L6）。抹掉之后对排查没有损失：错误原因本身还在。
	msg = scrubSecrets(msg)
	return fmt.Sprintf("平台返回 HTTP %d %s%s", resp.StatusCode, msg, hint)
}

// scrubSecrets 把错误文本里疑似凭据的片段替换成 <已隐藏>。
//
// 覆盖三类最常见形态：`Bearer xxx`、各大平台的 key 前缀、以及很长的
// 无空格 token。宁可多抹一点也不能漏 —— 这类文本的用途是"告诉用户哪里配错了"，
// 不需要原样保留凭据。
func scrubSecrets(s string) string {
	s = bearerRe.ReplaceAllString(s, "Bearer <已隐藏>")
	s = keyRe.ReplaceAllString(s, "<已隐藏>")
	return s
}

var (
	// Bearer <token>（大小写不敏感）
	bearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-+/=]{8,}`)
	// 常见平台密钥前缀 + 足够长的随机串
	keyRe = regexp.MustCompile(`\b(?:sk|rk|pk|ghp|gho|github_pat|xox[baprs]|AKIA|AIza)[-_A-Za-z0-9]{12,}`)
)
