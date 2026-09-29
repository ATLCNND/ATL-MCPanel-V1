package httpapi

import (
	"encoding/json"
	"testing"
)

// 创建实例时**不能漏字段**。
//
// 背景（真实事故）：`POST /api/instances` 的 body 带 `container:true`，但组装
// gRPC 请求时没有转发这个字段，而 Daemon 是按"运行时可用即容器化"执行的 ——
// 于是实例确实跑在容器里，元数据却写着 container=false，监控采集因此走 native
// 分支，面板上 CPU 恒为 0、内存只有十几 MB。
//
// 这类"漏转发一个字段"的错误**不会有任何报错**，只有下游行为诡异，
// 所以这里逐个字段断言，而不是只看 container 一个。
func TestBuildCreateInstanceRequest_ForwardsAllFields(t *testing.T) {
	body := []byte(`{
		"node_id": 7,
		"instance_id": "beta99",
		"name": "内测-Beta99",
		"mc_type": "paper",
		"core_type": "paper",
		"java_version": "21",
		"port": 25599,
		"max_mem": "2G",
		"min_mem": "512M",
		"jar_url": "/opt/atl-node/resources/paper.jar",
		"start_command": "java -jar {jar}",
		"cpu_quota": 150,
		"backup_dir": "/data/backups",
		"mem_limit": "3G",
		"container": true
	}`)

	var req createInstanceReq
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}

	// JSON 侧先断言一次：字段名写错（比如改成 is_container）会让前端静默失效
	if req.Container == nil || !*req.Container {
		t.Fatal("JSON 里的 container=true 没有被解析到 createInstanceReq.Container")
	}

	got := buildCreateInstanceRequest(req, req.CoreType, false)

	if !got.Container {
		t.Error("container 没有被转发给 Daemon：实例会在容器里跑、元数据却是 false，" +
			"监控随之按 native 采集（表现为 CPU 恒 0）")
	}
	if got.InstanceId != "beta99" || got.Name != "内测-Beta99" {
		t.Errorf("实例标识字段没转发对: id=%q name=%q", got.InstanceId, got.Name)
	}
	if got.McType != "paper" || got.CoreType != "paper" {
		t.Errorf("核心类型没转发对: mc_type=%q core_type=%q", got.McType, got.CoreType)
	}
	if got.JavaVersion != "21" || got.Port != 25599 {
		t.Errorf("java_version/port 没转发对: %q %d", got.JavaVersion, got.Port)
	}
	if got.MaxMem != "2G" || got.MinMem != "512M" {
		t.Errorf("内存字段没转发对: max=%q min=%q", got.MaxMem, got.MinMem)
	}
	if got.JarUrl != "/opt/atl-node/resources/paper.jar" || got.StartCommand != "java -jar {jar}" {
		t.Errorf("jar/启动命令没转发对: jar=%q cmd=%q", got.JarUrl, got.StartCommand)
	}
	if got.CpuQuota != 150 || got.MemLimit != "3G" || got.BackupDir != "/data/backups" {
		t.Errorf("配额/上限/备份目录没转发对: cpu=%d mem=%q backup=%q",
			got.CpuQuota, got.MemLimit, got.BackupDir)
	}
}

// container 字段是**三态**，三条路径都要锁住。
//
// 为什么不能是普通 bool：容器化现在是默认方式，而"默认开启"必须由请求体的
// **缺席**（nil）触发，不能靠零值 —— 零值无法区分"用户主动选了原生进程"和
// "旧客户端根本没提这件事"。老客户端没提就被改掉启动方式，正是上面那起事故
// 的形态（元数据与事实不一致，且不报错）。
//
//  1. 不传（nil）+ 节点有容器能力  → 容器化（默认开启）
//  2. 不传（nil）+ 节点无容器能力  → 原生进程（可用性优先，不猜）
//  3. 显式 false                  → 原生进程（用户的明确选择必须被尊重）
func TestResolveContainer_TriState(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name          string
		explicit      *bool
		nodeDefault   bool
		wantContainer bool
	}{
		{"不传 + 节点支持 → 默认容器化", nil, true, true},
		{"不传 + 节点不支持 → 原生进程", nil, false, false},
		{"显式 false 压过节点默认 → 原生进程", &no, true, false},
		{"显式 true 即使节点默认关闭也照传 → 容器化", &yes, false, true},
	}
	for _, c := range cases {
		if got := resolveContainer(c.explicit, c.nodeDefault); got != c.wantContainer {
			t.Errorf("%s：得到 container=%v，期望 %v", c.name, got, c.wantContainer)
		}
	}
}

// 不传 container 时，实际结果等于节点默认值 —— 这条用组装后的 gRPC 请求再验一次，
// 因为 resolveContainer 返回对了、却没接到 pb 字段上，同样是静默失效。
func TestBuildCreateInstanceRequest_OmittedContainerFollowsNodeDefault(t *testing.T) {
	var req createInstanceReq
	if err := json.Unmarshal([]byte(`{"instance_id":"x","core_type":"paper"}`), &req); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.Container != nil {
		t.Error("请求体没写 container 时应当是 nil（未表态），而不是某个零值")
	}
	if got := buildCreateInstanceRequest(req, "paper", true); !got.Container {
		t.Error("节点支持容器化时，未表态的创建请求应当容器化（默认开启）")
	}
	if got := buildCreateInstanceRequest(req, "paper", false); got.Container {
		t.Error("节点不支持容器化时，未表态的创建请求应当以原生进程运行")
	}
}

// 显式 container=false 必须能压过"默认开启"。
//
// 这不是边角用例：默认改成立即容器化之后，`false` 是老配置、脚本与排障时
// "我就不要容器"的唯一表达方式。它一旦失效，用户没有任何办法要一个原生实例。
func TestBuildCreateInstanceRequest_ExplicitFalseWins(t *testing.T) {
	var req createInstanceReq
	if err := json.Unmarshal([]byte(`{"instance_id":"x","core_type":"paper","container":false}`), &req); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.Container == nil {
		t.Fatal("显式 false 被解析成了 nil：无法区分『主动关掉』与『没表态』")
	}
	if got := buildCreateInstanceRequest(req, "paper", true); got.Container {
		t.Error("显式 container=false 没有被尊重：实例会被建成容器化")
	}
}
