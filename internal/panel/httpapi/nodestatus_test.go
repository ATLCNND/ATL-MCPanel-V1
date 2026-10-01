package httpapi

import (
	"testing"
	"time"
)

// 「节点在线」的判定必须**只有一处口径**。
//
// 2026-10-01 的断网测试暴露过：把 Daemon → 面板的链路掐断后，
// 「节点监控」按心跳变旧显示了离线、告警也起来了，而 `/api/nodes` 仍报
// `status: "online"`（那是数据库原始列）—— 同一个界面里两处自相矛盾。
// 现在三处（/api/nodes、/api/my/nodes、/api/monitor/nodes、告警判定）共用 nodeOnline。
func TestNodeOnlineRule(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-(nodeStaleAfter + time.Second))
	almost := now.Add(-(nodeStaleAfter - 5*time.Second))

	cases := []struct {
		name     string
		status   string
		lastSeen *time.Time
		want     bool
	}{
		{"心跳新鲜 + 状态 online", "online", &fresh, true},
		{"刚好在阈值内", "online", &almost, true},
		{"超过阈值（Daemon 失联）", "online", &stale, false},
		{"从未上报过心跳", "online", nil, false},
		{"状态列说离线", "offline", &fresh, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := nodeOnline(c.status, c.lastSeen); got != c.want {
				t.Errorf("nodeOnline(%q, %v) = %v，期望 %v", c.status, c.lastSeen, got, c.want)
			}
		})
	}
}

// 阈值本身也要锁住：心跳 10 秒一次，90 秒是"9 个心跳没来"——
// 调小会让网络抖动被误判成离线，调大则掉线后迟迟不告警。
func TestNodeStaleAfterIsNinetySeconds(t *testing.T) {
	if nodeStaleAfter != 90*time.Second {
		t.Errorf("过期阈值应为 90 秒（心跳间隔 10 秒的 9 倍），实际 %v", nodeStaleAfter)
	}
}

// 两个节点接口都必须带上按心跳算出来的 online，否则界面又会各说各话。
func TestNodeEndpointsExposeOnline(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm11", "adm11-pass-1234")
	seedInstance(t, srv, "onlinechk", 1)

	// 把节点的 last_seen 做旧 5 分钟（模拟 Daemon 失联，但 status 列还是 online）
	if _, err := srv.db.Exec(
		`UPDATE nodes SET status = 'online', last_seen = datetime('now', '-5 minutes') WHERE id = 1`); err != nil {
		t.Fatalf("改 last_seen 失败: %v", err)
	}

	code, body := doJSONArr(t, ts, "GET", "/api/nodes", admin, nil)
	if code != 200 || len(body) == 0 {
		t.Fatalf("读 /api/nodes 失败: %d %v", code, body)
	}
	if body[0]["online"] != false {
		t.Errorf("心跳已旧 5 分钟，/api/nodes 的 online 应为 false，实际 %v（status=%v）",
			body[0]["online"], body[0]["status"])
	}
	if body[0]["status"] != "online" {
		t.Errorf("原始 status 列应保持原样（它记录的是最后一次显式上报的状态），实际 %v", body[0]["status"])
	}
	if age, ok := body[0]["last_seen_age_s"].(float64); !ok || age < 200 {
		t.Errorf("应带出心跳年龄（秒），实际 %v", body[0]["last_seen_age_s"])
	}

	code, mine := doJSONArr(t, ts, "GET", "/api/my/nodes", admin, nil)
	if code != 200 || len(mine) == 0 {
		t.Fatalf("读 /api/my/nodes 失败: %d %v", code, mine)
	}
	if mine[0]["online"] != false {
		t.Errorf("/api/my/nodes 也要按心跳判定，实际 %v", mine[0]["online"])
	}

	// 心跳恢复新鲜后应立刻回到在线
	if _, err := srv.db.Exec(
		`UPDATE nodes SET last_seen = datetime('now') WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	_, body = doJSONArr(t, ts, "GET", "/api/nodes", admin, nil)
	if body[0]["online"] != true {
		t.Errorf("心跳恢复后 online 应为 true，实际 %v", body[0]["online"])
	}
}
