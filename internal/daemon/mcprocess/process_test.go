package mcprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
)

// testInstance 测试用的实例构造：状态目录就用实例目录（测试不涉及
// "root 信任的文件不能放租户目录"那条边界），并注入一个"身份与当前进程相同"
// 的运行身份。
//
// 注入身份不是为了省事：Start 里有一道安全闸 —— Daemon 以 root 运行却没有
// 降权身份时**拒绝启动实例**（那正是我们要修掉的漏洞）。测试环境与我们的 VM
// 本身就是 root，不注入的话一批与被测行为无关的用例会因这道闸失败。
// 身份取当前 uid，于是 chown 与 Credential 都退化成等价操作，测试语义不变。
// 注意这里**没有**用 t：调用点多达十几处，多传一个参数只会让改动面变大。
func testInstance(id, dir, jar, maxMem, minMem string) *Instance {
	inst := NewInstance(id, dir, dir, jar, maxMem, minMem)
	inst.RunAsFor = func(string) (*runas.Identity, error) {
		return &runas.Identity{
			Username: "test-same-uid",
			UID:      uint32(os.Getuid()),
			GID:      uint32(os.Getgid()),
			Home:     dir,
		}, nil
	}
	return inst
}

func TestNormMem(t *testing.T) {
	cases := map[string]string{
		"":     "1G",
		"2G":   "2G",
		"512M": "512M",
	}
	for in, want := range cases {
		if got := normMem(in); got != want {
			t.Errorf("normMem(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestRenderCommand(t *testing.T) {
	inst := testInstance("test1", "/opt/mcpanel/instances/test1", "/jars/folia.jar", "3G", "1G")

	cases := []struct {
		tpl  string
		want string
	}{
		{"java -Xmx{max_mem} -Xms{min_mem} -jar {jar} nogui",
			"java -Xmx3G -Xms1G -jar /jars/folia.jar nogui"},
		{"cd {dir} && ./start.sh", "cd /opt/mcpanel/instances/test1 && ./start.sh"},
		{"{java} -version", "java -version"},
		{"无占位符", "无占位符"},
		{"{jar} {jar}", "/jars/folia.jar /jars/folia.jar"}, // 多次替换
	}
	for _, c := range cases {
		if got := inst.renderCommand(c.tpl); got != c.want {
			t.Errorf("renderCommand(%q) = %q，期望 %q", c.tpl, got, c.want)
		}
	}
}

func TestIsAlive(t *testing.T) {
	if IsAlive(-1) || IsAlive(0) {
		t.Error("非法 PID 应返回 false")
	}
	// 当前进程自身必然存活
	if !IsAlive(os.Getpid()) {
		t.Error("当前进程应存活")
	}
	// 找一个几乎不可能存在的 PID
	if IsAlive(999999) {
		t.Log("警告：PID 999999 意外存在，跳过该断言")
	}
}

func TestRecentOutputReadsTail(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 写入 100 行
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&sb, "line-%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "console.log"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	inst := testInstance("t", dir, "", "1G", "1G")

	// 取最近 10 行
	got := inst.RecentOutput(10)
	if len(got) != 10 {
		t.Fatalf("应返回 10 行，实际 %d", len(got))
	}
	if got[len(got)-1] != "line-100\n" {
		t.Errorf("最后一行应为 line-100，实际 %q", got[len(got)-1])
	}
	if got[0] != "line-91\n" {
		t.Errorf("第一行应为 line-91，实际 %q", got[0])
	}
	for _, l := range got {
		if !strings.HasSuffix(l, "\n") {
			t.Errorf("每行应以换行结尾: %q", l)
		}
	}

	// 请求行数超过文件行数时返回全部
	if all := inst.RecentOutput(500); len(all) != 100 {
		t.Errorf("应返回全部 100 行，实际 %d", len(all))
	}

	// 非法参数
	if n := inst.RecentOutput(0); n != nil {
		t.Errorf("maxLines<=0 应返回 nil，实际 %v", n)
	}
}

func TestRecentOutputMissingFile(t *testing.T) {
	inst := testInstance("t", t.TempDir(), "", "1G", "1G")
	if got := inst.RecentOutput(10); got != nil {
		t.Errorf("日志不存在时应返回 nil，实际 %v", got)
	}
}

func TestReadPIDFileStaleContent(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte("garbage"), 0o644)
	if pid := ReadPIDFile(dir); pid != 0 {
		t.Errorf("非法内容应返回 0，实际 %d", pid)
	}
	_ = os.WriteFile(filepath.Join(dir, "daemon.pid"), []byte(strconv.Itoa(os.Getpid())), 0o644)
	if pid := ReadPIDFile(dir); pid != os.Getpid() {
		t.Errorf("应解析出当前 PID %d，实际 %d", os.Getpid(), pid)
	}
}

func TestRotateBySize(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "console.log")

	// 未超限：不轮转
	_ = os.WriteFile(logPath, []byte("small"), 0o644)
	rotateBySize(logPath, 1024, 3)
	if _, err := os.Stat(logPath + ".1"); err == nil {
		t.Error("未超限不应产生轮转文件")
	}

	// 超限：轮转为 .1
	_ = os.WriteFile(logPath, make([]byte, 2048), 0o644)
	rotateBySize(logPath, 1024, 3)
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("超限应生成 console.log.1: %v", err)
	}

	// 连续轮转应逐级推移
	for i := 0; i < 5; i++ {
		_ = os.WriteFile(logPath, make([]byte, 2048), 0o644)
		rotateBySize(logPath, 1024, 3)
	}
	if _, err := os.Stat(logPath + ".3"); err != nil {
		t.Errorf("应保留到 console.log.3: %v", err)
	}
	// 超出保留数量的编号不应存在
	if _, err := os.Stat(logPath + ".4"); err == nil {
		t.Error("不应存在超出保留数量的 console.log.4")
	}

	// 文件不存在时不应 panic
	rotateBySize(filepath.Join(dir, "missing.log"), 1, 3)
}

func TestInstanceDefaultState(t *testing.T) {
	inst := testInstance("id1", "/dir", "/j.jar", "2G", "1G")
	if inst.Status() != "stopped" {
		t.Errorf("初始状态应为 stopped，实际 %s", inst.Status())
	}
	if inst.PID() != 0 {
		t.Errorf("未运行时 PID 应为 0，实际 %d", inst.PID())
	}
	if inst.Adopted() {
		t.Error("新建实例不应处于接管状态")
	}
	// 订阅与广播
	ch, cancel := inst.Subscribe()
	defer cancel()
	inst.broadcast("hello\n")
	select {
	case got := <-ch:
		if got != "hello\n" {
			t.Errorf("广播内容不匹配: %q", got)
		}
	default:
		t.Error("订阅者应收到广播")
	}
}

// java_version 填相对路径时要按**实例目录**解析。
//
// 原先直接交给 javaruntime.Resolve，它拿 Daemon 自己的工作目录（/opt/mcpanel）
// 去 stat，于是实例里的 jdk/bin/java 永远找不到，还会静默回退到 PATH 上的 java ——
// 表现就是"我明明填了路径却没用"。
func TestResolveJavaBinRelativePath(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "jdk", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(binDir, "java")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	inst := testInstance("t1", dir, "", "1G", "1G")
	inst.JavaVersion = "jdk/bin/java"
	if got := inst.resolveJavaBin(); got != bin {
		t.Errorf("相对路径应按实例目录解析：期望 %s，实际 %s", bin, got)
	}
	if inst.JavaNote() == "" {
		t.Error("解析成功时也该留下说明（界面上要看得到用的是哪个 java）")
	}
}

// 相对路径不存在时回退到 PATH 上的 java，但**必须留下说明**（否则用户以为生效了）。
func TestResolveJavaBinMissingRelativeFallsBack(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("t2", dir, "", "1G", "1G")
	inst.JavaVersion = "nope/bin/java"

	if got := inst.resolveJavaBin(); got != "java" {
		t.Errorf("找不到时应回退到裸 java，实际 %s", got)
	}
	note := inst.JavaNote()
	if note == "" {
		t.Fatal("回退必须留下说明")
	}
	if !strings.Contains(note, "实例目录") {
		t.Errorf("说明应指明是「实例目录下找不到」，实际: %s", note)
	}
}

// 绝对路径与空值保持原行为。
func TestResolveJavaBinAbsAndEmpty(t *testing.T) {
	dir := t.TempDir()
	inst := testInstance("t3", dir, "", "1G", "1G")

	inst.JavaVersion = ""
	if got := inst.resolveJavaBin(); got != "java" {
		t.Errorf("空值应直接用 PATH 上的 java，实际 %s", got)
	}

	abs := filepath.Join(dir, "javabin")
	if err := os.WriteFile(abs, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst.JavaVersion = abs
	if got := inst.resolveJavaBin(); got != abs {
		t.Errorf("绝对路径应原样使用，期望 %s，实际 %s", abs, got)
	}
}
