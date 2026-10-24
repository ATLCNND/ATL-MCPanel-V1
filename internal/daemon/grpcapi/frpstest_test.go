package grpcapi

import (
	"strings"
	"testing"
)

// frpc 的日志措辞随版本变，这里把几种真实句式钉住 ——
// 自检能不能给出正确结论全靠这个分类函数。
//
// 关键是要区分三态：**成功**、**有结论的失败**（可以立刻返回给用户）、
// **还没结论**（继续等，别过早下判断）。
func TestClassifyFrpcSelfTestLog(t *testing.T) {
	loginOK := "2026/09/15 21:00:00 [I] [service.go:295] try to connect to server...\n" +
		"2026/09/15 21:00:00 [I] [service.go:304] login to server success, get run id [abc123]\n"
	proxyOK := "2026/09/15 21:00:00 [I] [proxy_manager.go:145] [atlmcpanel-selftest-25600] start proxy success\n"

	cases := []struct {
		name         string
		log          string
		wantOK       bool
		wantDecisive bool
	}{
		{"登录 + 注册都成功", loginOK + proxyOK, true, true},
		{"只有登录成功（还没注册完）", loginOK, false, false},
		{"token 不符", "2026/09/15 21:00:00 [E] [service.go:311] login to server failed: " +
			"token in login doesn't match token from configuration\n", false, true},
		{"端口被占", loginOK + "2026/09/15 21:00:00 [E] [proxy_manager.go:150] " +
			"[t] start error: port already used\n", false, true},
		{"连不上 frps", "2026/09/15 21:00:00 [E] [service.go:298] connect to server error: " +
			"dial tcp 1.2.3.4:7000: connect: connection refused\n", false, true},
		{"域名解析不了", "2026/09/15 21:00:00 [E] [service.go:298] connect to server error: " +
			"dial tcp: lookup mc.example.com: no such host\n", false, true},
		{"登录失败（措辞未命中已知句式）", "2026/09/15 21:00:00 [E] login to server failed: EOF\n", false, true},
		{"还什么都没写", "", false, false},
		{"只有无关日志", "2026/09/15 21:00:00 [I] [root.go:250] start frpc service for config file\n", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, decisive, reason := classifyFrpcSelfTestLog(c.log)
			if ok != c.wantOK || decisive != c.wantDecisive {
				t.Fatalf("ok=%v decisive=%v（期望 %v/%v）reason=%q",
					ok, decisive, c.wantOK, c.wantDecisive, reason)
			}
			if c.wantDecisive && !c.wantOK && reason == "" {
				t.Error("有结论的失败必须给出原因（界面要显示它）")
			}
		})
	}
}

// 日志尾部要能截断（失败时整段回传会把 gRPC 消息撑大）。
func TestLogTail(t *testing.T) {
	if got := logTail("short", 100); got != "short" {
		t.Errorf("短的应原样返回，实际 %q", got)
	}
	long := ""
	for i := 0; i < 100; i++ {
		long += "0123456789"
	}
	got := logTail(long, 50)
	if len(got) > 54 { // 50 字节 + 省略号（UTF-8 3 字节）
		t.Errorf("应截断到 ~50 字节，实际 %d", len(got))
	}
	if !strings.HasPrefix(got, "…") {
		t.Errorf("截断后应有省略号前缀，实际 %q", got[:3])
	}
}

// 2026-10-21 真机案例（Lolia 线路）：服务端在登录阶段回中文错误，
// frpc 把它包在 "connect to server error: …" 里。
// 旧逻辑会把它报成「连不上 frps（地址/端口/防火墙）」，用户于是去查网络 ——
// 而真相是**连上了、被服务端拒绝了**。这条用例把区别钉死。
func TestClassifyServerRefusalIsNotNetworkError(t *testing.T) {
	log := "2026-10-21 18:43:26.996 [I] [sub/root.go:142] start frpc service for config file [/tmp/x/frpc.toml]\n" +
		"2026-10-21 18:43:26.997 [I] [client/service.go:295] try to connect to server...\n" +
		"2026-10-21 18:43:27.149 [E] [client/service.go:280] FRPC 配置文件错误,请检查后重试,请反馈给管理员以解决这个问题\n" +
		"2026-10-21 18:43:27.150 [W] [client/service.go:298] connect to server error: " +
		"FRPC 配置文件错误,请检查后重试,请反馈给管理员以解决这个问题\n"
	ok, decisive, reason := classifyFrpcSelfTestLog(log)
	if ok || !decisive {
		t.Fatalf("应当有结论地失败，实际 ok=%v decisive=%v", ok, decisive)
	}
	// 必须带上服务端原话
	if !strings.Contains(reason, "配置文件错误") {
		t.Errorf("应原样带出服务端消息，实际 %q", reason)
	}
	// 必须说清「不是网络问题」这一层
	if strings.Contains(reason, "连不上 frps（地址/端口/防火墙）") {
		t.Errorf("不该再把服务端拒绝说成网络不通，实际 %q", reason)
	}
	if !strings.Contains(reason, "拒绝") {
		t.Errorf("应明确指出是服务端拒绝，实际 %q", reason)
	}
}

// 纯网络失败（没有服务端消息）仍按原样报「连不上」，别被上面那条改坏。
func TestClassifyNetworkErrorStillReported(t *testing.T) {
	log := "2026-10-21 18:00:00.000 [W] [client/service.go:298] connect to server error: dial tcp 1.2.3.4:7000: connect: connection refused\n"
	ok, decisive, reason := classifyFrpcSelfTestLog(log)
	if ok || !decisive {
		t.Fatalf("应是有结论的失败，实际 ok=%v decisive=%v", ok, decisive)
	}
	if !strings.Contains(reason, "拒绝") && !strings.Contains(reason, "连不上") {
		t.Errorf("原因应可读，实际 %q", reason)
	}
}
