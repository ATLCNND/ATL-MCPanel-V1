package analysis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// ValidateURLSyntax 保存配置时的**快速**校验：只做语法与"字面 IP"判断，不解析 DNS。
//
// 为什么不在这里解析域名：保存一个配置不该依赖"此刻 DNS 通不通"——
// 网络抖动、域名还没解析好、面板所在网络看不到对方 DNS，都会让用户存不下去；
// 而真正的防护发生在**发请求时**（PreflightError + CheckOutboundURL +
// 重定向每跳检查），那里 DNS 一定是通的。
//
// 但字面 IP（http://10.1.2.3/、http://169.254.169.254/）必须现在拦：
// 它们不需要解析，而且正是最典型的"想借面板去打内网"的写法。
func ValidateURLSyntax(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("地址不合法：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("地址缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		reason, class := classifyIP(ip)
		if class == ipClassNever {
			return fmt.Errorf("%s %s", host, reason)
		}
		if class == ipClassPrivate && !allowPrivate {
			return fmt.Errorf("%s 是%s；自建网关确实在内网时，请管理员在「分析平台」设置里打开“允许内网地址”", host, reason)
		}
	}
	return nil
}

// CheckOutboundURL 校验"面板即将带着用户的 key 去请求"的这个地址是否允许。
//
// 为什么必须做（这是 D2 的决定）：`base_url` 是**用户可控**的 URL，而请求由**面板**发出、
// 并且带着 `Authorization: Bearer <key>`。不加限制就等于：
//
//	任何人都能让面板向内网任何地址发请求，并把响应内容读回来（SSRF），
//	顺带把 key 送到攻击者指定的地方。
//
// 规则：
//   - 只允许 http/https；
//   - **永远拦下**：链路本地（169.254.0.0/16 —— 云上元数据服务就在这个网段，
//     读走它等于拿走机器凭据）、组播、未指定地址；
//   - **默认拦下、管理员可放行**：私有网段与回环地址。
//     自建网关（vLLM / LiteLLM / One-API…）常常就在内网甚至面板本机，
//     这是它存在的唯一理由 —— 所以做成"默认关、可显式开"而不是"永远禁"。
//
// 注意这里**解析域名**而不是只做字符串匹配：`http://127.0.0.1.nip.io` 这种
// 域名会解析到 127.0.0.1，只看字符串是拦不住的。
func CheckOutboundURL(ctx context.Context, raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("地址不合法：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("地址缺少主机名")
	}
	// 直接给 IP 或域名都解析一遍，统一按解析结果判断
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("无法解析主机 %q（%v）", host, err)
	}
	for _, ip := range ips {
		reason, class := classifyIP(ip.IP)
		if class == ipClassNever {
			return fmt.Errorf("%s（%s）%s", host, ip.IP, reason)
		}
		if class == ipClassPrivate && !allowPrivate {
			return fmt.Errorf("%s（%s）%s；自建网关确实在内网时，请管理员在「分析平台」设置里打开“允许内网地址”",
				host, ip.IP, reason)
		}
	}
	return nil
}

// IP 的放行类别。
type ipClass int

const (
	ipClassOK      ipClass = iota // 公网地址，放行
	ipClassPrivate                // 私有网段/回环：默认拦，管理员可放行
	ipClassNever                  // 无论如何都拦（云元数据等）
)

// classifyIP 给地址分类。
//
// 优先级刻意是"先判永远拦、再判可放行"：169.254.0.0/16 里的云元数据服务
// 与"自建网关"没有任何关系，把它一起放行等于给了一次"管理员开内网 ->
// 顺手把机器凭据交给攻击者"的机会。
func classifyIP(ip net.IP) (string, ipClass) {
	switch {
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return "链路本地地址（云元数据服务就在这个网段）", ipClassNever
	case ip.IsMulticast() || ip.IsInterfaceLocalMulticast():
		return "组播地址", ipClassNever
	case ip.IsUnspecified():
		return "未指定地址", ipClassNever
	case ip.IsLoopback():
		return "回环地址", ipClassPrivate
	case ip.IsPrivate():
		return "私有网段地址", ipClassPrivate
	}
	return "", ipClassOK
}

// RedirectGuard 返回一个 CheckRedirect 回调，让**每一跳**都过一遍上面的检查。
//
// 为什么不能只检查首个 URL：`https://evil.example` 可以 302 到 `http://169.254.169.254/`，
// Go 的 http.Client 默认会**带着 Authorization 头跟着跳**（同源才带，但 302 到别的
// 主机时 Go 会去掉敏感头 —— 仍不该让请求打出去：那台内网机器会收到我们的探测）。
func RedirectGuard(allowPrivate bool) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("重定向次数过多")
		}
		if err := CheckOutboundURL(req.Context(), req.URL.String(), allowPrivate); err != nil {
			return fmt.Errorf("重定向目标被拒绝：%w", err)
		}
		return nil
	}
}
