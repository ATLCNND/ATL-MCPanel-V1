package container

import (
	"strings"
	"testing"
)

// 容器模板的额外汇编参数必须真的出现在 docker run 命令里。
//
// 这几项是「通用模板能不能用」的全部：端口不通就访问不到、卷不挂数据就丢、
// 环境变量不传服务就起不对。而它们都是**拼字符串**的地方，最容易静默漏掉 ——
// 所以用断言逐项检查 argv，而不是只看「没报错」。
func TestArgvIncludesTemplateParams(t *testing.T) {
	rt := NewWithBin("docker", "atl-runtime:test")
	argv, err := rt.Argv(Spec{
		InstanceID:   "porttest",
		Dir:          "/srv/inst",
		UID:          1000,
		GID:          1000,
		Image:        "nginx:alpine",
		ExtraPorts:   []string{"127.0.0.1:8081:80"},
		ExtraVolumes: []string{"/srv/www:/usr/share/nginx/html:ro"},
		ExtraEnv:     []string{"TZ=Asia/Shanghai", "FOO=bar"},
		Command:      []string{"nginx", "-g", "daemon off;"},
	})
	if err != nil {
		t.Fatalf("构造 argv 失败: %v", err)
	}
	joined := strings.Join(argv, " ")
	checks := []struct{ what, want string }{
		{"实例镜像", "nginx:alpine"},
		{"端口发布", "--publish 127.0.0.1:8081:80"},
		{"卷挂载", "-v /srv/www:/usr/share/nginx/html:ro"},
		{"环境变量1", "-e TZ=Asia/Shanghai"},
		{"环境变量2", "-e FOO=bar"},
	}
	for _, c := range checks {
		if !strings.Contains(joined, c.want) {
			t.Errorf("%s 没进 argv：期望包含 %q\n实际: %s", c.what, c.want, joined)
		}
	}
	// 镜像不能回退成节点默认镜像
	if strings.Contains(joined, "atl-runtime:test") {
		t.Errorf("指定了实例镜像却仍用了节点默认镜像: %s", joined)
	}
	// 既有隔离参数不能被模板参数挤掉
	for _, must := range []string{"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges"} {
		if !strings.Contains(joined, must) {
			t.Errorf("隔离参数 %q 丢失（模板参数不该放宽隔离）: %s", must, joined)
		}
	}
}

// 同一个宿主端口不能被发布两次：容器模板给了 ExtraPorts 时，spec.Port 必须让位。
//
// 真机现象：面板给实例填了 port=29175，用户又写了 "127.0.0.1:29175:80"，
// docker run 里就有两条 --publish 抢同一端口，直接报 port is already allocated。
func TestArgvNoDoublePortPublish(t *testing.T) {
	rt := NewWithBin("docker", "img:test")
	argv, err := rt.Argv(Spec{
		InstanceID:        "p1",
		Dir:               "/srv/i",
		UID:               1000,
		GID:               1000,
		Port:              29175,
		ExtraPorts:        []string{"127.0.0.1:29175:80"},
		AllowEmptyCommand: true,
	})
	if err != nil {
		t.Fatalf("构造 argv 失败: %v", err)
	}
	joined := strings.Join(argv, " ")
	if n := strings.Count(joined, "29175"); n != 1 {
		t.Errorf("端口 29175 应只出现一次，实际 %d 次: %s", n, joined)
	}
	if !strings.Contains(joined, "--publish 127.0.0.1:29175:80") {
		t.Errorf("用户的映射没进 argv: %s", joined)
	}
}

// tmpfs 必须真的进 docker run（只读根下的可写例外，nginx 这类镜像靠它）。
func TestArgvIncludesTmpfs(t *testing.T) {
	rt := NewWithBin("docker", "img:test")
	argv, err := rt.Argv(Spec{
		InstanceID:        "t1",
		Dir:               "/srv/t1",
		UID:               1000,
		GID:               1000,
		ExtraTmpfs:        []string{"/var/cache/nginx", "/var/run"},
		AllowEmptyCommand: true,
	})
	if err != nil {
		t.Fatalf("构造 argv 失败: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--tmpfs /var/cache/nginx:size=64m,mode=1777") {
		t.Errorf("tmpfs 没进 argv: %s", joined)
	}
	if !strings.Contains(joined, "--tmpfs /var/run:size=64m,mode=1777") {
		t.Errorf("第二个 tmpfs 没进 argv: %s", joined)
	}
	// 隔离不能被 tmpfs 换掉
	if !strings.Contains(joined, "--read-only") {
		t.Errorf("根文件系统应仍是只读: %s", joined)
	}
}
