package analysis

import (
	"context"
	"fmt"
	"strings"
)

// 提供方类型。
const (
	KindLogShare     = "logshare"      // 公益合作首选：AI 分析
	KindMclogs       = "mclogs"        // 保底：不做 AI，只把日志变成可分享链接
	KindOpenAI       = "openai"        // 用户/管理员自配的 OpenAI 兼容平台
	KindBuiltinRules = "builtin-rules" // V2：本地规则引擎（离线可用）
)

// Provider 一条分析提供方配置（数据库里的行，去掉密文）。
//
// APIKey 只在**当次调用**里出现：接口层解密后构造客户端，用完即弃，
// 不回显、不写日志、不进审计（见 secret.go 与 docs/ANALYSIS-PLUGGABLE.md 3.3）。
type Provider struct {
	ID         int64
	Name       string
	Kind       string
	BaseURL    string
	Model      string
	APIKey     string
	KeyHint    string
	MaxBytes   int64
	TimeoutSec int
	Prompt     string
	OwnerID    int64 // 0 = 全局
	Enabled    bool
}

// RequiresKey 该类型是否需要 API Key。
func (p Provider) RequiresKey() bool {
	return p.Kind == KindOpenAI
}

// Describe 给界面用的一句话说明（谁提供、做什么）。
func (p Provider) Describe() string {
	switch p.Kind {
	case KindLogShare:
		return "第三方 AI 日志分析（公益合作，LogShare.CN）"
	case KindMclogs:
		return "日志分享链接（不做 AI 分析，用于去社区求助）"
	case KindOpenAI:
		who := p.Model
		if who == "" {
			who = "自定义平台"
		}
		return "自配平台（OpenAI 兼容）：" + who
	case KindBuiltinRules:
		return "面板内置规则诊断（离线可用）"
	}
	return p.Kind
}

// IsAI 该提供方是否给出 AI 结论（mclo.gs 不算 —— 它只给链接与错误行数）。
func (p Provider) IsAI() bool {
	return p.Kind == KindLogShare || p.Kind == KindOpenAI || p.Kind == KindBuiltinRules
}

// ChainEntry 提供方链里的一环。
type ChainEntry struct {
	Provider Provider
	// Reason 为什么排在这个位置（界面要能解释"为什么用了它/为什么跳过它"）
	Reason string
}

// BuildChain 按用户的偏好顺序排出这次要尝试的提供方。
//
// 设计要点（D3）：
//   - 默认顺序 logshare → mclogs → 用户自配平台；
//   - **顺序可配**（管理员在设置里改），但内置的两家始终作为兜底存在 ——
//     否则用户把顺序清空之后功能会静默变成"不可用"；
//   - 已经停用的提供方不进链，但**要在结果里说明**（否则用户会以为它坏了）。
func BuildChain(order []string, providers []Provider, wantProviderID int64) []ChainEntry {
	byID := map[int64]Provider{}
	byKind := map[string]Provider{}
	for _, p := range providers {
		byID[p.ID] = p
		if p.Kind == KindLogShare || p.Kind == KindMclogs || p.Kind == KindBuiltinRules {
			byKind[p.Kind] = p
		}
	}

	// 指定了具体提供方：只尝试它（用户明确的选择不该被"自动回退"掉，
	// 但失败时要说明可以改成自动）
	if wantProviderID > 0 {
		if p, ok := byID[wantProviderID]; ok {
			return []ChainEntry{{Provider: p, Reason: "用户指定的提供方"}}
		}
		return nil
	}

	var out []ChainEntry
	seen := map[string]bool{}
	add := func(p Provider, reason string) {
		key := fmt.Sprintf("%s/%d", p.Kind, p.ID)
		if seen[key] || !p.Enabled {
			return
		}
		seen[key] = true
		out = append(out, ChainEntry{Provider: p, Reason: reason})
	}

	for _, item := range order {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// 顺序项可以是 "logshare"/"mclogs" 这样的类型名，也可以是提供方 id
		switch item {
		case KindLogShare, KindMclogs, KindBuiltinRules:
			if p, ok := byKind[item]; ok {
				add(p, "按设置里的顺序")
			}
		default:
			var id int64
			if _, err := fmt.Sscanf(item, "%d", &id); err == nil {
				if p, ok := byID[id]; ok {
					add(p, "按设置里的顺序")
				}
			}
		}
	}

	// 兜底：配置里没提到的内置提供方补在最后（顺序只影响优先级，不影响可用性）
	if p, ok := byKind[KindLogShare]; ok {
		add(p, "默认提供方（未在顺序中指定）")
	}
	if p, ok := byKind[KindMclogs]; ok {
		add(p, "保底：AI 分析不可用时仍能把日志分享出去")
	}
	// 用户自配平台排在最后：它们消耗的是用户自己的额度，属于"更愿意用"的选项，
	// 但只有用户自己（或管理员）配了才有
	for _, p := range providers {
		if p.Kind == KindOpenAI {
			add(p, "自配平台")
		}
	}
	return out
}

// PreflightError 统一包装"这家提供方能不能用"的判定（配置不全、地址被拦等）。
//
// 放在链式尝试之前：这类错误**不该消耗一次限流额度**，也不该让链条继续往后走
// （配置错是用户要去改的，回退到别家只会让人以为"我配的平台没生效"）。
func PreflightError(ctx context.Context, p Provider, allowPrivate bool) error {
	switch p.Kind {
	case KindOpenAI:
		if strings.TrimSpace(p.BaseURL) == "" {
			return fmt.Errorf("「%s」未配置平台地址", p.Name)
		}
		if strings.TrimSpace(p.APIKey) == "" {
			return fmt.Errorf("「%s」未配置 API Key", p.Name)
		}
		if err := CheckOutboundURL(ctx, p.BaseURL, allowPrivate); err != nil {
			return fmt.Errorf("「%s」的地址不被允许：%w", p.Name, err)
		}
	}
	return nil
}
