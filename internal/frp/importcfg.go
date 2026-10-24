package frp

import (
	"fmt"
	"strconv"
	"strings"
)

// 第三方 frpc 配置的**导入**（底账 F15 的第一半：解析）。
//
// 为什么手写而不是引 TOML 库：
//   · 这个仓库生成 frpc 配置本来就是手写 TOML（见 renderConfig，用的是点号键
//     `auth.token = "…"`）——导入侧与生成侧用同一套形状，读代码的人只需要理解一种风格；
//   · 引一个 TOML 库就等于多一个第三方依赖，还要连带更新 THIRD-PARTY-NOTICES.md
//     （Apache-2.0/MIT 的再分发义务），为了一份**只读几十个键**的配置不值得。
//
// 但要认清代价：这不是一个完整的 TOML 解析器。它只认 frpc 配置实际会出现的形状 ——
// 表头 [a.b] / 数组表 [[a]] / 点号键 a.b = v / 字符串（单双引号）/ 整数 / 布尔 /
// 字符串数组。**遇到不认识的键不报错、只记 warning**：服务商随时会加新选项，
// 我们不认识的键不该让用户没法导入。

// ImportedTLS 传输层 TLS 相关开关（第三方服务商常有 useTLS / useEncryption 这类选项）。
type ImportedTLS struct {
	// Enable transport.tls.enable
	Enable bool
	// DisableCustomTLSFirstByte transport.tls.disableCustomTLSFirstByte
	DisableCustomTLSFirstByte bool
}

// ImportedProxy 一条隧道（frpc 的 [[proxies]]）。
type ImportedProxy struct {
	Name               string
	Type               string // tcp / udp / http / https
	LocalIP            string
	LocalPort          int
	RemotePort         int
	Subdomain          string
	CustomDomains      []string
	BandwidthLimit     string
	BandwidthLimitMode string
}

// ImportedConfig 从一份 frpc 配置里解析出来的结果。
type ImportedConfig struct {
	ServerAddr string
	ServerPort int
	// User frp v0.52+ 的 user 字段（HTTP Basic 用户名，Lolia 用它区分租户）
	User string
	// Token 历史上漂过三个位置：auth.token（现行）/ [metadatas] token（Lolia）/ 老式顶层 token
	Token string
	// TokenField 记下 token **出自哪个位置**：「auth」/「metadatas」/「top」。
	//
	// 为什么必须记住：这不只是格式问题 —— 服务端只认自己那个位置。
	// 实测对 Lolia：只发 auth.token 会被拒（回「FRPC 配置文件错误」），
	// 必须发 metadatas.token；而对 SakuraFRP 这类则要 auth.token。
	// 导入时记下来，运行时才能按原样还原。
	TokenField string
	TLS     ImportedTLS
	Proxies []ImportedProxy
	// Warnings 解析过程中遇到的、不影响导入但值得告知用户的情况（不认识的键等）
	Warnings []string
}

// 解析状态机用到的「当前位于哪张表」。
const (
	scopeRoot       = iota
	scopeProxy      // 位于 [[proxies]] 里
	scopeProxyTrans // 位于 [proxies.transport] 里
	scopeMetadatas  // 位于 [metadatas] 里（Lolia 的 token 在这里）
)

// ImportConfig 解析 frpc 的 TOML 配置。
//
// 返回的 error 只用于**致命**问题（不是合法配置、缺少必需项）；
// 其它情况（不认识的键、暂时没用上的选项）走 Warnings。
func ImportConfig(text string) (*ImportedConfig, error) {
	cfg := &ImportedConfig{ServerPort: 0}
	scope := scopeRoot
	var cur *ImportedProxy

	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		lineNo := i + 1
		line := stripTomlComment(raw)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// ---- 表头 ----
		if strings.HasPrefix(line, "[[") {
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
			switch inner {
			case "proxies":
				cfg.Proxies = append(cfg.Proxies, ImportedProxy{})
				cur = &cfg.Proxies[len(cfg.Proxies)-1]
				scope = scopeProxy
			default:
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("第 %d 行：不认识的数组表 [[%s]]，已忽略", lineNo, inner))
				cur = nil
				scope = scopeRoot
			}
			continue
		}
		if strings.HasPrefix(line, "[") {
			inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			switch {
			case inner == "metadatas":
				scope = scopeMetadatas
			case inner == "proxies.transport":
				if cur == nil {
					return nil, fmt.Errorf("第 %d 行：[proxies.transport] 必须出现在 [[proxies]] 之后", lineNo)
				}
				scope = scopeProxyTrans
			case strings.HasPrefix(inner, "proxies.plugin"):
				// 插件段（https2http 等）：导入不还原插件，记一条提示
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("第 %d 行：[%s] 插件配置不会被导入（面板只支持端口/域名方式的隧道）", lineNo, inner))
				scope = scopeProxyTrans
			default:
				cfg.Warnings = append(cfg.Warnings,
					fmt.Sprintf("第 %d 行：不认识的表 [%s]，已忽略", lineNo, inner))
				scope = scopeRoot
			}
			continue
		}

		// ---- 键值 ----
		//
		// ⚠️ 这一行"不是键值"时**不能直接失败**：第三方服务商给的配置经常带一行标题
		// （真实样例里就有 "Lolia FRP配置：" 与 "SakuraFRP" 这种裸行），
		// 用户是从网页/聊天里复制来的，带标语是常态。所以这类噪行只记 warning，
		// 由用户自己判断要不要在意；真正致命的错误（缺 serverAddr、端口不合法等）才报错。
		eq := indexOutsideQuotes(line, '=')
		if eq < 0 {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("第 %d 行不是键值、已跳过：%q", lineNo, strings.TrimSpace(raw)))
			continue
		}
		key := strings.TrimSpace(line[:eq])
		valText := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("第 %d 行缺少键名", lineNo)
		}

		val, err := parseTomlValue(valText)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行 %s 的值不合法：%v", lineNo, key, err)
		}

		// 键可能带点号（transport.tls.enable），也可能落在某张表里 —— 拼成完整路径
		full := key
		switch scope {
		case scopeMetadatas:
			full = "metadatas." + key
		case scopeProxyTrans:
			full = "proxies.transport." + key
		}

		if err := applyKey(cfg, cur, scope, full, val, lineNo); err != nil {
			return nil, err
		}
	}

	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("配置里没有 serverAddr（这不是一份 frpc 配置？）")
	}
	if cfg.ServerPort == 0 {
		return nil, fmt.Errorf("配置里没有 serverPort")
	}
	if len(cfg.Proxies) == 0 {
		return nil, fmt.Errorf("配置里没有 [[proxies]]，没有可导入的隧道")
	}
	for idx := range cfg.Proxies {
		p := &cfg.Proxies[idx]
		if p.Type == "" {
			p.Type = "tcp" // frpc 的默认值
		}
		if p.LocalPort == 0 {
			return nil, fmt.Errorf("隧道 %s 缺少 localPort", proxyLabel(p, idx))
		}
		switch p.Type {
		case "tcp", "udp":
			if p.RemotePort == 0 {
				return nil, fmt.Errorf("隧道 %s 是 %s，但没有 remotePort", proxyLabel(p, idx), p.Type)
			}
			if p.Subdomain != "" || len(p.CustomDomains) > 0 {
				return nil, fmt.Errorf("隧道 %s 是 %s，却给了 subdomain/customDomains（端口与域名只能二选一）",
					proxyLabel(p, idx), p.Type)
			}
		case "http", "https":
			if p.Subdomain == "" && len(p.CustomDomains) == 0 {
				return nil, fmt.Errorf("隧道 %s 是 %s，但没有 subdomain 或 customDomains", proxyLabel(p, idx), p.Type)
			}
			if p.RemotePort != 0 {
				return nil, fmt.Errorf("隧道 %s 是 %s，却给了 remotePort（端口与域名只能二选一）",
					proxyLabel(p, idx), p.Type)
			}
		default:
			return nil, fmt.Errorf("隧道 %s 的 type=%q 不支持（只支持 tcp/udp/http/https）",
				proxyLabel(p, idx), p.Type)
		}
	}
	return cfg, nil
}

func proxyLabel(p *ImportedProxy, idx int) string {
	if p.Name != "" {
		return p.Name
	}
	return fmt.Sprintf("#%d", idx+1)
}

// applyKey 把一条键值落到结构上。
//
// 单独拆出来是因为「同一个键在不同位置含义不同」（port 在顶层的 proxies 里、
// token 可能在 metadatas 里），集中在一处比散在解析循环里更容易核对。
func applyKey(cfg *ImportedConfig, cur *ImportedProxy, scope int, full string, val any, lineNo int) error {
	switch full {
	// ---- 服务端 ----
	case "serverAddr":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("第 %d 行：serverAddr 应该是字符串", lineNo)
		}
		cfg.ServerAddr = s
	case "serverPort":
		n, ok := val.(int)
		if !ok || n <= 0 || n > 65535 {
			return fmt.Errorf("第 %d 行：serverPort 应该是 1-65535 的整数", lineNo)
		}
		cfg.ServerPort = n
	case "user":
		if s, ok := val.(string); ok {
			cfg.User = s
		}
	// ---- token 的三个历史位置 ----
	case "token", "auth.token", "metadatas.token":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("第 %d 行：%s 应该是字符串", lineNo, full)
		}
		if cfg.Token == "" {
			// 同一份文件里可能有多个位置都写了 token（服务商互相抄），取先出现的
			cfg.Token = s
			switch full {
			case "metadatas.token":
				cfg.TokenField = "metadatas"
			case "auth.token":
				cfg.TokenField = "auth"
			default:
				cfg.TokenField = "top"
			}
		} else if cfg.Token != s {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("第 %d 行：%s 与前面出现的 token 不一致，已忽略这一处", lineNo, full))
		}
	case "auth.method":
		if s, ok := val.(string); ok && s != "token" {
			cfg.Warnings = append(cfg.Warnings,
				fmt.Sprintf("第 %d 行：auth.method=%q 不是 token，面板只支持 token 认证", lineNo, s))
		}
	// ---- 传输层 TLS ----
	case "transport.tls.enable", "proxies.transport.tls.enable":
		cfg.TLS.Enable = asBool(val)
	case "transport.tls.disableCustomTLSFirstByte", "proxies.transport.tls.disableCustomTLSFirstByte":
		cfg.TLS.DisableCustomTLSFirstByte = asBool(val)
	// ---- 隧道（必须在 [[proxies]] 里）----
	case "name", "type", "localIP", "localPort", "remotePort", "subdomain", "customDomains",
		"proxies.transport.bandwidthLimit", "proxies.transport.bandwidthLimitMode",
		"proxies.transport.proxyProtocolVersion", "proxies.transport.useEncryption", "proxies.transport.useCompression":
		if cur == nil {
			return fmt.Errorf("第 %d 行：%s 必须写在 [[proxies]] 里面", lineNo, full)
		}
		return applyProxyKey(cur, full, val, lineNo)
	default:
		cfg.Warnings = append(cfg.Warnings,
			fmt.Sprintf("第 %d 行：不认识的配置项 %s，已忽略（不影响导入）", lineNo, full))
	}
	return nil
}

func applyProxyKey(p *ImportedProxy, key string, val any, lineNo int) error {
	switch key {
	case "name":
		p.Name = asString(val)
	case "type":
		p.Type = strings.ToLower(asString(val))
	case "localIP":
		p.LocalIP = asString(val)
	case "localPort":
		n, ok := val.(int)
		if !ok || n <= 0 || n > 65535 {
			return fmt.Errorf("第 %d 行：localPort 应该是 1-65535 的整数", lineNo)
		}
		p.LocalPort = n
	case "remotePort":
		n, ok := val.(int)
		if !ok || n <= 0 || n > 65535 {
			return fmt.Errorf("第 %d 行：remotePort 应该是 1-65535 的整数", lineNo)
		}
		p.RemotePort = n
	case "subdomain":
		p.Subdomain = asString(val)
	case "customDomains":
		arr, ok := val.([]string)
		if !ok {
			return fmt.Errorf("第 %d 行：customDomains 应该是字符串数组", lineNo)
		}
		p.CustomDomains = arr
	case "proxies.transport.bandwidthLimit":
		p.BandwidthLimit = asString(val)
	case "proxies.transport.bandwidthLimitMode":
		p.BandwidthLimitMode = asString(val)
	case "proxies.transport.useEncryption", "proxies.transport.useCompression", "proxies.transport.proxyProtocolVersion":
		// 这些选项面板目前没有对应的字段，导入时忽略（不报错），但要提示一句
		return nil
	}
	return nil
}

// ---- 小工具：TOML 词法 ----

// stripTomlComment 去掉行尾注释（引号里的 # 不算）。
func stripTomlComment(s string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inDouble:
			i++ // 跳过被转义的字符
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			return s[:i]
		}
	}
	return s
}

// indexOutsideQuotes 找第一个不在引号里的目标字符。
func indexOutsideQuotes(s string, target byte) int {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inDouble:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == target && !inSingle && !inDouble:
			return i
		}
	}
	return -1
}

// parseTomlValue 解析一个 TOML 值：字符串（单/双引号）、整数、布尔、字符串数组。
func parseTomlValue(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("值是空的")
	}
	// 字符串数组
	if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("数组没有闭合")
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []string{}, nil
		}
		out := []string{}
		for _, part := range splitOutsideQuotes(inner, ',') {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			v, err := parseTomlValue(p)
			if err != nil {
				return nil, err
			}
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("数组元素必须是字符串")
			}
			out = append(out, str)
		}
		return out, nil
	}
	// 单引号字面量（Lolia 用的这种：内部不做转义）
	if strings.HasPrefix(s, "'") {
		if len(s) < 2 || !strings.HasSuffix(s, "'") {
			return nil, fmt.Errorf("单引号字符串没有闭合")
		}
		return s[1 : len(s)-1], nil
	}
	// 双引号基本字符串（支持 \" \\ \n \t 等常见转义）
	if strings.HasPrefix(s, "\"") {
		if len(s) < 2 || !strings.HasSuffix(s, "\"") {
			return nil, fmt.Errorf("双引号字符串没有闭合")
		}
		body := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] == '\\' && i+1 < len(body) {
				i++
				switch body[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(body[i])
				}
				continue
			}
			b.WriteByte(body[i])
		}
		return b.String(), nil
	}
	// 布尔
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	// 整数
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	return nil, fmt.Errorf("无法识别的值 %q", s)
}

// splitOutsideQuotes 按分隔符切分，但忽略引号内的分隔符（数组元素里可能有逗号）。
func splitOutsideQuotes(s string, sep byte) []string {
	var out []string
	inSingle, inDouble := false, false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inDouble:
			i++
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == sep && !inSingle && !inDouble:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}
