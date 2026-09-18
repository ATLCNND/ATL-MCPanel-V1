package httpapi

import "testing"

func TestSameMachine(t *testing.T) {
	cases := []struct {
		name     string
		lineHost string
		nodeIP   string
		want     bool
	}{
		{"线路填回环，节点在本机", "127.0.0.1", "127.0.0.1", true},
		{"线路填 localhost", "localhost", "10.0.0.5", true},
		{"线路填 IPv6 回环", "::1", "10.0.0.5", true},
		{"线路填 [::1]", "[::1]", "10.0.0.5", true},
		{"线路填节点自己的 IP", "10.0.0.5", "10.0.0.5", true},
		{"大小写与空格不应影响判断", " LocalHost ", "10.0.0.5", true},
		{"线路在别的机器（域名）", "frp.example.com", "10.0.0.5", false},
		{"线路在别的机器（IP）", "1.2.3.4", "10.0.0.5", false},
		{"节点 IP 未知时只有回环算同机", "1.2.3.4", "", false},
		{"主机名为空时按同机处理（宁可多避开一个端口）", "", "10.0.0.5", true},
	}
	for _, c := range cases {
		if got := sameMachine(c.lineHost, c.nodeIP); got != c.want {
			t.Errorf("%s: sameMachine(%q, %q) = %v，期望 %v", c.name, c.lineHost, c.nodeIP, got, c.want)
		}
	}
}
