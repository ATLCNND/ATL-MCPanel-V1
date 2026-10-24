package frp

import (
	"fmt"
	"sort"
	"strings"
)

// 第三方 frpc 配置的**导出**（底账 F15 的第二半：把面板里的线路还原成标准配置，供复制/交付）。
//
// 与 renderConfig 的区别（两个函数名字像、用途完全不同，别混）：
//   · renderConfig 是给**本机 daemon 上的 frpc 进程**用的：带日志路径、带证书路径、
//     带 https2http 插件段 —— 那些都是节点上的绝对路径，交给别人毫无意义；
//   · RenderStandardConfig 是给**人**看的/复制的：只保留服务端与隧道本身，
//     形状与第三方服务商给的配置一致，用户可以拿去比对、备份、或交给另一台机器。
//
// 输出风格选择**点号键 + 双引号**（auth.token），因为：
//   ① 这是 frp 官方文档与多数服务商的写法；
//   ② 本仓库 renderConfig 也是这个风格，导入侧（ImportConfig）已经能吃 ——
//      于是可以立一条**往返不变量**：导出再导入，关键字段必须一模一样（见 exportcfg_test.go）。

// ExportOptions 导出时的可选项。
type ExportOptions struct {
	// IncludeToken 是否把 token 写进文本。
	//
	// 默认应当为 true（这个功能存在的意义就是「能直接拿去用」），但调用方要清楚：
	// 导出的文本**含凭据**，界面上必须提示，且不要顺手贴到公开的地方。
	IncludeToken bool
	// UseLoliaTokenStyle 用 Lolia 那家的写法（[metadatas] token 表）而不是 auth.token。
	//
	// 为什么留这个开关：有的服务商/客户端只认自己那套写法，
	// 用户复制过去能直接用，比「格式正确」更重要。
	UseLoliaTokenStyle bool
	// Header 可选的自定义注释头（例如面板名与导出时间）。
	Header string
	// User frp 的 user 字段（第三方线路的租户标识，Lolia 这类必须写）。
	User string
	// 传输层 TLS（三态字符串，空 = 不输出）
	TLSEnable                 string
	TLSDisableCustomFirstByte string
}

// RenderStandardConfig 把一条线路及其隧道渲染成标准 frpc TOML。
func RenderStandardConfig(server Server, tunnels []Tunnel, opts ExportOptions) (string, error) {
	if strings.TrimSpace(server.Host) == "" {
		return "", fmt.Errorf("线路缺少服务端地址")
	}
	if server.BindPort <= 0 {
		return "", fmt.Errorf("线路缺少服务端端口")
	}
	if len(tunnels) == 0 {
		return "", fmt.Errorf("这条线路上还没有隧道，没有可导出的内容")
	}

	var sb strings.Builder
	if opts.Header != "" {
		for _, line := range strings.Split(strings.TrimRight(opts.Header, "\n"), "\n") {
			fmt.Fprintf(&sb, "# %s\n", line)
		}
	}
	fmt.Fprintf(&sb, "serverAddr = %q\n", server.Host)
	fmt.Fprintf(&sb, "serverPort = %d\n", server.BindPort)
	if opts.User != "" {
		fmt.Fprintf(&sb, "user = %q\n", opts.User)
	}
	// 传输层 TLS：三态，只有导出对象里写过才输出（沿用隧道上的取值）
	if opts.TLSEnable != "" {
		fmt.Fprintf(&sb, "transport.tls.enable = %s\n", opts.TLSEnable)
	}
	if opts.TLSDisableCustomFirstByte != "" {
		fmt.Fprintf(&sb, "transport.tls.disableCustomTLSFirstByte = %s\n", opts.TLSDisableCustomFirstByte)
	}
	if opts.IncludeToken && server.Token != "" {
		if opts.UseLoliaTokenStyle {
			sb.WriteString("\n[metadatas]\n")
			fmt.Fprintf(&sb, "token = %q\n", server.Token)
		} else {
			sb.WriteString("auth.method = \"token\"\n")
			fmt.Fprintf(&sb, "auth.token = %q\n", server.Token)
		}
	}

	// 排序后再输出：同一份数据每次导出应得到**完全一样**的文本，
	// 否则用户没法用 diff 比对两次导出（「我改了什么」应该一眼看得出）。
	ordered := make([]Tunnel, len(tunnels))
	copy(ordered, tunnels)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TunnelID != ordered[j].TunnelID {
			return ordered[i].TunnelID < ordered[j].TunnelID
		}
		return ordered[i].LocalPort < ordered[j].LocalPort
	})

	for _, t := range ordered {
		proto := strings.ToLower(strings.TrimSpace(t.Protocol))
		if proto == "" {
			proto = "tcp"
		}
		sb.WriteString("\n[[proxies]]\n")
		// 名字优先用用户的隧道名；没有就退回 tunnel_id（frpc 只允许 ASCII 安全字符）
		name := t.Name
		if strings.TrimSpace(name) == "" {
			name = t.TunnelID
		}
		fmt.Fprintf(&sb, "name = %q\n", sanitizeProxyName(name))
		fmt.Fprintf(&sb, "type = %q\n", proto)

		switch proto {
		case "http", "https":
			if len(t.CustomDomains) > 0 {
				fmt.Fprintf(&sb, "customDomains = [%s]\n", quoteList(t.CustomDomains))
			} else if t.Subdomain != "" {
				fmt.Fprintf(&sb, "subdomain = %q\n", t.Subdomain)
			} else {
				return "", fmt.Errorf("隧道 %s 是 %s，但没有域名（subdomain / customDomains）", t.TunnelID, proto)
			}
			fmt.Fprintf(&sb, "localPort = %d\n", t.LocalPort)
		case "tcp", "udp":
			if t.RemotePort <= 0 {
				return "", fmt.Errorf("隧道 %s 是 %s，但没有远端端口", t.TunnelID, proto)
			}
			sb.WriteString("localIP = \"127.0.0.1\"\n")
			fmt.Fprintf(&sb, "localPort = %d\n", t.LocalPort)
			fmt.Fprintf(&sb, "remotePort = %d\n", t.RemotePort)
		default:
			return "", fmt.Errorf("隧道 %s 的协议 %q 不支持导出（只支持 tcp/udp/http/https）", t.TunnelID, proto)
		}

		// 服务商的传输参数（Lolia 这类缺了就登不上/起不来），导出要原样带上
		if t.BandwidthLimit != "" || t.BandwidthLimitMode != "" {
			sb.WriteString("\n[proxies.transport]\n")
			if t.BandwidthLimit != "" {
				fmt.Fprintf(&sb, "bandwidthLimit = %q\n", t.BandwidthLimit)
			}
			if t.BandwidthLimitMode != "" {
				fmt.Fprintf(&sb, "bandwidthLimitMode = %q\n", t.BandwidthLimitMode)
			}
		}
	}
	return sb.String(), nil
}

// MaskToken 把 token 打码成「前 4 位…后 4 位」，供界面/日志展示。
//
// 为什么打码而不是不显示：用户需要能**认出来**自己粘的是哪一份（手上常有多家服务商），
// 但完整回显等于把凭据写进截图、工单与聊天记录。留首尾各 4 位是「够认出、不够用」的折中。
func MaskToken(tok string) string {
	if tok == "" {
		return ""
	}
	if len(tok) <= 8 {
		return strings.Repeat("*", len(tok))
	}
	return tok[:4] + "…" + tok[len(tok)-4:]
}
