package portguard

import "testing"

// 这一组测试锁住的是"哪些端口不许当穿透目标"。案例全部来自实测：
// 在本修复之前，以实例身份调用 ApplyTunnel(local_port=22) 会返回 success=true，
// 生成的 frpc 配置里就是 localAddr = "127.0.0.1:22"。

func TestReason_DeniesPlatformAndPrivilegedPorts(t *testing.T) {
	bad := []int{
		22,   // SSH：最要紧的一个
		25,   // 邮件
		443,  // 特权端口区间
		7000, // frps
		7400, // frps 管理口
		8080, // 面板 HTTP 默认
		8443, // 面板 HTTPS 默认
		9090, // 面板 gRPC 默认
		9091, // Daemon gRPC 默认
		3306,
	}
	for _, p := range bad {
		if why := Reason(p, nil); why == "" {
			t.Errorf("端口 %d 应被拒绝，实际放行", p)
		}
	}
}

func TestReason_AllowsGameAndAppPorts(t *testing.T) {
	// 实例自己的游戏端口、以及插件常用的附加端口（语音、网页地图）都必须放行，
	// 否则这个检查会挡住正常用法，最后被人整个关掉。
	for _, p := range []int{25565, 25566, 8123, 24454, 19132, 30000, 65535} {
		if why := Reason(p, nil); why != "" {
			t.Errorf("端口 %d 不该被拒绝：%s", p, why)
		}
	}
}

func TestReason_InvalidPorts(t *testing.T) {
	for _, p := range []int{0, -1, 65536} {
		if why := Reason(p, nil); why == "" {
			t.Errorf("端口 %d 应被拒绝", p)
		}
	}
}

func TestReason_ExtraOverridesDefaultAllow(t *testing.T) {
	// 换了端口的部署：面板实际监听 18080，默认清单里没有它，必须靠 extra 兜住
	extra := map[int]string{}
	Set(extra, "面板实际监听端口", ":18080", "127.0.0.1:19091")

	if why := Reason(18080, extra); why == "" {
		t.Error("extra 里的端口应被拒绝")
	}
	if why := Reason(19091, extra); why == "" {
		t.Error("extra 里的端口应被拒绝")
	}
	// 不影响别的端口
	if why := Reason(18081, extra); why != "" {
		t.Errorf("16081 不该受 extra 影响：%s", why)
	}
}

func TestCheck_MessageIsActionable(t *testing.T) {
	err := Check(22, nil)
	if err == nil {
		t.Fatal("22 应被拒绝")
	}
	// 提示里必须带上端口与理由，否则用户不知道该改成什么
	msg := err.Error()
	for _, want := range []string{"22", "SSH"} {
		if !contains(msg, want) {
			t.Errorf("错误信息里应包含 %q：%s", want, msg)
		}
	}
}

func TestFromListenAddr(t *testing.T) {
	cases := map[string]int{
		":8080":             8080,
		"127.0.0.1:9091":    9091,
		"0.0.0.0:8443":      8443,
		"[::]:7000":         7000,
		"8080":              8080,
		"":                  0,
		"not-an-addr":       0,
		"127.0.0.1:notport": 0,
	}
	for in, want := range cases {
		if got := FromListenAddr(in); got != want {
			t.Errorf("FromListenAddr(%q) = %d，期望 %d", in, got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
