package grpcapi

import (
	"context"
	"regexp"
	"strings"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// maxCPUQuotaPct 面板允许设置的最大 CPU 配额（百分比，100 = 1 核）。
//
// 为什么要封顶而不是"交给用户填"：这个值最终会被写进 cgroup 的 `cpu.max`
//（`<quota> <period>` 里的 quota），一个荒谬的数字（比如 10^9）写下去不会报错，
// 只会让该实例把整台机器的 CPU 吃干 —— 而受害的是同节点的其它实例。
// 6400% = 64 核，比任何一台单机节点的核数都高，正常场景永远撞不到这个上限。
const maxCPUQuotaPct = 6400

// memLimitRe cgroup 内存上限的合法形态。
//
// 只接受"数字 + 可选单位"：`4G` / `512M` / `1073741824` / `2g` / `512mB`。
// **必须严格**：这个字符串会被 Daemon 以 root 身份写进 `/sys/fs/cgroup/**/memory.max`
//（或 v1 的 `memory.limit_in_bytes`）—— 带空格、分号、斜杠、`..` 的取值要么让写入
// 落到别的文件上，要么写进去之后内核解析失败而限制悄悄失效。
var memLimitRe = regexp.MustCompile(`^[0-9]+([kKmMgGtT])?[bB]?$`)

// SetInstanceLimits 修改实例的 CPU / 内存上限（下次启动生效）。
//
// 为什么需要这条 RPC（2026-10-02）：这两个值原先**只在建实例时**写进节点的
// instance.json，面板此后再也没有办法改 —— 节点用户建实例时把内存留空
//（= 不限制），运营侧就永远收不回来。面板侧现在有「资源上限」的修改入口，
// 它改完库里的值之后必须**同时**把新值送到节点，否则就是典型的一改就假生效：
// 界面上显示 4G，cgroup 里还是 unlimited。
//
// 生效时机只能如实说：cgroup 的 cpu.max / memory.max 是**启动时**由 Daemon 写入的
//（见 mcprocess 的 Apply / SetMemoryLimit），运行中的 JVM 改不了自己脚下的配额，
// 所以对正在跑的实例就是"重启后生效" —— 与换 JDK 的行为完全一致，返回消息里会明说。
func (s *Server) SetInstanceLimits(ctx context.Context, req *pb.SetInstanceLimitsRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}

	cpu := int(req.CpuQuota)
	if cpu < 0 || cpu > maxCPUQuotaPct {
		return &pb.OperationResponse{Success: false, Error: "CPU 配额不合法：应为 0（不限制）到 6400（百分比，100 = 1 核）之间的整数"}, nil
	}
	mem := strings.TrimSpace(req.MemLimit)
	if mem != "" && (len(mem) > 32 || !memLimitRe.MatchString(mem)) {
		return &pb.OperationResponse{Success: false, Error: "内存上限不合法：应形如 4G / 512M，或留空表示不限制"}, nil
	}

	if err := s.reg.SetLimits(req.InstanceId, cpu, mem); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	s.log.Info("实例资源上限已更新", "instance", req.InstanceId,
		"cpu_quota", cpu, "mem_limit", mem, "running", inst.Status() == "running")
	msg := "资源上限已更新，重启实例后生效"
	if inst.Status() == "running" {
		msg += "（当前进程仍在用启动时施加的 cgroup 限制）"
	}
	return &pb.OperationResponse{Success: true, Message: msg}, nil
}
