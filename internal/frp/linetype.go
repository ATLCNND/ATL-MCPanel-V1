package frp

import (
	"fmt"
	"strings"
)

// 线路类型（底账 F16）。
//
// 三种出网方式在**面板**层面是同一件事（一条线路 + 若干隧道），但行为差别很大：
//
//	self_hosted  自建 frps：有端口段，面板从段里分配 remote_port，节点上跑 frpc。
//	third_party  第三方单隧道：服务商给一条固定隧道（远端端口或子域），没端口段，
//	             值来自用户导入的配置，节点上仍然跑 frpc。
//	direct       公网直出：节点自己有公网 IP，**完全不跑 frpc** ——
//	             地址就是「线路主机:本地端口」，端口由实例自己监听后直接对外。
//
// 为什么必须是一个显式枚举而不是"有没有配置"推断出来的：三种类型的**约束不一样**
// （要不要端口段、能不能分配端口、节点上要不要起 frpc），靠推断迟早出现
// "线路说直连、端口却在分配"这种自相矛盾的状态。
const (
	// LineSelfHosted 自建 frps（现有行为）。
	LineSelfHosted = "self_hosted"
	// LineThirdParty 第三方单隧道服务商。
	LineThirdParty = "third_party"
	// LineDirect 公网直出（节点自己有公网 IP，不走穿透）。
	LineDirect = "direct"
)

// NormalizeLineType 把用户输入规整成合法类型；空值按自建处理（兼容老数据）。
func NormalizeLineType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", LineSelfHosted:
		return LineSelfHosted
	case LineThirdParty, "third-party", "thirdparty":
		return LineThirdParty
	case LineDirect, "public_direct", "public-direct", "direct_public":
		return LineDirect
	default:
		return ""
	}
}

// ValidateLineType 校验并返回规范化后的类型，非法时给出可读原因。
func ValidateLineType(s string) (string, error) {
	t := NormalizeLineType(s)
	if t == "" {
		return "", fmt.Errorf("未知的线路类型 %q（只支持 self_hosted / third_party / direct）", s)
	}
	return t, nil
}

// LineTypeLabel 给界面/日志用的中文名。
func LineTypeLabel(t string) string {
	switch NormalizeLineType(t) {
	case LineThirdParty:
		return "第三方单隧道"
	case LineDirect:
		return "公网直出"
	default:
		return "自建 frps"
	}
}

// LineNeedsPortSegment 该类型是否要求端口段（只有自建 frps 需要）。
func LineNeedsPortSegment(t string) bool {
	return NormalizeLineType(t) == LineSelfHosted
}

// LineRunsFrpc 该类型的隧道是否需要节点上跑 frpc。
//
// 直连不需要：实例自己监听端口，玩家直连「主机:端口」。
// 这一条是**行为分叉点** —— 面板下发隧道时据此决定要不要调 daemon 的 ApplyTunnel。
func LineRunsFrpc(t string) bool {
	return NormalizeLineType(t) != LineDirect
}

// LineAllocatesPort 该类型是否由面板分配 remote_port。
//
// 自建 frps 要分配；第三方与直连都不分配 ——
// 前者的远端端口是服务商给的（导入时带进来），后者的"远端"就是本地端口本身。
func LineAllocatesPort(t string) bool {
	return NormalizeLineType(t) == LineSelfHosted
}
