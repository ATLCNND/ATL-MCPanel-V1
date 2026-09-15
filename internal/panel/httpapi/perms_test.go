package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestLevelRank(t *testing.T) {
	cases := map[string]int{
		"owner":  3,
		"collab": 2,
		"viewer": 1,
		"":       0,
		"bogus":  0,
	}
	for level, want := range cases {
		if got := levelRank(level); got != want {
			t.Errorf("levelRank(%q) = %d，期望 %d", level, got, want)
		}
	}
}

func TestLevelAtLeast(t *testing.T) {
	cases := []struct {
		have, need string
		want       bool
	}{
		{"owner", "viewer", true},
		{"owner", "collab", true},
		{"owner", "owner", true},
		{"collab", "viewer", true},
		{"collab", "collab", true},
		{"collab", "owner", false}, // 协作者不能做 owner 操作
		{"viewer", "viewer", true},
		{"viewer", "collab", false},
		{"viewer", "owner", false},
		{"", "viewer", false}, // 无授权
	}
	for _, c := range cases {
		if got := levelAtLeast(c.have, c.need); got != c.want {
			t.Errorf("levelAtLeast(%q, %q) = %v，期望 %v", c.have, c.need, got, c.want)
		}
	}
}

func TestExtractToken(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc123", "abc123"},
		{"abc123", ""},
		{"", ""},
		{"Bearer ", ""},
		{"bearer abc", ""}, // 大小写敏感
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		if got := extractToken(req); got != c.want {
			t.Errorf("extractToken(Authorization=%q) = %q，期望 %q", c.header, got, c.want)
		}
	}
}

func TestPanelPublicAddress(t *testing.T) {
	s := &Server{listenAddr: ":8080"} // 无 tls_listen
	cases := []struct {
		cfg  panelTunnelConfig
		host string
		want string
	}{
		{panelTunnelConfig{Enabled: false, ProxyType: "tcp", RemotePort: 18080}, "1.2.3.4", ""},
		{panelTunnelConfig{Enabled: true, ProxyType: "tcp", RemotePort: 18080}, "1.2.3.4", "http://1.2.3.4:18080"},
		{panelTunnelConfig{Enabled: true, ProxyType: "https", CustomDomain: "p.example.com"}, "1.2.3.4", "https://p.example.com"},
		{panelTunnelConfig{Enabled: true, ProxyType: "http", CustomDomain: "a.com,b.com"}, "1.2.3.4", "http://a.com"},
		{panelTunnelConfig{Enabled: true, ProxyType: "tcp", RemotePort: 9000}, "", ""},
	}
	for _, c := range cases {
		if got := s.panelPublicAddress(c.cfg, c.host); got != c.want {
			t.Errorf("panelPublicAddress(%+v, %q) = %q，期望 %q", c.cfg, c.host, got, c.want)
		}
	}

	// 配置了 HTTPS 监听时，TCP 模式的公网地址应为 https
	s2 := &Server{listenAddr: ":8080", tlsListen: ":8443"}
	got := s2.panelPublicAddress(panelTunnelConfig{Enabled: true, ProxyType: "tcp", RemotePort: 18080}, "1.2.3.4")
	if got != "https://1.2.3.4:18080" {
		t.Errorf("启用 HTTPS 后应为 https 地址，实际 %q", got)
	}
}

func TestPublicTargetPort(t *testing.T) {
	s := &Server{listenAddr: ":8080"}
	if got := s.publicTargetPort(); got != 8080 {
		t.Errorf("无 TLS 时应转发到 HTTP 端口，实际 %d", got)
	}
	s2 := &Server{listenAddr: ":8080", tlsListen: ":8443"}
	if got := s2.publicTargetPort(); got != 8443 {
		t.Errorf("启用 TLS 时应转发到 HTTPS 端口，实际 %d", got)
	}
}

func TestSplitDomains(t *testing.T) {
	if got := splitDomains(""); got != nil {
		t.Errorf("空字符串应返回 nil，实际 %v", got)
	}
	got := splitDomains(" a.com , b.com ,, ")
	if len(got) != 2 || got[0] != "a.com" || got[1] != "b.com" {
		t.Errorf("域名拆分不正确: %v", got)
	}
}
