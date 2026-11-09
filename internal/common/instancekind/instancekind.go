// Package instancekind 定义「实例类型（模板）」以及每种类型**能做什么**。
//
// 这个包是**唯一**的判据来源，面板、Daemon、前端（经 API 拿到的 capabilities
// 列表）都读它 —— 前端不再写 `if template === 'minecraft'` 这类分支。
//
// 为什么要这一层（docs/V1.0-PREVIEW-GOALS.md §11.1 第 6 条）：
// 0.12.x 试图用「同一套实例模型 + if template 兼容」来支持多类型，结果是每加一个
// 功能都要在同一个函数里再插一条分支，且分支之间互相不知道对方存在（选 docker 后
// 仍然显示 Java 版本、JVM 内存、核心 jar、启动命令）。这里把「类型能做什么」变成
// 数据（能力集合），谁要加功能就declare 它依赖哪个能力，而不是到处问"你是什么类型"。
//
// 三条硬规则：
//  1. 能力集合只由类型决定，不因节点的可用性而变（节点没装 docker 时，docker 实例
//     是"起不来"，不是"变成另一种类型"）。
//  2. 能力是**功能开关**，不是权限：能不能点由面板的角色/可见性另外决定。
//  3. 新增功能必须先在这里登记能力，再在需要它的类型上打开；**不许**在业务代码里
//     再写模板名判断。
package instancekind

import "sort"

// Kind 实例类型（模板）。
type Kind string

const (
	// KindMinecraft 这是本面板的立身之本：Java 服务端（Paper/Folia/Forge…），
	// 有 RCON、世界、玩家、server.properties 这些概念。
	KindMinecraft Kind = "minecraft"
	// KindDocker 通用 Docker：给镜像 + 端口 + 卷 + 环境变量，跑任何东西。
	// 面板不认识容器里的业务，因此不提供玩家/世界/核心/Rcon 这类理解层功能。
	KindDocker Kind = "docker"
	// KindConsole 通用控制台：不预设任何业务，只提供能收发 stdin/stdout 的界面，
	// 可挂到自定义脚本（配合 start.sh 使用）。
	KindConsole Kind = "console"
)

// Capability 能力标识。
//
// 取值刻意用「用户看得见的东西」命名（console/files/backup），而不是「实现细节」
// （rcon_client/sftp），因为前端就是按它决定显示哪些页签的。
type Capability string

const (
	// CapConsole 能收发 stdin/stdout 的控制台。
	CapConsole Capability = "console"
	// CapFiles 文件管理（浏览/上传/下载/编辑/解压）。
	CapFiles Capability = "files"
	// CapBackup 备份与回滚。
	CapBackup Capability = "backup"
	// CapLogs 控制台日志的读取与归档（历史日志文件）。
	CapLogs Capability = "logs"
	// CapLogAnalysis AI 日志分析：需要 Minecraft 的日志形态与崩溃特征。
	CapLogAnalysis Capability = "log_analysis"
	// CapJava JDK 版本选择与 JVM 内存（-Xmx/-Xms）。
	CapJava Capability = "java"
	// CapCore 服务端核心（jar）选择与下载。
	CapCore Capability = "core"
	// CapRCON RCON 通道（面板用它执行服务端命令、取 TPS 等）。
	CapRCON Capability = "rcon"
	// CapWorld 世界信息（种子/难度/时间）与 gamerule。
	CapWorld Capability = "world"
	// CapPlayers 在线玩家列表（依赖 RCON 或日志解析）。
	CapPlayers Capability = "players"
	// CapConfig server.properties 这类「游戏配置」的图形化编辑。
	CapConfig Capability = "config"
	// CapImage 容器镜像（通用 Docker 才有：镜像就是它的"核心"）。
	CapImage Capability = "image"
	// CapPorts 端口映射（通用 Docker 可映射多个）。
	CapPorts Capability = "ports"
	// CapVolumes 卷挂载（通用 Docker）。
	CapVolumes Capability = "volumes"
	// CapEnv 环境变量（通用 Docker）。
	CapEnv Capability = "env"
	// CapScript 启动脚本（start.sh / 自定义命令）。
	CapScript Capability = "script"
)

// all 每种类型的完整能力集合。
//
// 注意 minecraft **不含** image/ports/volumes/env：MC 实例的镜像与端口由面板自己
// 决定（基础镜像 + 单端口），不是用户填的；把它算进去会让界面出现"填镜像"这种
// 对 MC 无意义的字段。
var all = map[Kind][]Capability{
	KindMinecraft: {
		CapConsole, CapFiles, CapBackup, CapLogs, CapLogAnalysis,
		CapJava, CapCore, CapRCON, CapWorld, CapPlayers, CapConfig, CapScript,
	},
	KindDocker: {
		CapConsole, CapFiles, CapBackup, CapLogs,
		CapImage, CapPorts, CapVolumes, CapEnv, CapScript,
	},
	KindConsole: {
		CapConsole, CapFiles, CapBackup, CapLogs, CapScript,
	},
}

// Kinds 返回全部类型（顺序稳定：minecraft → docker → console）。
func Kinds() []Kind { return []Kind{KindMinecraft, KindDocker, KindConsole} }

// Normalize 把外部传进来的字符串收敛成合法 Kind。
//
// 空串按 minecraft 处理（0.11.x 及更早建出来的实例都没有这个字段，它们全是 MC）；
// 认不出的值也退回 minecraft —— 宁可显示成 MC 也不要把一个实例变成"没有控制台"
// 的空壳，那会让人以为实例坏了。调用方若需要严格校验，用 Parse。
func Normalize(s string) Kind {
	if k, ok := Parse(s); ok {
		return k
	}
	return KindMinecraft
}

// Parse 严格解析类型名。
func Parse(s string) (Kind, bool) {
	k := Kind(s)
	if _, ok := all[k]; ok {
		return k, true
	}
	return "", false
}

// Valid 该类型是否合法。
func (k Kind) Valid() bool {
	_, ok := all[k]
	return ok
}

// String 实现 fmt.Stringer（日志里直接用）。
func (k Kind) String() string { return string(k) }

// Label 类型的中文名（用于日志与界面）。
func (k Kind) Label() string {
	switch k {
	case KindMinecraft:
		return "Minecraft 服务端"
	case KindDocker:
		return "通用 Docker"
	case KindConsole:
		return "通用控制台"
	}
	return string(k)
}

// Capabilities 返回该类型的全部能力（副本，调用方可随意改）。
func (k Kind) Capabilities() []Capability {
	src := all[Normalize(string(k))]
	out := make([]Capability, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CapabilityStrings 返回字符串形式的能力列表（给 JSON API / proto 用）。
func (k Kind) CapabilityStrings() []string {
	caps := k.Capabilities()
	out := make([]string, len(caps))
	for i, c := range caps {
		out[i] = string(c)
	}
	return out
}

// Has 该类型是否具备某项能力。
func (k Kind) Has(c Capability) bool {
	for _, x := range all[Normalize(string(k))] {
		if x == c {
			return true
		}
	}
	return false
}

// IsContainerBacked 该类型是否**必须**用容器运行。
//
// minecraft 与 console 两种类型都可以在宿主上直接跑（老装机、没有 docker 的节点
// 也要能用）；docker 类型的定义就是"跑一个容器"，所以它必须有容器运行时。
func (k Kind) IsContainerBacked() bool { return Normalize(string(k)) == KindDocker }

// SupportsContainerMode 该类型是否允许**选择**容器化隔离。
//
// 只有 minecraft 有这个选项（0.11.x 起就有：同一套 java 命令行搬进容器执行）；
// docker 天生是容器（没有"关掉"的说法），console 则按脚本形态运行。
func (k Kind) SupportsContainerMode() bool { return Normalize(string(k)) == KindMinecraft }
