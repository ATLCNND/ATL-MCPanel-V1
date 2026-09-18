// Package portguard 拦住"把节点自己的服务挂到公网"这类穿透目标。
//
// 背景（实测）：面板建公网端口时只校验 `local_port > 0`，Daemon 的 ApplyTunnel
// 完全透传，frp 生成配置时固定写 `localAddr = "127.0.0.1:<local_port>"`。
// 于是**实例 owner 可以把节点的 22(SSH) / 9090(面板 gRPC) / 9091(Daemon gRPC) /
// 7400(frps 管理口) 挂到公网**，而 frpc 还是以 root 运行的 —— 一个本来
// 只在实例里活动的租户，因此获得了一条对节点的**公开入口**。
//
// 这里放的是"哪些本地端口不许当穿透目标"这一条判断，面板与 Daemon 都用它：
// 面板是入口校验（给人看的提示），Daemon 是最后一道（不能假设调用方一定是自家面板）。
package portguard

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// alwaysDenied 与"平台自己用不用"无关、任何情况下都不该挂公网的端口。
//
// 22/SSH 是其中最要紧的一个：节点用口令登录，把它挂到公网等于把爆破面
// 从"内网"搬到"全网"。其余几个是 frp 与常见反代的默认端口。
var alwaysDenied = map[int]string{
	22:   "SSH 登录端口（挂到公网等于把节点登录入口暴露给全网）",
	25:   "邮件服务端口",
	53:   "DNS 端口",
	111:  "rpcbind 端口",
	7000: "frp 服务端默认端口",
	7400: "frp 管理接口默认端口",
	3306: "数据库默认端口",
	5432: "数据库默认端口",
	6379: "Redis 默认端口",
	8080: "面板默认 HTTP 端口",
	8443: "面板默认 HTTPS 端口",
	9090: "面板 gRPC 默认端口",
	9091: "Daemon gRPC 默认端口",
}

// Reason 返回该本地端口被拒绝的理由；允许时返回空字符串。
//
// extra 是**本机实际配置**的端口（面板会把自己监听的端口放进来，Daemon 同理）：
// 默认清单只能覆盖默认值，换了端口的部署必须靠它兜住。
// extra 里的理由为空时用一句通用说明。
func Reason(port int, extra map[int]string) string {
	if port <= 0 || port > 65535 {
		return fmt.Sprintf("端口 %d 不是合法端口（1-65535）", port)
	}
	if why, ok := extra[port]; ok {
		if why == "" {
			why = "平台自身服务占用的端口"
		}
		return why
	}
	if why, ok := alwaysDenied[port]; ok {
		return why
	}
	// 特权端口（<1024）需要 root 才能监听，能落在这个区间的都是系统服务。
	// 实例服务端不该用它们（MC 默认 25565），所以整段拦掉最省心：
	// 逐个列举系统服务是列不全的，而"漏一个"的代价是暴露一个系统服务。
	if port < 1024 {
		return "系统特权端口（需要 root 才能监听的服务都在这个区间）"
	}
	return ""
}

// Check 组合 Reason 的常用形式。
func Check(port int, extra map[int]string) error {
	if why := Reason(port, extra); why != "" {
		return fmt.Errorf("本地端口 %d 不能作为穿透目标：%s", port, why)
	}
	return nil
}

// FromListenAddr 从监听地址里取出端口（":8080"、"127.0.0.1:9091"、"0.0.0.0:8443"）。
// 解析不出时返回 0（调用方忽略即可）。
func FromListenAddr(addr string) int {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		// 兼容只写了端口的情况："8080"
		portStr = strings.TrimPrefix(addr, ":")
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

// Set 把若干监听地址收进 extra 表（面板/Daemon 用它声明"我自己占着哪些端口"）。
func Set(extra map[int]string, why string, addrs ...string) {
	for _, a := range addrs {
		if p := FromListenAddr(a); p > 0 {
			if _, exists := extra[p]; !exists {
				extra[p] = why
			}
		}
	}
}
