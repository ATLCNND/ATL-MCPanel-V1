package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// 到期实例**不能再被启动**，但必须**还能被停**。
//
// 背景（这轮发现的缺口）：到期以前只是调度器每分钟巡检时顺手做的事
//（scheduler.checkExpiry 的已到期分支），接口层一点检查都没有 —— 归属者只要
// 再点一下「启动」就等于把到期时间无限延长；更省事的是一条
// `{"action":"start","cron":"* * * * *"}` 的定时任务，它连点都不用点。
//
// 这里锁住两件事：start 被拒（403 + 能看懂的到期说明），stop 不被到期拦。
func TestExpiredInstanceStartRefusedStopAllowed(t *testing.T) {
	srv, ts := newTestServer(t)
	seedInstance(t, srv, "expinst1", 1)
	admin := loginAs(t, ts, "expadmin", "expadmin-pass-1234")

	setInstanceExpiry(t, srv, "expinst1", time.Now().Add(-2*time.Hour), 1)

	code, body := doJSON(t, ts, "POST", "/api/instances/expinst1/start", admin, nil)
	if code != http.StatusForbidden {
		t.Fatalf("已到期且开启自动停机的实例不该能启动：code=%d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "到期") {
		t.Errorf("拒绝理由要说明是到期（否则用户会以为是权限或节点故障）：%q", msg)
	}

	// 停止必须放行：到期实例本来就该是停着的，拦它只会让管理员没法收尾。
	// 测试环境里没有 Daemon，所以这里只断言"不是被到期拦下的 403"。
	code, body = doJSON(t, ts, "POST", "/api/instances/expinst1/stop", admin, nil)
	if code == http.StatusForbidden {
		t.Fatalf("停止到期实例被拒绝了（到期只该拦启动，不该拦停止）：%v", body)
	}
}

// 到期只拦"把实例拉起来"的动作。
//
// 这条用 expiryBlocksStart 钉住边界，不依赖节点侧的具体报错：
// 一个写错的动作名（或多拦了 stop）会让用户既开不了也关不掉，那比不拦更糟。
func TestExpiryBlocksStartOnly(t *testing.T) {
	if !expiryBlocksStart("start") {
		t.Error("start 必须被到期拦下")
	}
	// restart 同样是把实例拉起来（到期语义下"重启一下"不该成为绕过口）
	if !expiryBlocksStart("restart") {
		t.Error("restart 也必须被到期拦下")
	}
	for _, action := range []string{"stop", "kill", "delete"} {
		if expiryBlocksStart(action) {
			t.Errorf("%s 不该被到期拦下：到期实例本就该停着，关不掉/删不掉会让运维没法收尾", action)
		}
	}
}

// instanceStartable 的判定口径必须与 scheduler.checkExpiry 的"已到期"分支一致，
// 否则会出现"调度器认为该停、接口认为能开"的来回拉扯。
func TestInstanceStartable(t *testing.T) {
	srv, _ := newTestServer(t)
	seedInstance(t, srv, "inst-a", 1)

	// expires_at 为 NULL ＝ 永不过期
	if ok, reason := srv.instanceStartable("inst-a"); !ok {
		t.Errorf("没设到期时间应放行：%s", reason)
	}

	// 还没到期
	setInstanceExpiry(t, srv, "inst-a", time.Now().Add(24*time.Hour), 1)
	if ok, reason := srv.instanceStartable("inst-a"); !ok {
		t.Errorf("还没到期应放行：%s", reason)
	}

	// 已到期但配置为「仅告警不自动停止」→ 与调度器一致，不拦
	setInstanceExpiry(t, srv, "inst-a", time.Now().Add(-time.Hour), 0)
	if ok, reason := srv.instanceStartable("inst-a"); !ok {
		t.Errorf("expiry_autostop=0（仅告警）时不该拦启动：%s", reason)
	}

	// 已到期 + 到期自动停机 → 拦
	setInstanceExpiry(t, srv, "inst-a", time.Now().Add(-time.Hour), 1)
	ok, reason := srv.instanceStartable("inst-a")
	if ok {
		t.Error("已到期且开启自动停机时必须拦下启动")
	} else if !strings.Contains(reason, "到期") {
		t.Errorf("理由要说清是到期：%q", reason)
	}

	// 查不到实例记录时不拦：这是"要不要停机"的策略判断，不是权限判断，
	// 权限另由 requireInstanceLevel 把关 —— 不该因为一次查询失败挡住正常用户。
	if ok, reason := srv.instanceStartable("no-such-instance"); !ok {
		t.Errorf("查不到实例时不该拦：%s", reason)
	}
}

// setInstanceExpiry 直接写库设置到期时间（等价于 handleSetExpiry 落库的那两列）。
// autostop 用 int（0/1）而不是 bool：库里存的就是 INTEGER。
func setInstanceExpiry(t *testing.T, srv *Server, instanceID string, at time.Time, autostop int) {
	t.Helper()
	if _, err := srv.db.Exec(
		`UPDATE instances SET expires_at = ?, expiry_autostop = ? WHERE instance_id = ?`,
		at.UTC(), autostop, instanceID); err != nil {
		t.Fatalf("设置到期时间失败: %v", err)
	}
}
