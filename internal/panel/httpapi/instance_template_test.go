package httpapi

import (
	"net/http"
	"testing"
)

// 模板字段的管道：合法值通过、未知值拒绝、空串按 minecraft（存量默认）。
func TestNormalizeTemplate(t *testing.T) {
	cases := []struct { in, want string; bad bool }{
		{"", "minecraft", false},
		{"minecraft", "minecraft", false},
		{" Docker ", "docker", false},
		{"console", "console", false},
		{"paper", "", true},
	}
	for _, c := range cases {
		got, err := normalizeTemplate(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("%q 应被拒绝，实际得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不该报错: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 未知模板必须在**调用 Daemon 之前**就被拒（否则会先建一半再报错）。
// 注意：正向用例（真的建一个 docker 实例）需要活的 Daemon，这里只验证校验与默认值。
func TestCreateInstanceRejectsUnknownTemplateEarly(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, srv, ts, "adm-t1", "adm-t1-pass-1234")
	if _, err := srv.db.Exec(`INSERT INTO nodes (name, ip, ssh_user, ssh_auth, ssh_port) VALUES ('n1','127.0.0.1','root','',22)`); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	var nodeID int64
	_ = srv.db.QueryRow(`SELECT id FROM nodes WHERE name='n1'`).Scan(&nodeID)

	code, body := doJSON(t, ts, "POST", "/api/instances", admin, map[string]interface{}{
		"node_id": nodeID, "instance_id": "t-bad", "name": "x", "template": "paper", "port": 26002,
	})
	if code != http.StatusBadRequest {
		t.Errorf("未知模板应 400，实际 %d %v", code, body)
	}

	// 校验失败不该留下半条记录
	var n int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE instance_id='t-bad'`).Scan(&n)
	if n != 0 {
		t.Errorf("被拒的创建留下了 %d 行记录", n)
	}
}

// 存量实例（迁移前就存在）升级后必须是 minecraft —— 默认值写错的后果是全部实例「变成」别的模板。
func TestInstanceTemplateDefaultsToMinecraft(t *testing.T) {
	srv, _ := newTestServer(t)
	if _, err := srv.db.Exec(`INSERT INTO nodes (name, ip, ssh_user, ssh_auth, ssh_port) VALUES ('n2','127.0.0.1','root','',22)`); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	var nodeID int64
	_ = srv.db.QueryRow(`SELECT id FROM nodes WHERE name='n2'`).Scan(&nodeID)
	// 不带 template 插入（模拟老代码路径）
	if _, err := srv.db.Exec(`INSERT INTO instances (instance_id, node_id, name) VALUES ('legacy',?,'x')`, nodeID); err != nil {
		t.Fatalf("插入存量实例失败: %v", err)
	}
	var tpl string
	_ = srv.db.QueryRow(`SELECT COALESCE(template,'') FROM instances WHERE instance_id='legacy'`).Scan(&tpl)
	if tpl != "minecraft" {
		t.Errorf("存量实例默认模板 = %q，期望 minecraft", tpl)
	}
}

// 三种模板的必要条件不同，必须分别拦在建实例之前：
//   docker  → 必须有镜像；
//   console → 必须有启动命令（它就是「跑用户给的命令、把 stdin/stdout 接到控制台」。
//             脚本启动的输出、交互式提示都从这里看 —— 没有命令等于没有东西可跑）。
func TestValidateTemplateInput(t *testing.T) {
	cases := []struct {
		name    string
		tpl     string
		req     createInstanceReq
		wantErr bool
	}{
		{"minecraft 不额外要求", "minecraft", createInstanceReq{}, false},
		{"docker 缺镜像", "docker", createInstanceReq{}, true},
		{"docker 有镜像", "docker", createInstanceReq{Image: "nginx:alpine"}, false},
		{"console 缺命令", "console", createInstanceReq{}, true},
		{"console 空白命令", "console", createInstanceReq{StartCommand: "   "}, true},
		{"console 有命令", "console", createInstanceReq{StartCommand: "./run.sh"}, false},
	}
	for _, c := range cases {
		err := validateTemplateInput(c.tpl, c.req)
		if c.wantErr && err == nil {
			t.Errorf("%s：应当报错但没有", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：不该报错，得到 %v", c.name, err)
		}
	}
}
