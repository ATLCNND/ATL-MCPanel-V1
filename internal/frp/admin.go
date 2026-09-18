package frp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// AdminClient 访问 frps 的管理 API，用于读取每个代理的流量。
//
// 为什么需要它：节点整机的网络吞吐无法区分"哪个实例在用带宽"。
// frps 的 /api/proxy/{tcp,udp} 会返回每个代理的累计流量，
// 按隧道名（即 tunnel_id）过滤即可得到单实例的真实流量。
//
// 注意：该接口返回的是**当日累计值**，不是速率；速率由两次采样的差值算出。
// 且 frps 每天零点重置计数 —— 计数器变小时按"新周期"处理。
type AdminClient struct {
	addr     string
	user     string
	password string
	http     *http.Client
}

// NewAdminClient 创建管理 API 客户端。addr 为空时返回 nil（表示未启用）。
func NewAdminClient(addr, user, password string) *AdminClient {
	if addr == "" {
		return nil
	}
	return &AdminClient{
		addr:     addr,
		user:     user,
		password: password,
		http:     &http.Client{Timeout: 4 * time.Second},
	}
}

// proxyStat frps 返回的单条代理状态
type proxyStat struct {
	Name            string `json:"name"`
	Status          string `json:"status"`
	TodayTrafficIn  int64  `json:"todayTrafficIn"`
	TodayTrafficOut int64  `json:"todayTrafficOut"`
	CurConns        int64  `json:"curConns"`
}

// fetchProxies 拉取指定协议的代理列表。
// frps 的类型路径是 tcp/udp/http/https/stcp 等。
func (c *AdminClient) fetchProxies(protocol string) ([]proxyStat, error) {
	if c == nil {
		return nil, fmt.Errorf("未配置 frps 管理 API")
	}
	url := fmt.Sprintf("http://%s/api/proxy/%s", c.addr, protocol)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("frps 管理 API 返回 %d", resp.StatusCode)
	}
	var out struct {
		Proxies []proxyStat `json:"proxies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Proxies, nil
}

// trafficTotal 汇总指定隧道名的当日收/发总量。
// names 为该实例所有隧道的 tunnel_id 集合。
func (c *AdminClient) trafficTotal(names map[string]bool) (in, out int64, conns int64, err error) {
	if c == nil || len(names) == 0 {
		return 0, 0, 0, nil
	}
	// 一个实例可能同时有 tcp 与 udp 隧道，两种都要查
	all := []proxyStat{}
	for _, proto := range []string{"tcp", "udp"} {
		ps, e := c.fetchProxies(proto)
		if e != nil {
			err = e
			continue
		}
		all = append(all, ps...)
	}
	for _, p := range all {
		if names[p.Name] {
			in += p.TodayTrafficIn
			out += p.TodayTrafficOut
			conns += p.CurConns
		}
	}
	return in, out, conns, err
}

// trafficSampler 记录上一次采样，用于把累计值换算成速率。
type trafficSampler struct {
	mu      sync.Mutex
	lastIn  int64
	lastOut int64
	lastAt  time.Time
	valid   bool
}

// Rate 由累计值计算速率（字节/秒）。
//
// 计数器回绕（frps 每日重置）或首次采样时不返回速率，
// 只更新基准 —— 否则重置瞬间会算出负值或一个巨大的假峰值。
func (t *trafficSampler) Rate(in, out int64) (inRate, outRate int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if !t.valid || in < t.lastIn || out < t.lastOut {
		t.lastIn, t.lastOut, t.lastAt, t.valid = in, out, now, true
		return 0, 0
	}
	elapsed := now.Sub(t.lastAt).Seconds()
	if elapsed < 0.5 {
		return 0, 0
	}
	inRate = int64(float64(in-t.lastIn) / elapsed)
	outRate = int64(float64(out-t.lastOut) / elapsed)
	t.lastIn, t.lastOut, t.lastAt = in, out, now
	return inRate, outRate
}
