package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 单个实例的定时任务条数必须有上限。
//
// 背景（这轮发现的缺口）：定时任务由实例归属者自由创建且不限条数，而调度器
// 每轮都要**同步**派发它们（开机要等 JVM 起来，单条任务最长 3 分钟）。
// 一批"每分钟一次"的任务就能把一轮 RunOnce 拖过好几个 tick —— 排在派发之后的
// 到期自动停机与磁盘超限停机会被一起饿死，而那两道正是"无限期免费占用"
// 与"节点磁盘被写满"的最后一道防线。
func TestCreateTaskRespectsLimit(t *testing.T) {
	srv, ts := newTestServer(t)
	seedInstance(t, srv, "taskcap1", 1)
	admin := loginAs(t, ts, "taskcapadmin", "taskcap-pass-1234")

	// 先塞到上限：其中一条是**已禁用**的 —— 禁用的行同样要占调度器每轮的解析开销，
	// 也是用户留着"以后再用"的，不清理就不该再让新的进来。
	for i := 0; i < maxTasksPerInstance-1; i++ {
		if _, err := srv.db.Exec(
			`INSERT INTO instance_tasks (instance_id, name, action, cron, enabled)
			 VALUES (?, ?, 'start', '0 6 * * *', 1)`,
			"taskcap1", fmt.Sprintf("任务 %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.db.Exec(
		`INSERT INTO instance_tasks (instance_id, name, action, cron, enabled)
		 VALUES ('taskcap1', '已禁用', 'start', '0 6 * * *', 0)`); err != nil {
		t.Fatal(err)
	}

	code, body := doJSON(t, ts, "POST", "/api/instances/taskcap1/tasks", admin,
		map[string]any{"action": "start", "cron": "0 6 * * *"})
	if code != http.StatusBadRequest {
		t.Fatalf("已达上限时新建任务应被拒：code=%d body=%v", code, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "上限") {
		t.Errorf("拒绝理由要说明已达上限（用户才知道要删旧任务）：%q", msg)
	}

	// 上限只针对数量：删掉一条之后应当又能新建（避免"一旦满了就永久锁死"）
	if _, err := srv.db.Exec(
		`DELETE FROM instance_tasks WHERE instance_id = 'taskcap1' AND enabled = 0`); err != nil {
		t.Fatal(err)
	}
	code, body = doJSON(t, ts, "POST", "/api/instances/taskcap1/tasks", admin,
		map[string]any{"action": "start", "cron": "0 6 * * *"})
	if code != http.StatusOK {
		t.Fatalf("腾出位置后应能继续创建：code=%d body=%v", code, body)
	}
}
