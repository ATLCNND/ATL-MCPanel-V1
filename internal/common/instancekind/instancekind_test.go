package instancekind

import (
	"strings"
	"testing"
)

func TestKindsAndLabels(t *testing.T) {
	for _, k := range Kinds() {
		if !k.Valid() {
			t.Fatalf("%s 应当在合法类型列表里", k)
		}
		if k.Label() == "" || k.Label() == string(k) {
			t.Fatalf("%s 缺少中文名", k)
		}
	}
	if len(Kinds()) != 3 {
		t.Fatalf("类型数量应为 3（minecraft/docker/console），实际 %d", len(Kinds()))
	}
}

// 0.11.x 建出来的实例没有 template 字段，必须按 minecraft 处理 ——
// 认不出的值同理：退回"什么都能干"的老类型，而不是变成一个没有控制台的空壳。
func TestNormalize(t *testing.T) {
	cases := map[string]Kind{
		"":            KindMinecraft,
		"minecraft":   KindMinecraft,
		"docker":      KindDocker,
		"console":     KindConsole,
		"vanilla":     KindMinecraft, // 认不出 → 老类型
		"Minecraft":   KindMinecraft, // 大小写敏感：不认识的形态一律退回
		"мinecraft":   KindMinecraft,
		" docker ":    KindMinecraft,
		"unknown-x12": KindMinecraft,
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %s，期望 %s", in, got, want)
		}
	}
	if _, ok := Parse("docker"); !ok {
		t.Fatal("Parse(docker) 应当成功")
	}
	if _, ok := Parse("vanilla"); ok {
		t.Fatal("Parse(vanilla) 应当失败（严格模式不做兼容）")
	}
}

// §11.1 第 3 条：docker 与 console 实例下**不出现**日志分析 / 玩家 / 游戏设置 / 配置。
func TestMinecraftOnlyCapabilities(t *testing.T) {
	mcOnly := []Capability{CapLogAnalysis, CapPlayers, CapWorld, CapConfig, CapRCON, CapCore, CapJava}
	for _, k := range []Kind{KindDocker, KindConsole} {
		for _, c := range mcOnly {
			if k.Has(c) {
				t.Errorf("%s 类型不该具备 %s 能力", k, c)
			}
		}
	}
	for _, c := range mcOnly {
		if !KindMinecraft.Has(c) {
			t.Errorf("minecraft 类型应当具备 %s 能力", c)
		}
	}
}

// 三种类型都必须有控制台 / 文件 / 备份 —— §11.3 第 4 条要求"三种模板下控制台、
// 文件、备份页行为一致（不出现某模板下某页签空着没反应）"。
func TestEveryKindHasCoreTabs(t *testing.T) {
	for _, k := range Kinds() {
		for _, c := range []Capability{CapConsole, CapFiles, CapBackup, CapLogs, CapScript} {
			if !k.Has(c) {
				t.Errorf("%s 类型缺少 %s 能力（三种模板的核心页签必须一致）", k, c)
			}
		}
	}
}

// minecraft 不该出现 docker 专属的填项：界面上"填镜像"对 MC 毫无意义。
func TestDockerOnlyCapabilities(t *testing.T) {
	for _, c := range []Capability{CapImage, CapPorts, CapVolumes, CapEnv} {
		if KindMinecraft.Has(c) {
			t.Errorf("minecraft 类型不该具备 %s 能力", c)
		}
		if KindConsole.Has(c) {
			t.Errorf("console 类型不该具备 %s 能力", c)
		}
	}
	if !KindDocker.Has(CapImage) {
		t.Error("docker 类型必须具备镜像能力")
	}
}

func TestCapabilityStringsAreSortedAndCopySafe(t *testing.T) {
	got := KindMinecraft.CapabilityStrings()
	if len(got) == 0 {
		t.Fatal("能力列表不该为空")
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("能力列表应按字典序输出，实际 %v", got)
		}
	}
	// 返回的必须是副本：调用方改了不该影响全局表
	caps := KindMinecraft.Capabilities()
	caps[0] = "tampered"
	if KindMinecraft.Capabilities()[0] == "tampered" {
		t.Fatal("Capabilities() 必须返回副本")
	}
}

func TestContainerSemantics(t *testing.T) {
	if !KindDocker.IsContainerBacked() {
		t.Error("docker 类型必须由容器承载")
	}
	if KindMinecraft.IsContainerBacked() || KindConsole.IsContainerBacked() {
		t.Error("minecraft / console 不强制容器（没有 docker 的节点也要能跑）")
	}
	if !KindMinecraft.SupportsContainerMode() {
		t.Error("只有 minecraft 允许切换容器化开关")
	}
	if KindDocker.SupportsContainerMode() {
		t.Error("docker 实例本身就是容器，没有\"关闭容器化\"这种说法")
	}
}

func TestSpecValidate(t *testing.T) {
	ok := Spec{Image: "nginx:alpine", Ports: []PortMap{{Host: 8080, Container: 80}}}
	if err := ok.Validate(KindDocker); err != nil {
		t.Fatalf("合法规格被拒: %v", err)
	}

	bad := []struct {
		name string
		spec Spec
		kind Kind
		want string
	}{
		{"docker 缺镜像", Spec{}, KindDocker, "请填写容器镜像"},
		{"镜像名带空格", Spec{Image: "nginx alpine"}, KindDocker, "镜像名"},
		{"MC 填了镜像", Spec{Image: "nginx"}, KindMinecraft, "不需要镜像"},
		{"挂载源是绝对路径", Spec{Image: "x", Mounts: []Mount{{Host: "/etc", Target: "/etc"}}}, KindDocker, "实例目录下的相对路径"},
		{"挂载源带 ..", Spec{Image: "x", Mounts: []Mount{{Host: "a/../../etc", Target: "/data"}}}, KindDocker, "实例目录下的相对路径"},
		{"挂载目标非绝对", Spec{Image: "x", Mounts: []Mount{{Host: "data", Target: "data"}}}, KindDocker, "容器内的绝对路径"},
		{"端口越界", Spec{Image: "x", Ports: []PortMap{{Host: 70000, Container: 80}}}, KindDocker, "宿主端口"},
		{"容器端口重复", Spec{Image: "x", Ports: []PortMap{{Host: 1, Container: 80}, {Host: 2, Container: 80}}}, KindDocker, "重复映射"},
		{"环境变量没有等号", Spec{Image: "x", Env: []string{"FOO"}}, KindDocker, "KEY=VALUE"},
		{"环境变量键名非法", Spec{Image: "x", Env: []string{"1BAD=1"}}, KindDocker, "键名"},
		{"环境变量值含换行", Spec{Image: "x", Env: []string{"A=1\nB=2"}}, KindDocker, "不允许换行"},
		{"工作目录非绝对", Spec{Image: "x", WorkingDir: "data"}, KindDocker, "必须是绝对路径"},
		{"console 填了端口", Spec{Ports: []PortMap{{Host: 1, Container: 2}}}, KindConsole, "不支持自定义端口映射"},
	}
	for _, c := range bad {
		err := c.spec.Validate(c.kind)
		if err == nil {
			t.Errorf("%s：应当报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，实际 %q", c.name, c.want, err.Error())
		}
	}
}

// 同端口号的 TCP 与 UDP 可以共存（不该被当成重复）。
func TestSpecValidateUDPNotDuplicate(t *testing.T) {
	s := Spec{Image: "x", Ports: []PortMap{
		{Host: 25565, Container: 25565},
		{Host: 25565, Container: 25565, UDP: true},
	}}
	if err := s.Validate(KindDocker); err != nil {
		t.Fatalf("TCP/UDP 同端口号应当允许: %v", err)
	}
}

func TestSpecEncodeDecodeRoundTrip(t *testing.T) {
	ro := false
	in := Spec{
		Image:      "itzg/minecraft-server:latest",
		Cmd:        []string{"--nogui"},
		Mounts:     []Mount{{Host: "data", Target: "/data", ReadOnly: true}},
		Ports:      []PortMap{{Host: 25565, Container: 25565}},
		Env:        []string{"EULA=true"},
		WorkingDir: "/data",
		ReadOnlyRoot: &ro,
	}
	s, err := in.Encode()
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	if out.Image != in.Image || out.WorkingDir != in.WorkingDir || len(out.Mounts) != 1 || len(out.Ports) != 1 {
		t.Fatalf("往返丢失字段: %+v", out)
	}
	if out.ReadOnlyRoot == nil || *out.ReadOnlyRoot != false {
		t.Fatal("read_only_root 显式 false 必须能往返（否则会被当成默认 true）")
	}
	// 零值不该被写成 "{}"，否则"这一列没有内容"就分不出来
	if s, _ := (Spec{}).Encode(); s != "" {
		t.Fatalf("空规格应编码为空串，实际 %q", s)
	}
	if got, err := DecodeSpec("  "); err != nil || !got.IsZero() {
		t.Fatalf("空串应解出零值，实际 %+v err=%v", got, err)
	}
	if _, err := DecodeSpec("{bad json"); err == nil {
		t.Fatal("坏 JSON 应当报错")
	}
}

func TestMountCleanHostSub(t *testing.T) {
	cases := map[string]string{
		"":       ".",
		".":      ".",
		"./":     ".",
		"data":   "data",
		"./data": "data",
		"a/b/":   "a/b",
	}
	for in, want := range cases {
		if got := (Mount{Host: in}).CleanHostSub(); got != want {
			t.Errorf("CleanHostSub(%q) = %q，期望 %q", in, got, want)
		}
	}
}
