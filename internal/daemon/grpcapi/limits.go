package grpcapi

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// maxCPUQuotaPct 面板允许设置的最大 CPU 配额（百分比，100 = 1 核）。
//
// 为什么要封顶而不是"交给用户填"：这个值最终会被写进 cgroup 的 `cpu.max`
// （`<quota> <period>` 里的 quota），一个荒谬的数字（比如 10^9）写下去不会报错，
// 只会让该实例把整台机器的 CPU 吃干 —— 而受害的是同节点的其它实例。
// 6400% = 64 核，比任何一台单机节点的核数都高，正常场景永远撞不到这个上限。
const maxCPUQuotaPct = 6400

// memLimitRe cgroup 内存上限的合法形态。
//
// 只接受"数字 + 可选单位"：`4G` / `512M` / `1073741824` / `2g` / `512mB`。
// **必须严格**：这个字符串会被 Daemon 以 root 身份写进 `/sys/fs/cgroup/**/memory.max`
// （或 v1 的 `memory.limit_in_bytes`）—— 带空格、分号、斜杠、`..` 的取值要么让写入
// 落到别的文件上，要么写进去之后内核解析失败而限制悄悄失效。
var memLimitRe = regexp.MustCompile(`^[0-9]+([kKmMgGtT])?[bB]?$`)

// SetInstanceLimits 修改实例的 CPU / 内存上限（下次启动生效）。
//
// 为什么需要这条 RPC（2026-10-02）：这两个值原先**只在建实例时**写进节点的
// instance.json，面板此后再也没有办法改 —— 节点用户建实例时把内存留空
// （= 不限制），运营侧就永远收不回来。面板侧现在有「资源上限」的修改入口，
// 它改完库里的值之后必须**同时**把新值送到节点，否则就是典型的一改就假生效：
// 界面上显示 4G，cgroup 里还是 unlimited。
//
// 生效时机只能如实说：cgroup 的 cpu.max / memory.max 是**启动时**由 Daemon 写入的
// （见 mcprocess 的 Apply / SetMemoryLimit），运行中的 JVM 改不了自己脚下的配额，
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

// memValueRe JVM 堆取值（-Xmx / -Xms）的合法形态。
//
// 与 cgroup 的 memLimitRe 区分开：这个值**不进内核**，它是拼进 `-Xmx4G` 这类
// 启动参数的字符串，所以只要求"数字 + 可选单位"即可（`4G` / `1536m` /
// `512M`），不必像 memLimitRe 那样容忍纯字节数写法 —— 但**同样必须严格**：
// 它会出现在启动命令里，带空格/分号/`$()` 的取值等于往命令里注入内容。
// `-Xmx` 被写成负数或 0 时 JVM 直接启动失败，故单位前的数字必须 > 0。
var memValueRe = regexp.MustCompile(`^[0-9]+([kKmMgGtT])?$`)

// SetInstanceJvmMem 修改实例的 JVM 堆上限 / 初始堆（下次启动生效）。
//
// 为什么需要它（2026-10-04 用户反馈"实例配额修改没法修改 JVM 内存"）：
// max_mem / min_mem 原先与 cpu_quota / mem_limit 一样，**只在建实例时**写进
// instance.json，之后面板没有任何入口能改 —— 想给整合包多分点堆只能删库重建。
//
// 与 SetInstanceLimits 的关键区别（写给下一个改这里的人）：
//   - 这里改的是**启动命令里的 -Xmx/-Xms**（JVM 堆）；
//   - SetInstanceLimits 改的是 **cgroup 的 memory.max**（整个进程的天花板）。
//
// 两者都必须满足"堆 < cgroup 上限"，但**校验不在这里做**：面板侧才知道另一个值
// 是多少，而且用户完全可能先改堆、再改配额。所以这里如实保存，由面板界面提示
// 关系（建议 -Xmx×1.3+512MB），避免在 Daemon 侧拒绝一个用户稍后会补齐的组合。
func (s *Server) SetInstanceJvmMem(ctx context.Context, req *pb.SetInstanceJvmMemRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}

	maxMem := strings.TrimSpace(req.MaxMem)
	minMem := strings.TrimSpace(req.MinMem)
	if !validJvmMem(maxMem) {
		return &pb.OperationResponse{Success: false,
			Error: "JVM 堆上限不合法：应形如 4G / 1536m（数字 + 可选单位 K/M/G/T），且必须大于 0"}, nil
	}
	if minMem != "" && !validJvmMem(minMem) {
		return &pb.OperationResponse{Success: false,
			Error: "JVM 初始堆不合法：应形如 1G / 512m（可留空，留空则由 Daemon 按默认值处理）"}, nil
	}

	if err := s.reg.SetJvmMem(req.InstanceId, maxMem, minMem); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}

	s.log.Info("实例 JVM 堆已更新", "instance", req.InstanceId,
		"max_mem", maxMem, "min_mem", minMem, "running", inst.Status() == "running")
	msg := "JVM 堆（-Xmx/-Xms）已更新为 -Xmx" + maxMem + memNote(minMem) + "，重启实例后生效"
	if inst.Status() == "running" {
		msg += "（当前进程仍跑在启动时的那份堆配置上）"
	}
	return &pb.OperationResponse{Success: true, Message: msg}, nil
}

// validJvmMem 校验 JVM 堆取值：空串允许（由调用方决定语义），非空时必须合法且 > 0。
func validJvmMem(v string) bool {
	if v == "" {
		return false // 调用方对空串有各自的判断；-Xmx 不能为空
	}
	if len(v) > 16 || !memValueRe.MatchString(v) {
		return false
	}
	return memBytes(v) > 0
}

// memBytes 把 "4G" / "1536m" 换算成字节（仅用于判断 > 0）。
func memBytes(v string) int64 {
	n := len(v)
	mult := int64(1)
	switch v[n-1] {
	case 'k', 'K':
		mult, v = 1<<10, v[:n-1]
	case 'm', 'M':
		mult, v = 1<<20, v[:n-1]
	case 'g', 'G':
		mult, v = 1<<30, v[:n-1]
	case 't', 'T':
		mult, v = 1<<40, v[:n-1]
	}
	num, err := strconv.ParseInt(v, 10, 64)
	if err != nil || num <= 0 {
		return 0
	}
	return num * mult
}

// memNote 拼返回消息里的初始堆部分（留空时不显示）。
func memNote(minMem string) string {
	if minMem == "" {
		return ""
	}
	return " -Xms" + minMem
}
