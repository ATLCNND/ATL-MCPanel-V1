package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这些测试盯的是"启动参数拼得对不对"与"逃逸符号链接找不找得到"。
// 它们不需要 docker：docker run 的语义在 VM 上用真实容器验（22 项断言），
// 这里只保证参数本身没有明显的错漏 —— 而参数错是最容易在真实环境里
// 以一句莫名其妙的 docker 报错结束的地方（实测踩过相对路径挂载）。

func TestArgv_IncludesSecurityFlags(t *testing.T) {
	r := &Runtime{bin: "/usr/bin/docker", image: DefaultImage}
	argv, err := r.Argv(Spec{
		InstanceID:  "beta01",
		Dir:         "/opt/atl-node/instances/beta01",
		UID:         997,
		GID:         994,
		MemoryBytes: 4 << 30,
		CPUPercent:  150,
		Port:        25565,
		JavaHome:    "/usr/lib/jvm/temurin-21",
		Command:     []string{"bash", "/data/start.sh"},
	})
	if err != nil {
		t.Fatalf("Argv 失败: %v", err)
	}
	line := strings.Join(argv, " ")

	want := []string{
		"run", "--rm", "-i",
		"--name atl-beta01",
		"--user 997:994", // 没有它就等于把宿主 root 交出去（节点禁用 user namespace）
		"--memory 4294967296b", "--memory-swap 4294967296b",
		"--cpus 1.5",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
		"--read-only",
		"--pids-limit 256",
		"-v /opt/atl-node/instances/beta01:/data",
		"-v /usr/lib/jvm/temurin-21:/usr/lib/jvm/temurin-21:ro",
		"-e HOME=/data",
		"--publish 127.0.0.1:25565:25565", // 只发布到回环，frpc 在宿主上连它
		"--network atl-beta01-net",        // 每实例一张网，避免实例之间互通
		DefaultImage,
		"bash /data/start.sh",
	}
	for _, w := range want {
		if !strings.Contains(line, w) {
			t.Errorf("启动参数缺少 %q\n实际: %s", w, line)
		}
	}
	// 不允许出现 host 网络（那会让容器直接看见宿主回环服务）
	if strings.Contains(line, "--network host") || strings.Contains(line, "network=host") {
		t.Errorf("不允许使用 host 网络: %s", line)
	}
}

func TestArgv_RejectsRelativePaths(t *testing.T) {
	r := &Runtime{bin: "docker", image: DefaultImage}

	// 相对路径会被 docker 当成"命名卷"，报错信息与真实原因对不上：
	// invalid mount config ... mount path must be absolute（实测踩过）
	if _, err := r.Argv(Spec{InstanceID: "x", Dir: "instances/x", Command: []string{"true"}}); err == nil {
		t.Error("实例目录是相对路径时必须报错")
	}
	if _, err := r.Argv(Spec{
		InstanceID: "x", Dir: "/instances/x", JavaHome: "jvm/21", Command: []string{"true"},
	}); err == nil {
		t.Error("JDK 目录是相对路径时必须报错")
	}

	// 共享资源目录是可选项：相对路径时跳过挂载，但不能因此让实例起不来
	argv, err := r.Argv(Spec{
		InstanceID: "x", Dir: "/instances/x", ResourcesDir: "resources",
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatalf("共享资源目录相对路径不该导致失败: %v", err)
	}
	if strings.Contains(strings.Join(argv, " "), "resources:resources") {
		t.Errorf("相对路径的共享资源不该被挂进容器: %s", strings.Join(argv, " "))
	}
}

func TestArgv_OmitsOptionalBits(t *testing.T) {
	r := &Runtime{bin: "docker", image: DefaultImage}
	argv, err := r.Argv(Spec{InstanceID: "x", Dir: "/instances/x", UID: 1, GID: 2,
		Command: []string{"java", "-version"}})
	if err != nil {
		t.Fatalf("Argv 失败: %v", err)
	}
	line := strings.Join(argv, " ")
	for _, bad := range []string{"--memory", "--cpus", "--publish", "JAVA_HOME"} {
		if strings.Contains(line, bad) {
			t.Errorf("未指定限额/端口/JDK 时不该出现 %q：%s", bad, line)
		}
	}
}

func TestNameOf_NoCollisionsWithOtherTools(t *testing.T) {
	if got := NameOf("beta01"); got != "atl-beta01" {
		t.Errorf("容器名 = %q，期望 atl-beta01", got)
	}
	if got := NetworkOf("beta01"); got != "atl-beta01-net" {
		t.Errorf("网络名 = %q，期望 atl-beta01-net", got)
	}
}

// TestJDKExtraMounts 盯的是"发行版 JDK 把配置放在 JAVA_HOME 之外"这件事。
//
// 实测背景：Debian 的 openjdk 把 java.security 放在 /etc/java-21-openjdk，
// $JAVA_HOME/conf/security/java.security 只是指向它的符号链接。只挂 /usr/lib/jvm 时
// 这个链接在容器里悬空，走到需要它的代码路径就抛
// "InternalError: Error loading java.security file"（错误信息与真实原因不符）。
func TestJDKExtraMounts(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "jvm", "jdk-21")

	// 造一个"典型布局"：JDK 内有若干符号链接指向外面的 /etc 风格目录
	outside := filepath.Join(root, "etc", "java-21-openjdk", "security")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "java.security"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "conf", "security"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "java.security"),
		filepath.Join(home, "conf", "security", "java.security")); err != nil {
		t.Skipf("当前环境不支持符号链接: %v", err)
	}
	// 内部链接（指向 JAVA_HOME 之内）不该被当成额外挂载
	if err := os.WriteFile(filepath.Join(home, "real.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "real.txt"), filepath.Join(home, "link.txt")); err != nil {
		t.Fatal(err)
	}
	// 悬挂链接（目标不存在）应被跳过
	if err := os.Symlink(filepath.Join(root, "not-there", "gone"), filepath.Join(home, "dangling")); err != nil {
		t.Fatal(err)
	}

	got := JDKExtraMounts(home)
	if len(got) != 1 {
		t.Fatalf("应恰好找到 1 个额外挂载目录，实际 %d 个: %v", len(got), got)
	}
	if !strings.Contains(got[0], "java-21-openjdk") {
		t.Errorf("额外挂载目录不对: %v", got)
	}
	if strings.Contains(strings.Join(got, " "), "not-there") {
		t.Errorf("悬挂链接不该产生挂载: %v", got)
	}

	// 缓存：第二次调用应返回同样结果（且不因缓存而串味）
	again := JDKExtraMounts(home)
	if len(again) != len(got) || again[0] != got[0] {
		t.Errorf("缓存结果不一致: %v vs %v", again, got)
	}
	if JDKExtraMounts("") != nil {
		t.Error("空 JAVA_HOME 应返回 nil")
	}
}
