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
	if !req.Container {
		t.Fatal("JSON 里的 container=true 没有被解析到 createInstanceReq.Container")
	}

	got := buildCreateInstanceRequest(req, req.CoreType)

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

// 不传 container 时必须是 false —— 默认**不**容器化。
//
// 这条不是废话：容器化会影响启动方式与路径语义（容器内实例目录是 /data），
// 若哪天有人把默认值写成 true，所有既有实例的启动行为都会变。
func TestBuildCreateInstanceRequest_ContainerDefaultsFalse(t *testing.T) {
	var req createInstanceReq
	if err := json.Unmarshal([]byte(`{"instance_id":"x","core_type":"paper"}`), &req); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if req.Container {
		t.Error("未指定 container 时应为 false")
	}
	if got := buildCreateInstanceRequest(req, "paper"); got.Container {
		t.Error("未指定 container 时不该把实例建成容器化")
	}
}
