package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// mclo.gs 客户端：**保底通道**，不是 AI 分析。
//
// 定位（2026-09-30 按用户反馈修正）：它是 Minecraft 互助社区里贴日志的通用做法 ——
// 把日志变成一个**可分享的链接**，别人（社区、模组作者、你的群、客服）点开就能看原文。
// 它**不提供任何 AI 结论**，只数一下 ERROR 行。
//
// 接口事实全部来自 2026-09-29 的实测（见 docs/ANALYSIS-PLUGGABLE.md 3.6）：
//   - 上传：POST /1/log，表单字段 content → {success,id,url,raw,token,lines,errors,expires,…}
//   - 原文：GET /1/raw/{id}（注意**不是** /1/log/{id}/raw，那个是 404）
//   - 元信息：GET /1/log/{id}
//   - 删除：DELETE /1/log/{id} + **请求头 Authorization: Bearer <token>**
//     （?token= / X-Token / 表单 / JSON body 四种写法实测都是 400 Missing token）
//   - 限制：GET /1/limits → storageTime(秒) / maxLength / maxLines
type MclogsClient struct {
	base string
	http *http.Client
}

// NewMclogsClient 创建客户端（base 默认 https://api.mclo.gs）。
func NewMclogsClient(base string) *MclogsClient {
	if strings.TrimSpace(base) == "" {
		base = "https://api.mclo.gs"
	}
	return &MclogsClient{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// MclogsResult 上传结果。
type MclogsResult struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Raw     string `json:"raw"`
	Token   string `json:"token"`
	Lines   int    `json:"lines"`
	Errors  int    `json:"errors"`
	Size    int64  `json:"size"`
	Expires int64  `json:"expires"`
	Source  string `json:"source"`
}

// MclogsLimits 平台限制。
type MclogsLimits struct {
	StorageTime int64 `json:"storageTime"`
	MaxLength   int64 `json:"maxLength"`
	MaxLines    int64 `json:"maxLines"`
}

type mclogsResp struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	MclogsResult
}

// Upload 上传一份日志，返回分享链接与删除凭据。
//
// source 会原样回显（对方字段），我们用它标注来源（atl-mcpanel/<版本>）。
func (c *MclogsClient) Upload(ctx context.Context, content, source string) (*MclogsResult, error) {
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("日志内容为空")
	}
	form := url.Values{}
	form.Set("content", content)
	if source != "" {
		form.Set("source", source)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/1/log", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out mclogsResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, fmt.Errorf("mclo.gs 拒绝了这次上传：%s", fallback(out.Error, "未说明原因"))
	}
	return &out.MclogsResult, nil
}

// GetMeta 读元信息（用于确认链接有效、拿保留期）。
func (c *MclogsClient) GetMeta(ctx context.Context, id string) (*MclogsResult, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/1/log/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	var out mclogsResp
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, fmt.Errorf("读取 mclo.gs 元信息失败：%s", fallback(out.Error, "日志不存在或已过期"))
	}
	return &out.MclogsResult, nil
}

// Delete 删除云端副本（**必须用 Bearer 头**，实测其它写法都返回 400）。
func (c *MclogsClient) Delete(ctx context.Context, id, token string) error {
	if token == "" {
		return errors.New("缺少删除凭据（token）")
	}
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.base+"/1/log/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	var out mclogsResp
	if err := c.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("删除失败：%s", fallback(out.Error, "凭据可能已失效"))
	}
	return nil
}

// GetLimits 读平台限制（保留期、单份大小/行数）。
func (c *MclogsClient) GetLimits(ctx context.Context) (*MclogsLimits, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/1/limits", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Success bool `json:"success"`
		MclogsLimits
	}
	if err := c.do(req, &raw); err != nil {
		return nil, err
	}
	if !raw.Success {
		return nil, errors.New("读取 mclo.gs 限制失败")
	}
	return &raw.MclogsLimits, nil
}

func (c *MclogsClient) do(req *http.Request, out interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求 mclo.gs 失败：%w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// 对方的错误体是 {"success":false,"error":"..."}，原文比状态码有用得多
		if err := json.Unmarshal(body, out); err == nil {
			if e, ok := out.(*mclogsResp); ok && e.Error != "" {
				return fmt.Errorf("mclo.gs 返回 HTTP %d：%s", resp.StatusCode, e.Error)
			}
		}
		return fmt.Errorf("mclo.gs 返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// ErrorCount 仅解析"错误行数"（供 UI 显示"数出 N 行 ERROR"）。
func (r *MclogsResult) ErrorCount() int { return r.Errors }

// RetentionDays 保留期（天，向上取整）—— 界面用的是天数，而接口给的是秒。
func (r *MclogsResult) RetentionDays() int {
	if r.Expires <= 0 {
		return 0
	}
	secs := r.Expires - time.Now().Unix()
	if secs <= 0 {
		return 0
	}
	return int((secs + 86399) / 86400)
}

// LimitsRetentionDays 平台保留期换算成天（从 /1/limits 的 storageTime）。
func (l *MclogsLimits) RetentionDays() int {
	if l == nil || l.StorageTime <= 0 {
		return 0
	}
	return int((l.StorageTime + 86399) / 86400)
}

// MaxLengthMB 单份上限（MB，向上取整）。
func (l *MclogsLimits) MaxLengthMB() int64 {
	if l == nil || l.MaxLength <= 0 {
		return 0
	}
	return (l.MaxLength + (1 << 20) - 1) >> 20
}

// 求助文本的生成已经搬到 helptext.go（模板化、管理员可改）。
// 这里保留 fallback：客户端的默认地址与版本号回退。

func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
