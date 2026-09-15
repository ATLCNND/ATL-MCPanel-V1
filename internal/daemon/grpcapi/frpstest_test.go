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
