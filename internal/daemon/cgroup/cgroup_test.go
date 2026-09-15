package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		"test1":        "test1",
		"my-server_1":  "my-server_1",
		"a.b":          "a.b",
		"中文实例":         "____",
		"a/b":          "a_b",
		"":             "instance",
		"../etc/passwd": ".._etc_passwd",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestSanitizePreventsTraversal(t *testing.T) {
	// 关键安全属性：实例 ID 不能借由 cgroup 目录名越出根目录
	m := New(t.TempDir())
	got := m.dir("../../escape")
	if filepath.Dir(got) != m.root {
		t.Errorf("目录应始终位于 root 之下：root=%s got=%s", m.root, got)
	}
}

// TestInitOnRealSystem 在真实系统上检测 cgroup v2 可用性（不修改任何设置）。
func TestInitOnRealSystem(t *testing.T) {
	m := New("") // 使用默认路径
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		t.Skip("当前环境无 cgroup v2，跳过")
	}
	// 只检测控制器可用性，不创建目录（避免测试污染系统）
	if !m.controllerAvailable("cpu") {
		t.Skip("cgroup v2 未提供 cpu 控制器")
	}
	t.Log("cgroup v2 + cpu 控制器可用")
}

// TestApplyDisabledWhenNotInitialized 未初始化时所有操作应为安全的空操作。
func TestApplyDisabledWhenNotInitialized(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "cg"))
	// 未调用 Init，enabled=false
	if m.Enabled() {
		t.Fatal("未初始化不应处于启用状态")
	}
	if err := m.Apply("i1", 200); err != nil {
		t.Errorf("未启用时 Apply 应为空操作，实际报错: %v", err)
	}
	if err := m.Assign("i1", 1234); err != nil {
		t.Errorf("未启用时 Assign 应为空操作，实际报错: %v", err)
	}
	if err := m.Remove("i1"); err != nil {
		t.Errorf("未启用时 Remove 应为空操作，实际报错: %v", err)
	}
	if _, _, err := m.Stats("i1"); err == nil {
		t.Error("未启用时 Stats 应返回错误")
	}
}

// TestQuotaValueFormat 校验 cpu.max 的取值换算（100% = 1 核）。
func TestQuotaValueFormat(t *testing.T) {
	// quotaUsecPerPercent 应为 1000（1% × 100ms = 1000us）
	if quotaUsecPerPercent != 1000 {
		t.Fatalf("换算系数应为 1000，实际 %d", quotaUsecPerPercent)
	}
	// 200% → 200000/100000，即 2 核
	if got := 200 * quotaUsecPerPercent; got != 200000 {
		t.Errorf("200%% 应为 200000us，实际 %d", got)
	}
	// 600% → 600000/100000，即 6 核
	if got := 600 * quotaUsecPerPercent; got != 600000 {
		t.Errorf("600%% 应为 600000us，实际 %d", got)
	}
}

// TestApplyInTempRoot 用临时目录模拟 cgroup 目录结构，验证文件写入正确。
func TestApplyInTempRoot(t *testing.T) {
	root := t.TempDir()
	m := New(root)
	m.enabled = true // 直接置为启用，绕过对真实 /sys 的依赖

	if err := m.Apply("inst1", 400); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "inst1", "cpu.max"))
	if err != nil {
		t.Fatalf("未写入 cpu.max: %v", err)
	}
	if string(b) != "400000 100000" {
		t.Errorf("cpu.max 内容应为 \"400000 100000\"（400%%），实际 %q", string(b))
	}

	// 配额为 0 应写 max（不限制）
	if err := m.Apply("inst2", 0); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(root, "inst2", "cpu.max"))
	if string(b) != "max 100000" {
		t.Errorf("配额 0 应写入 \"max 100000\"，实际 %q", string(b))
	}
}

func TestStatsParsing(t *testing.T) {
	root := t.TempDir()
	m := New(root)
	m.enabled = true

	dir := filepath.Join(root, "inst1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "usage_usec 12345678\nuser_usec 10000000\nsystem_usec 2345678\nnr_periods 900\nnr_throttled 12\nthrottled_usec 345678\n"
	if err := os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	usage, throttled, err := m.Stats("inst1")
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if usage != 12345678 {
		t.Errorf("usage_usec 应为 12345678，实际 %d", usage)
	}
	if throttled != 345678 {
		t.Errorf("throttled_usec 应为 345678，实际 %d", throttled)
	}
}
