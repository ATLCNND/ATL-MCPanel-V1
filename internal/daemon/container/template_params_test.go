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
