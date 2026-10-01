package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 回归：**并发拉实例列表不能把数据库连接池锁死**。
//
// 2026-10-01 内测的真实事故：面板整体卡死 —— 登录挂起、而健康检查仍是 1ms。
// 根因是 handleListInstances 在**遍历 rows 的过程中**又去查 instance_assignments
// （拿权限级别）：占着一个连接、再要一个连接。连接池只有 4 个，几个并发请求
// 就能把 4 个连接全占成"外层结果集"，于是谁都拿不到第二个连接、谁也不释放
// 手里的那个，**互相等死**，此后所有需要数据库的接口（包括登录）永久挂起。
//
// 为什么原来的测试没抓到：管理员走 instanceLevel 的短路分支（不查库），
// 而既有用例基本都用 admin 令牌。所以这条**必须用普通用户**。
func TestConcurrentInstanceListDoesNotDeadlock(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm12", "adm12-pass-1234")
	inst := "listdl01"
	seedInstance(t, srv, inst, 1)

	// 普通用户 + 给他一个实例授权（这样 instanceLevel 会真的去查库）
	user := mkUser(t, srv, ts, admin, "listdl", "listdl-pass-1234", RoleUser)
	code, _ := call(t, ts, "POST", "/api/instances/"+inst+"/assignments", admin,
		map[string]interface{}{"username": "listdl", "level": LevelOwner})
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("给普通用户授权失败：%d", code)
	}

	// 并发拉列表。修复前这里会永久挂住（一直等到 go test 的整体超时）。
	const workers = 12
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _, err := rawCall(ts, "GET", "/api/instances", user, nil)
			if err != nil {
				errs <- err
				return
			}
			if c != http.StatusOK {
				errs <- fmt.Errorf("HTTP %d", c)
			}
		}()
	}

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("并发拉实例列表 20 秒没返回 —— 连接池被锁死了（见本用例顶部说明）")
	}
	close(errs)
	for err := range errs {
		t.Errorf("并发请求失败：%v", err)
	}

	// 卡死之后连登录都不通，所以再确认一次"需要数据库的其它接口还活着"
	c, body := doJSON(t, ts, "POST", "/api/auth/login", "",
		map[string]string{"username": "listdl", "password": "listdl-pass-1234"})
	if c != http.StatusOK || body["token"] == nil {
		t.Fatalf("并发列表之后登录失败（连接池可能仍被占着）：%d %v", c, body)
	}
}

// 在线时长统计不该给数据库加"每个请求一次查询"的负担。
//
// 那次事故的栈里，54 个 goroutine 卡在这条查询上 —— 而它统计的只是
// "用户在线了多久"。现在先在内存里节流（30 秒窗口），再谈查库。
func TestTouchOnlineThrottlesInMemory(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm13", "adm13-pass-1234")

	for i := 0; i < 20; i++ {
		if code, _ := doJSON(t, ts, "GET", "/api/auth/me", admin, nil); code != http.StatusOK {
			t.Fatalf("第 %d 次读自己的资料失败：%d", i, code)
		}
	}

	srv.touchMu.Lock()
	n := len(srv.touchAt)
	srv.touchMu.Unlock()
	if n == 0 {
		t.Error("内存节流表为空：说明每个请求仍在查库")
	}
	// 同一用户在 30 秒窗口内只应放行一次
	if srv.touchAllowed(1) {
		t.Error("30 秒内第二次应被内存节流拦下（否则每个请求都会变成一次数据库往返）")
	}
}

// rawCall 与 call 类似，但不依赖 *testing.T（并发 goroutine 里不能调 t.Fatal）。
// httptest.Server.URL 是**字段不是方法**，所以这里收 *httptest.Server 本身。
func rawCall(ts *httptest.Server, method, path, token string, body any) (int, string, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, "", err
		}
	}
	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw), nil
}
