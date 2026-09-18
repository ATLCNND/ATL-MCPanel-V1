// Package mcprocess 管理 MC 服务端进程（启动/停止/控制台 IO 桥接）。
package mcprocess

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/javaruntime"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
)

// Instance MC 进程实例（核心无关）。
type Instance struct {
	ID           string
	Dir          string // 实例根目录（含 jar、world 等）
	JarPath      string // 服务端 jar 路径（可空，用于默认模板）
	StartCommand string // 自定义启动命令模板（空则用默认 java -jar）
	JavaArgs     []string
	MaxMem       string
	MinMem       string
	// Port 实例的游戏端口（面板建实例时指定，之后不可改）。
	//
	// 它是**服务端应该监听的端口**，隧道的 local_port 也取自它；
	// 启动前由 syncServerPort 落进 server.properties —— 详见 serverport.go。
	Port int
	// JavaVersion 期望使用的 JDK（主版本号如 "21"，或直接的 java 路径）。
	//
	// 空 → 用 PATH 上的 java。非空但解析不到对应 JDK 时也会回退到 PATH，
	// 并把回退原因记进日志与启动提示 —— "选了 17 却跑 21" 必须让人看得见，
	// 静默回退比启动失败更难排查。
	JavaVersion string

	mu        sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	logFile   *os.File      // 控制台日志文件
	status    string        // running / stopped / starting / error
	subMu     sync.Mutex    // 独立锁保护订阅者列表（避免与 mu 嵌套导致死锁）
	subs      []*consoleSub // 控制台输出订阅者
	started   bool
	startMode string // 实际使用的启动方式：start.sh / custom / default

	// pidFile 记录子进程 PID（供 Daemon 重启后接管）。
	//
	// **不在实例目录里**：它虽然只是一个数字，却被 root 的 Daemon 当作
	// "这个 pid 就是我的实例进程"来信任（接管时会按它认领进程，停止时会向它
	// 的进程组发信号）。放在实例用户可写的目录里，等于把"向任意进程组发信号"
	// 的能力交给租户 —— 例如把 panel 或 sshd 的 pid 写进去。
	// 因此它属于平台状态，放在 <state_dir>/<实例ID>/ 下（root 0700）。
	pidFile string
	// StateDir 平台状态目录（<state_dir>/<实例ID>），与实例目录分开，见 pidFile。
	StateDir string

	// RunAsFor 在启动时解析该实例的运行身份（由 Daemon 注入）。
	//
	// 是**函数**而不是静态值：per-instance 用户可能在 Daemon 运行期间才被创建
	//（先建实例、后补用户的运维顺序很常见），每次启动现解析能自动跟上；
	// 返回错误时启动会被拒绝（见 Start），不会静默退回 root。
	RunAsFor func(instanceID string) (*runas.Identity, error)

	adoptedPID int // 非 0 表示接管 Daemon 重启前遗留的进程

	// CPUQuotaPercent 实例的 CPU 配额（百分比，100 = 1 核；0 = 不限制）
	CPUQuotaPercent int
	// BackupDir 该实例备份的存放目录（空则回退到实例目录下的 backups/）
	BackupDir string
	// MemLimitBytes 实例的 cgroup 内存上限（0 = 不限制）。
	//
	// 注意：这不是 JVM 的 -Xmx —— -Xmx 只管堆，进程还有元空间、直接内存、
	// 线程栈等堆外开销。因此上限必须比 -Xmx 留出余量，
	// 否则 JVM 会因为堆外内存被 OOM 杀掉。建议 -Xmx × 1.3 + 512MB。
	MemLimitBytes int64
	// Limiter 资源限制实现（cgroup），为 nil 时使用全局默认
	Limiter ResourceLimiter

	// Container 容器运行时；非 nil 表示该实例跑在 docker 容器里。
	//
	// 与 native 模式的区别集中在三处：起（argv 换成 docker run）、
	// 杀（必须 docker rm -f，kill CLI 杀不掉容器）、限额（交给 Docker，
	// Daemon 不再写 cgroup —— 两边都写会静默失效）。
	// stdin/stdout 这两条链路完全不变，因为 docker CLI 仍是子进程。
	Container *container.Runtime
	// ContainerResourcesDir 只读挂入的节点共享资源目录（如 /opt/atl-node/resources）。
	ContainerResourcesDir string
	// containerized 本次运行是否真的走了容器（供状态展示与限额归属判断）。
	containerized bool
	// wantContainer 元数据要求该实例容器化（即使运行时暂不可用）。
	wantContainer bool

	limitWarn string // 最近一次资源限制应用失败的原因
	// javaNote 最近一次 Java 版本解析的说明（回退时会写明原因）
	javaNote string
	// stopRecorded 标记本次停机是否已由 Stop/Kill 记入统计。
	// 用于区分「面板主动停止」与「进程自行崩溃」——后者需要由退出监视器补记，
	// 否则崩溃不计入关机次数，统计会偏低。
	stopRecorded bool
}

// ResourceLimiter 资源限制接口（由 cgroup 包实现；测试可注入桩）。
type ResourceLimiter interface {
	Apply(instanceID string, cpuPercent int) error
	Assign(instanceID string, pid int) error
	// AssignTree 把进程及其后代一并纳入限制（可选；未实现时回退到 Assign）
	AssignTree(instanceID string, pid int) (int, error)
	// SetMemoryLimit 设置 cgroup memory.max（limitBytes<=0 表示不限制）
	SetMemoryLimit(instanceID string, limitBytes int64) error
}

// LifecycleHook 在实例真正启动成功 / 停止完成时回调。
//
// 启停计数的正确挂载点是**进程生命周期**而非 gRPC 处理器：
// 重启（内部直接 Stop+Start）、删除前停止、崩溃恢复等路径都不会
// 经过 StartInstance/StopInstance，挂在处理器上会漏计。
type LifecycleHook func(instanceID string, started bool)

var lifecycleHook LifecycleHook

// SetLifecycleHook 注入生命周期回调（Daemon 启动时调用一次）。
func SetLifecycleHook(h LifecycleHook) { lifecycleHook = h }

func fireLifecycle(id string, started bool) {
	if lifecycleHook != nil {
		lifecycleHook(id, started)
	}
}

// ExitHook 在实例进程**真正退出**后回调（不含"刚收到停止请求"）。
//
// 与 LifecycleHook 的区别很重要：
//   - LifecycleHook(id,false) 在 Stop()/Kill() 被**调用**时就记一次关机，
//     用于启停计数（用户一按停止就算关机，符合直觉）
//   - ExitHook 只在进程确实消失后触发，供**必须与进程同生共死**的外部资源收尾
//
// 目前的唯一用途是 frpc（隧道）。此前隧道是在 StopInstance 处理器里顺手停掉的，
// 而 `Stop()` 是"发完 stop 指令就返回"（见其注释），于是实例还在存盘、隧道已经断了：
// 正常服务器几秒就退出、看不出来；服务端**没在读控制台**时（首次启动下载依赖、
// JVM 卡住、插件死锁）这个错配会持续很久 —— 面板上"实例 running、隧道 stopped"，
// 用户看到的是一个连不上的公网地址。
type ExitHook func(instanceID string)

var exitHook ExitHook

// SetExitHook 注入进程退出回调（Daemon 启动时调用一次）。
func SetExitHook(h ExitHook) { exitHook = h }

func fireExit(id string) {
	if exitHook != nil {
		exitHook(id)
	}
}

// SetResourceLimiter 注入全局资源限制实现（Daemon 启动时调用一次）。
func SetResourceLimiter(l ResourceLimiter) { defaultLimiter = l }

var defaultLimiter ResourceLimiter

// applyResourceLimit 在进程启动后施加 CPU 配额。
// 失败只记录状态，**不终止实例** —— 资源限制不应成为服务可用性的单点。
func (i *Instance) applyResourceLimit(pid int) string {
	lim := i.Limiter
	if lim == nil {
		lim = defaultLimiter
	}
	if lim == nil {
		return ""
	}
	if err := lim.Apply(i.ID, i.CPUQuotaPercent); err != nil {
		return "应用 CPU 配额失败: " + err.Error()
	}
	// 先移入进程本身，随后再收敛子进程（见 reconcileLimit）
	if err := lim.Assign(i.ID, pid); err != nil {
		return "加入 cgroup 失败: " + err.Error()
	}
	// 内存上限（0 = 不限制）。与 CPU 配额一样，失败不终止实例。
	if err := lim.SetMemoryLimit(i.ID, i.MemLimitBytes); err != nil {
		return "应用内存上限失败: " + err.Error()
	}

	// 以 start.sh 启动时，脚本会在被移入 cgroup 之前就 fork 出真正的负载，
	// 因此需要在随后一小段时间内反复把新出现的子进程收进 cgroup。
	go i.reconcileLimit(pid)
	return ""
}

// reconcileLimit 周期性把新出现的子进程收进 cgroup。
//
// 背景：cgroup 成员按进程记录，移动父进程不会带上已存在的子进程。
// 这里在启动后的一段窗口内多次收敛，覆盖"脚本先 fork 再执行"的情况。
func (i *Instance) reconcileLimit(pid int) {
	lim := i.Limiter
	if lim == nil {
		lim = defaultLimiter
	}
	if lim == nil {
		return
	}
	// 依次在 100ms / 400ms / 1.2s / 3s 收敛；脚本的 fork 通常发生在前几毫秒
	for _, delay := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 800 * time.Millisecond, 1800 * time.Millisecond} {
		time.Sleep(delay)
		// 实例已停止或被别的进程取代则不再处理
		i.mu.Lock()
		alive := i.cmd != nil && i.cmd.Process != nil && i.cmd.Process.Pid == pid
		i.mu.Unlock()
		if !alive {
			return
		}
		if _, err := lim.AssignTree(i.ID, pid); err != nil {
			i.mu.Lock()
			// 进程在这几毫秒里刚好退出时，写 cgroup.procs 会得到 ESRCH
			//（"no such process"）。这**不是**"配额没生效"，只是没东西可收了 ——
			// 秒退的实例（崩了、jar 不对、缺 EULA）很容易命中。
			//
			// 这条以前没人看得见（limitWarn 没有出口），2026-09-15 把它接到
			// 界面之后立刻就在临时实例上冒出来了：一个已经停掉的实例显示
			// "资源限制未生效"，属于纯噪音。所以这里按"进程是否还在"区分。
			gone := !IsAlive(pid)
			if gone {
				i.limitWarn = ""
			} else {
				i.limitWarn = "收敛子进程到 cgroup 失败: " + err.Error()
			}
			i.mu.Unlock()
			if gone {
				return
			}
		}
	}
}

// LimitWarning 返回最近一次资源限制应用失败的原因（空表示正常）。
func (i *Instance) LimitWarning() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.limitWarn
}

// StartMode 返回最近一次启动实际使用的方式（start.sh / custom / default）。
func (i *Instance) StartMode() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.startMode
}

// StartCommandTemplate 返回自定义启动命令模板。
func (i *Instance) StartCommandTemplate() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.StartCommand
}

// Memory 返回 (max, min) 内存设置。
func (i *Instance) Memory() (string, string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.MaxMem, i.MinMem
}

// RenderStartCommand 返回按当前配置渲染后的启动命令（不实际执行）。
// scriptActive 为 true 时表示实际会运行实例目录下的 start.sh。
func (i *Instance) RenderStartCommand() (command string, scriptActive bool, scriptPath string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	sp := filepath.Join(i.Dir, "start.sh")
	if fi, err := os.Stat(sp); err == nil && !fi.IsDir() {
		return "sh " + sp, true, sp
	}
	if i.StartCommand != "" {
		return i.renderCommand(i.StartCommand), false, sp
	}
	return fmt.Sprintf("java -Xms%s -Xmx%s -jar %s nogui",
		normMem(i.MinMem), normMem(i.MaxMem), i.JarPath), false, sp
}

// NewInstance 创建实例管理器。
//
// stateDir 是**平台状态目录**（<state_dir>/<实例ID>），用于存放 daemon.pid 这类
// 被 root 信任的文件；它必须与 dir（实例目录）分开，理由见 pidFile 字段说明。
func NewInstance(id, dir, stateDir, jarPath, maxMem, minMem string) *Instance {
	return &Instance{
		ID:       id,
		Dir:      dir,
		StateDir: stateDir,
		JarPath:  jarPath,
		MaxMem:   maxMem,
		MinMem:   minMem,
		status:   "stopped",
		pidFile:  filepath.Join(stateDir, "daemon.pid"),
	}
}

// NewAdopted 接管一个 Daemon 重启前遗留、仍在运行的进程。
// 由于 stdin 管道已丢失，此类实例无法接收控制台命令（只读输出），但支持停止。
func NewAdopted(id, dir, stateDir, jarPath, maxMem, minMem, startCommand string, pid int) *Instance {
	inst := NewInstance(id, dir, stateDir, jarPath, maxMem, minMem)
	inst.StartCommand = startCommand
	inst.adoptedPID = pid
	inst.status = "running"
	// 接管实例仍可通过 tail 日志文件提供只读控制台输出
	go inst.tailLog(filepath.Join(dir, "logs", "console.log"))
	go inst.watchAdopted(pid)
	return inst
}

// Adopted 是否处于接管状态（无 stdin 管道）。
func (i *Instance) Adopted() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.adoptedPID != 0
}

// SetJarPath 更新实例使用的核心 jar 路径（重启后生效）。
func (i *Instance) SetJarPath(p string) {
	i.mu.Lock()
	i.JarPath = p
	i.mu.Unlock()
}

// Status 返回当前状态。
func (i *Instance) Status() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// PID 返回进程 PID（未运行返回 0）。
func (i *Instance) PID() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adoptedPID != 0 && IsAlive(i.adoptedPID) {
		return i.adoptedPID
	}
	if i.cmd != nil && i.cmd.Process != nil && i.status == "running" {
		return i.cmd.Process.Pid
	}
	return 0
}

// IsAlive 判断 PID 是否存活。
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// 信号 0 仅做存在性检查
	return syscall.Kill(pid, 0) == nil
}

// Start 启动实例进程（核心无关，支持自定义命令或默认 java -jar）。
func (i *Instance) Start() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.status == "running" || i.status == "starting" {
		return fmt.Errorf("实例已在运行")
	}

	// 启动前检查孤儿进程：若实例目录下仍有 java 在跑（例如上次强制关闭
	// 未清理干净、或 Daemon 的 PID 记录与实际不符），直接启动会撞上
	// Minecraft 的 session.lock 并报出难以理解的 "already locked"。
	// 这里提前拦住并给出可操作的提示。
	if pid, found := FindOrphan(i.Dir); found {
		return orphanStartError(pid)
	}

	// 容器模式额外检查：同名容器是否还在。
	//
	// native 模式靠 PID 记录判断"上一次的进程还在不在"，容器模式不能这么判断：
	// 容器是 dockerd 管的独立进程，Daemon 崩溃后它照跑。所以这里直接问 docker。
	if i.wantContainer && i.Container == nil {
		// 元数据说要容器化，但节点上没有可用的 docker。
		// 不静默退回 native：那会让"面板显示容器化、实际没有隔离"成为可能。
		i.status = "error"
		return fmt.Errorf("该实例已启用容器化隔离，但节点上没有可用的容器运行时（docker）：" +
			"请先在节点上安装 docker 并导入基础镜像（部署脚本会做），" +
			"或在面板上关闭该实例的容器化")
	}
	if i.Container != nil {
		st, err := i.Container.Inspect(context.Background(), i.ID)
		if err == nil && st.Exists {
			if st.Running {
				return fmt.Errorf("实例容器 %s 已在运行（%s）：请先停止，或用「强制关闭」清理后再启动",
					container.NameOf(i.ID), st.Status)
			}
			// 已退出但没被 --rm 清掉（例如 dockerd 重启过）：先删掉，
			// 否则 docker run --name 会直接撞名失败
			_ = i.Container.Remove(context.Background(), i.ID, true)
		}
	}

	// 确保实例目录存在
	if err := os.MkdirAll(filepath.Join(i.Dir, "logs"), 0o755); err != nil {
		i.status = "error"
		return fmt.Errorf("创建日志目录失败: %w", err)
	}

	// ---- 解析运行身份（早做，失败要早说）----
	//
	// 用"每次启动现解析"而不是构造时定好的静态值：per-instance 用户可能在
	// Daemon 运行期间才被创建（先建实例、后补用户的运维顺序很常见），
	// 现解析能自动跟上，也不会因为一次解析失败就把实例永久卡住。
	var identity *runas.Identity
	if i.RunAsFor != nil {
		id, err := i.RunAsFor(i.ID)
		if err != nil {
			i.status = "error"
			return fmt.Errorf("解析实例运行身份失败：%w", err)
		}
		identity = id
	}
	if identity == nil && runas.IsRoot() {
		// 这是整套修复的最后一道闸：Daemon 以 root 跑却没有解析出降权身份时，
		// 宁可实例起不来，也绝不"照旧跑成 root" —— 那正是本次要修掉的漏洞。
		i.status = "error"
		return fmt.Errorf(
			"Daemon 以 root 运行，但该实例没有可用的降权身份，已拒绝启动：" +
				"以 root 运行实例会让任何能操作这台实例的用户获得节点 root（可读走其它租户的存档、mTLS 私钥）。" +
				"请检查节点配置里的 instance_user（默认 per-instance）与 Daemon 日志")
	}
	if identity != nil {
		if err := os.MkdirAll(i.StateDir, 0o700); err != nil {
			i.status = "error"
			return fmt.Errorf("创建实例状态目录失败: %w", err)
		}
		// 只有在**真的要换 uid** 时才需要这些检查与改属主：
		// 身份与 Daemon 自己相同（非 root 的 current 模式）时，权限天然一致，
		// 而那些检查会把测试用的 0700 临时目录、以及"实例目录 0700"这种
		// 完全正常的布局误判成故障。
		if identity.UID != uint32(os.Geteuid()) || identity.GID != uint32(os.Getegid()) {
			if bad := runas.CheckTraversable(i.Dir); bad != "" {
				i.status = "error"
				return fmt.Errorf(
					"目录 %s 缺少其他用户的执行位（o+x），实例进程（用户 %s）无法进入自己的目录。"+
						"请执行：chmod o+x %s", bad, identity.Username, bad)
			}
		}
	}

	// 让服务端监听的端口与实例的 port 一致（隧道正是按后者下发的）。
	// 失败只记日志、不拦启动 —— 这是修正一致性的动作，不是启动的前置条件。
	i.syncServerPortLogged()

	// 日志轮转：MC 的 stdout 直接写入该文件且进程持有 fd，
	// 运行期间无法安全轮转，因此在每次启动前按大小滚动。
	logPath := filepath.Join(i.Dir, "logs", "console.log")
	rotateIfNeeded(logPath)

	// 打开日志文件（追加）
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		i.status = "error"
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	i.logFile = lf

	// 构建启动命令。优先级（高 → 低）：
	//   1. 实例目录下的 start.sh（可在文件管理中直接编辑，最灵活）
	//   2. 自定义启动命令模板（instance.json 的 start_command，支持占位符）
	//   3. 默认 java -Xms/-Xmx -jar
	// 这一优先级让「启动脚本」成为可见、可改、可版本管理的实体，
	// 同时保留零配置开箱即用的默认行为。
	var cmd *exec.Cmd
	scriptPath := filepath.Join(i.Dir, "start.sh")
	if fi, err := os.Stat(scriptPath); err == nil && !fi.IsDir() {
		// 通过 sh 调用，因此不要求脚本本身带执行位 —— 用户可直接在
		// 文件管理中创建/编辑 start.sh，无需额外 chmod。
		i.startMode = "start.sh"
		cmd = exec.Command("sh", scriptPath)
	} else if i.StartCommand != "" {
		i.startMode = "custom"
		// 自定义命令：用 sh -c 执行，支持占位符替换和任意脚本
		command := i.renderCommand(i.StartCommand)
		cmd = exec.Command("sh", "-c", command)
	} else {
		i.startMode = "default"
		// 默认 java -jar 模板
		if _, err := os.Stat(i.JarPath); err != nil {
			i.status = "error"
			i.logFile.Close()
			i.logFile = nil
			return fmt.Errorf("jar 不存在: %s", i.JarPath)
		}
		args := []string{
			fmt.Sprintf("-Xms%s", normMem(i.MinMem)),
			fmt.Sprintf("-Xmx%s", normMem(i.MaxMem)),
			"-jar", i.JarPath,
			"nogui",
		}
		args = append(args, i.JavaArgs...)
		javaBin := i.resolveJavaBin()
		cmd = exec.Command(javaBin, args...)
	}
	cmd.Dir = i.Dir
	// 独立进程组：便于停止时连同子进程一起终止（sh -c 包裹时尤其重要）
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// ---- 容器模式：把"宿主命令行"换成"docker run"（见 docs/CONTAINERIZATION.md）----
	//
	// 注意这里**不是另起一套启动逻辑**：命令行仍按上面同一套优先级算出来
	//（start.sh → 自定义 → 默认 java），容器模式只是把它搬进容器执行。
	// 这样"面板里选 Java 版本""自定义启动命令""启动脚本"这些既有功能
	// 在两种模式下语义一致，不会出现"只有容器模式才有的启动方式"。
	if i.Container != nil {
		if identity == nil {
			// 没有 uid 就写不出 --user；而容器里的 root 就是宿主 root。
			i.status = "error"
			if i.logFile != nil {
				i.logFile.Close()
				i.logFile = nil
			}
			return fmt.Errorf("该实例启用了容器模式，但没有解析到降权身份：" +
				"容器里的 root 等同于节点 root，因此拒绝以 root 身份启动。" +
				"请检查节点配置里的 instance_user（默认 per-instance）")
		}
		cargv, cerr := i.containerArgv(cmd, identity)
		if cerr != nil {
			i.status = "error"
			if i.logFile != nil {
				i.logFile.Close()
				i.logFile = nil
			}
			return cerr
		}
		// docker CLI 自己必须是 root（要连 /var/run/docker.sock），
		// 降权由 --user 在容器内生效 —— 所以这里**不能**设 Credential。
		cmd = exec.Command(cargv[0], cargv[1:]...)
		cmd.Dir = i.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		i.containerized = true
	}

	// ---- 降权运行（T0 修复的核心）----
	//
	// 这一段决定了"能管一台实例"到底等于多大的权限。以前这里什么都没有，
	// 于是 Daemon（root）exec 出来的 java / sh 也是 root，而面板允许实例所有者
	// 编辑 start.sh、允许协作者往控制台发命令 —— 两者都能拿到 shell，
	// 于是任何能碰一台实例的用户都拿到了节点 root。
	//
	// 现在把实例进程切到专用身份（每实例一个系统用户）。注意这只是修复的一半：
	// 另一半是"root 会去读的文件不能放在租户可写的目录里"
	//（frpc.toml / instance.json，见 config.StateDir 的说明）。
	//
	// 改属主必须在**这里**做，也就是在 Daemon 自己往实例目录写完（server.properties、
	// 日志文件）之后、exec 之前：早一步的话，Daemon 随后写的文件又变回 root 属主，
	// 服务端下次改写它就会 permission denied。
	if identity != nil {
		// 同 uid 时不必改属主（改也是空操作），省掉一次整棵目录的遍历
		if identity.UID != uint32(os.Geteuid()) || identity.GID != uint32(os.Getegid()) {
			if err := runas.ChownTree(i.Dir, identity); err != nil {
				i.status = "error"
				if i.logFile != nil {
					i.logFile.Close()
					i.logFile = nil
				}
				return fmt.Errorf("把实例目录交给运行用户 %s 失败: %w", identity.Username, err)
			}
		}
		// 目录私有化：只改属主是不够的 —— 目录 0755 时，**另一个实例**的进程
		// （同样是普通用户、只是 uid 不同）能穿进来读走 0644 的存档与名单。
		// 每次启动都补一次 chmod，顺带把老装机（先前建的是 0755）纠正过来。
		if err := os.Chmod(i.Dir, 0o700); err != nil {
			i.status = "error"
			if i.logFile != nil {
				i.logFile.Close()
				i.logFile = nil
			}
			return fmt.Errorf("设置实例目录权限失败: %w", err)
		}
		// 注意这两种模式下降权的落点不同：
		//   - native：直接给子进程设 Credential（内核在 exec 时切换 uid）；
		//   - container：**不能**给 docker CLI 设 Credential —— CLI 要以 root 连
		//     /var/run/docker.sock，降权是容器内的 `--user <uid>:<gid>`。
		//     最初这里没区分，结果 CLI 以实例 uid 去连 docker socket，
		//     容器一个都起不来，而报错只在实例日志里（"permission denied ...
		//     docker.sock"），很容易误判成 docker 没装好。
		if !i.containerized {
			cmd.SysProcAttr.Credential = identity.Credential()
			// HOME 显式给实例目录：per-instance 用户是 --system 建的、没有真实家目录，
			// 而 JVM 取不到 passwd 项时会把 user.home 落成 "/"，
			// 插件往 user.home 写文件就会在根目录上碰壁。
			cmd.Env = append(os.Environ(), "HOME="+identity.Home)
		}
	}

	// 关键设计：stdout/stderr 直接重定向到日志文件（不经管道）。
	// 这样 Daemon 崩溃/重启不会中断 MC 进程；Daemon 通过 tail 文件获取输出。
	cmd.Stdout = lf
	cmd.Stderr = lf

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		i.status = "error"
		return err
	}

	i.cmd = cmd
	i.stdin = stdin
	i.status = "running"
	i.started = true
	i.adoptedPID = 0

	// 施加资源限制（CPU 配额）。必须在 Start 之后 —— cgroup 只能对已存在的
	// 进程生效。失败不终止实例，仅记录原因供界面提示。
	if i.containerized {
		// 容器实例的限额由 Docker 实施（--memory / --cpus）。**必须跳过这里**：
		// 同一个进程在一个 cgroup 层级里只能属于一个 cgroup，Daemon 再写一次
		// 会把容器自己的 cgroup 覆盖掉，配额静默失效（见 docs/CONTAINERIZATION.md 3.3）。
		i.limitWarn = ""
	} else {
		i.limitWarn = i.applyResourceLimit(cmd.Process.Pid)
	}

	// 通知外部：本次启动成功（用于累计开机次数与运行时长）
	i.stopRecorded = false
	go fireLifecycle(i.ID, true)

	// 监视进程退出：若进程自行消失（崩溃、被系统终止），
	// 补记一次关机 —— 否则崩溃不计入统计，关机次数会低于实际。
	go i.watchExit(cmd.Process.Pid)

	// 记录 PID，供 Daemon 重启后接管
	_ = os.WriteFile(i.pidFile, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644)

	// 通过 tail 日志文件广播控制台输出
	go i.tailLog(logPath)

	// 等待进程退出
	go func() {
		err := cmd.Wait()
		// 先把"进程已死"通知出去（收 frpc），**再**把状态改成 stopped。
		//
		// 顺序不能反：谁看到 status=stopped，谁就可能立刻重新启动实例并拉起
		// 新的 frpc（Restart 就是靠轮询状态判断的）—— 那时本协程才去收 frpc，
		// 收掉的会是**新**的那一个。
		//
		// 也因此不能放在下面的临界区里：收 frpc 最多要等 3 秒，占着 i.mu
		// 会把 Status() 这类查询全堵住。
		fireExit(i.ID)

		i.mu.Lock()
		defer i.mu.Unlock()
		i.status = "stopped"
		i.started = false
		_ = os.Remove(i.pidFile)
		if i.logFile != nil {
			i.logFile.Close()
			i.logFile = nil
		}
		if err != nil {
			i.broadcast(fmt.Sprintf("[进程退出] %v\n", err))
		} else {
			i.broadcast("[进程正常退出]\n")
		}
	}()

	return nil
}

// containerArgv 把已算好的宿主命令行翻译成"docker run + 容器内命令行"。
//
// 翻译只做两件事：
//  1. **路径**：实例目录挂到 /data，因此命令行里指向实例目录的路径要一并改写
//     （start.sh 的路径、-jar 后面的 jar 路径、自定义命令里的 {dir} 等）。
//     用"前缀替换"而不是解析参数列表，是因为自定义启动命令是一整段 shell 文本，
//     按参数切会把 `java -jar /path/x.jar` 这种整段当成一个字符串。
//  2. **java**：宿主的 java 路径原样可用（JDK 只读挂入同一个路径），
//     但如果 java 落在实例目录里（用户自己传的 JDK），那它已经在 /data 下，
//     要改写成容器内路径，并且不用再额外挂载。
//
// 起脚本统一用 bash 而不是 sh：节点的 /bin/sh 是 bash，用户的 start.sh
// 多半是 bash 写法；而基础镜像里的 /bin/sh 是 dash，直接跑会踩 [[ ]]、数组。
func (i *Instance) containerArgv(native *exec.Cmd, identity *runas.Identity) ([]string, error) {
	if err := i.Container.EnsureNetwork(context.Background(), i.ID); err != nil {
		return nil, fmt.Errorf("准备容器网络失败：%w", err)
	}

	// JDK：容器内要执行的 java、以及要只读挂入的 JDK 根目录
	javaBin, javaHome := i.containerJava()

	var inner []string
	switch i.startMode {
	case "start.sh":
		inner = []string{"bash", container.MountPoint + "/start.sh"}
	case "custom":
		script := ""
		if len(native.Args) >= 3 {
			script = native.Args[2] // ["sh","-c",<脚本文本>]
		}
		inner = []string{"bash", "-c", i.toContainerPaths(script)}
	default:
		inner = make([]string, 0, len(native.Args))
		for n, a := range native.Args {
			if n == 0 && javaBin != "" {
				inner = append(inner, javaBin)
				continue
			}
			inner = append(inner, i.toContainerPaths(a))
		}
	}

	spec := container.Spec{
		InstanceID:   i.ID,
		Dir:          i.Dir,
		UID:          identity.UID,
		GID:          identity.GID,
		MemoryBytes:  i.MemLimitBytes,
		CPUPercent:   i.CPUQuotaPercent,
		Port:         i.Port,
		JavaHome:     javaHome,
		ResourcesDir: i.ContainerResourcesDir,
		Command:      inner,
	}
	return i.Container.Argv(spec)
}

// toContainerPaths 把命令行里指向实例目录的路径改写为容器内路径。
func (i *Instance) toContainerPaths(s string) string {
	if s == "" {
		return s
	}
	if s == i.Dir {
		return container.MountPoint
	}
	return strings.ReplaceAll(s, i.Dir, container.MountPoint)
}

// containerJava 解析容器内要用的 java 可执行文件，以及需要只读挂入的 JDK 根目录。
//
// javaHome 为空表示"不需要额外挂载"：要么 JDK 就在实例目录里（已在 /data 下），
// 要么根本没解析到具体 JDK（那就只能指望镜像 PATH 上的 java —— 基础镜像里
// 没有 java，所以这种情况会在容器里以"找不到 java"失败，属于应当让人看见的错误）。
func (i *Instance) containerJava() (javaBin, javaHome string) {
	bin := i.resolveJavaBin()
	if bin == "" {
		return "", ""
	}
	abs := bin
	if !filepath.IsAbs(abs) {
		p, err := exec.LookPath(abs)
		if err != nil {
			return bin, "" // 找不到实体，交给容器里的 PATH 去试
		}
		abs = p
	}
	// /usr/bin/java 往往是 alternatives 的软链，而容器里没有这套 alternatives，
	// 必须解析到 JDK 内那个真实文件
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if filepath.Base(abs) != "java" {
		return bin, ""
	}
	home := filepath.Dir(filepath.Dir(abs)) // <home>/bin/java

	// JDK 在实例目录里：整个实例目录已经挂成 /data，不必重复挂载
	if home == i.Dir || strings.HasPrefix(home, i.Dir+string(os.PathSeparator)) {
		return i.toContainerPaths(abs), ""
	}
	return abs, home
}

// SetContainer 启用/停用容器模式（Daemon 在创建、加载、切换时调用）。
//
// rt 为 nil 表示容器运行时不可用（节点没装 docker）；
// required 表示**元数据要求**该实例必须容器化 —— 这时 rt 为 nil 会在启动时
// 被明确拒绝，而不是悄悄退回 native。这个区分很重要：面板上写着"容器化"、
// 实际却跑在宿主上，比直接起不来更危险（隔离看起来在，其实不在）。
func (i *Instance) SetContainer(rt *container.Runtime, resourcesDir string, required bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.Container = rt
	i.ContainerResourcesDir = resourcesDir
	i.wantContainer = required
	// containerized 的含义是"该实例按容器模式运行"，而不是"本次是容器起的"：
	// Daemon 重启后接管一个正在跑的容器时，Stop/Kill 也必须走容器那条路，
	// 否则会去对容器 init 进程发信号 —— 那是 docker 的内部进程，语义完全不对。
	i.containerized = rt != nil && required
}

// Containerized 该实例是否按容器模式运行。
func (i *Instance) Containerized() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.containerized
}

// tail 相关的节奏参数。
const (
	// tailPollInterval 读到文件末尾后的等待间隔。
	tailPollInterval = 200 * time.Millisecond
	// tailFlushIdleTicks 连续多少次"没有任何新数据"之后，把没带换行的残行也推出去。
	//
	// 为什么需要它：服务端偶尔会写下**不带换行**的内容（进度条 `\r` 刷新、
	// 交互式提示符）。攒着不发的代价是这类内容在控制台上永远看不见。
	// 3 × 200ms = 600ms：正常整行输出不受影响（拿到换行就立刻发），
	// 只有"半行"要多等这 600ms，换来确定性。
	tailFlushIdleTicks = 3
	// tailMaxPendingBytes 残行的长度上限，超过就无条件推出，防止
	// "服务端写了一个几十 MB 不带换行的东西"把内存吃光。
	tailMaxPendingBytes = 64 * 1024
)

// tailLog 持续读取日志文件新增内容并广播（类似 tail -f）。
// 用于把 MC 输出推送给控制台订阅者，且不依赖进程管道。
//
// 为什么要在这里按行攒：bufio.Reader.ReadString('\n') 在**追尾一个正在被写入的
// 文件**时，读到文件末尾就会把"还没写完的半行"当作一次成功读取返回（err=EOF）。
// 原样广播的话，一行会被拆成两条消息发给控制台，后果不只是看着断成两截：
//
//   - 行级别高亮（console_format.go）靠"行首的 [WARN]/[ERROR]"判断，
//     拆开后前半截有标记、后半截没有，同一行会被涂成两种样子；
//   - 前半截没有换行，ESC[K 会在行中间执行，底色只涂半行。
//
// 所以这里只广播**以换行结尾**的完整行，半行留在缓冲里等下一轮补齐；
// 半行若长时间补不齐（服务端本来就不发换行），按上面的 idle 规则兜底推出。
func (i *Instance) tailLog(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	// 从文件末尾开始，只推送新产生的输出
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return
	}
	reader := bufio.NewReader(f)

	var pending strings.Builder // 尚未收到换行的残行
	idle := 0
	finalizing := false // 已进入"停机前最后一次确认"阶段

	// flushPending 把残行推出去（没有换行就原样推，不自己补 —— 补出来的行
	// 会让控制台显示服务端并没有输出过的内容）。
	flushPending := func() {
		if pending.Len() == 0 {
			return
		}
		i.broadcast(pending.String())
		pending.Reset()
		idle = 0
	}

	for {
		chunk, err := reader.ReadString('\n')
		if chunk != "" {
			pending.WriteString(chunk)
			switch {
			case strings.HasSuffix(chunk, "\n"):
				// 完整行：立刻推送，不加任何延迟
				flushPending()
			case pending.Len() >= tailMaxPendingBytes:
				// 半行已经太长：不能再攒了
				flushPending()
			default:
				idle = 0 // 有新数据，重新计时
			}
		}
		if err != nil {
			if !i.isActive() {
				// 实例已经停了。**不能立刻返回**：进程退出与"文件里最后几行
				// 变得可见"之间有个很短的窗口（cmd.Wait 返回后收尾协程才把
				// status 改成 stopped），此时直接退出会把关服日志
				//（Stopping server / Saving worlds）整段丢掉 —— 恰恰是停机时
				// 最想看的那几行。多等一拍再确认一次，然后才收尾。
				if !finalizing {
					finalizing = true
					time.Sleep(tailPollInterval)
					continue
				}
				flushPending()
				return
			}
			idle++
			if idle >= tailFlushIdleTicks {
				flushPending()
			}
			time.Sleep(tailPollInterval)
		}
	}
}

// isActive 实例是否处于运行/接管状态（tail 循环据此决定是否继续等待）。
func (i *Instance) isActive() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status == "running"
}

// watchAdopted 监视接管进程，退出时更新状态。
func (i *Instance) watchAdopted(pid int) {
	for {
		time.Sleep(2 * time.Second)
		if !IsAlive(pid) {
			// 同 cmd.Wait 那条路径：先通知"进程已死"，再改状态（顺序理由见彼处注释）。
			// 这里不判断 exited 也可以 —— 无论这次退出是面板要求的还是进程自己崩的，
			// 进程确实没了，隧道就该跟着下。
			fireExit(i.ID)

			i.mu.Lock()
			// exited 表示进程是「自行退出」而非被面板停止：
			// 主动停止会先把 adoptedPID 清零，此处就不会命中，
			// 从而避免与 Stop() 里的回调重复计数。
			exited := i.adoptedPID == pid
			if exited {
				i.adoptedPID = 0
				i.status = "stopped"
			}
			recorded := i.stopRecorded
			i.mu.Unlock()
			_ = os.Remove(i.pidFile)
			i.broadcast("\n[进程已退出]\n")
			// 接管进程崩溃时无人调用 Stop()，需在此补记停机 ——
			// 否则关机次数偏低，也看不出实例是否频繁崩溃。
			if exited && !recorded {
				go fireLifecycle(i.ID, false)
			}
			return
		}
	}
}

// Stop 停止实例进程。托管实例优先优雅 stop；接管实例发送 SIGTERM。
func (i *Instance) Stop() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.status != "running" {
		return fmt.Errorf("实例未运行")
	}
	// 接管状态：无 stdin 管道，用 SIGTERM（MC 有 shutdown hook，可优雅退出）
	if i.adoptedPID != 0 {
		// 容器模式下"接管的进程"其实是接管的**容器**：容器没有 stdin 可写，
		// 但可以让 docker 去送 SIGTERM（同样是优雅退出）。
		if i.containerized && i.Container != nil {
			if err := i.Container.GracefulStop(context.Background(), i.ID, 30*time.Second); err != nil {
				return fmt.Errorf("停止容器失败: %w", err)
			}
			i.stopRecorded = true
			go fireLifecycle(i.ID, false)
			return nil
		}
		if err := syscall.Kill(-i.adoptedPID, syscall.SIGTERM); err != nil {
			// 进程组不存在时退回单进程信号
			if err2 := syscall.Kill(i.adoptedPID, syscall.SIGTERM); err2 != nil {
				return fmt.Errorf("发送停止信号失败: %v", err2)
			}
		}
		go func(pid int) {
			for n := 0; n < 30; n++ {
				time.Sleep(time.Second)
				if !IsAlive(pid) {
					i.mu.Lock()
					i.status = "stopped"
					i.adoptedPID = 0
					_ = os.Remove(i.pidFile)
					i.mu.Unlock()
					return
				}
			}
		}(i.adoptedPID)
		return nil
	}
	if i.stdin != nil {
		i.stdin.Write([]byte("stop\n"))
	}
	// 记为一次关机（以用户发出的停止请求为准）
	i.stopRecorded = true
	go fireLifecycle(i.ID, false)
	return nil
}

// Restart 重启实例：先优雅停止并等待进程退出，超时则强制杀死，最后重新启动。
// 注意：Stop() 只发送停止指令，进程退出需要时间，必须等待而不能立刻 Start（否则会报“实例已在运行”）。
func (i *Instance) Restart(timeout time.Duration) error {
	if i.Status() != "running" {
		return i.Start()
	}
	if err := i.Stop(); err != nil {
		return err
	}

	// 等待优雅退出
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && i.Status() == "running" {
		time.Sleep(300 * time.Millisecond)
	}

	// 仍未退出则强制杀死（并再等一会儿让状态归位）
	if i.Status() == "running" {
		_ = i.Kill()
		for n := 0; n < 25 && i.Status() == "running"; n++ {
			time.Sleep(200 * time.Millisecond)
		}
	}

	if i.Status() == "running" {
		return fmt.Errorf("无法停止实例（进程未退出），已取消重启")
	}
	return i.Start()
}

// Kill 强制杀死进程（含接管的遗留进程）。
func (i *Instance) Kill() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adoptedPID != 0 {
		if err := syscall.Kill(-i.adoptedPID, syscall.SIGKILL); err != nil {
			_ = syscall.Kill(i.adoptedPID, syscall.SIGKILL)
		}
		i.stopRecorded = true
		_ = os.Remove(i.pidFile)
		i.status = "stopped"
		i.adoptedPID = 0
		go fireLifecycle(i.ID, false)
		return nil
	}
	if i.cmd != nil && i.cmd.Process != nil {
		// 容器模式：**必须先删容器**。
		//
		// 只杀 docker CLI 的进程组是不够的 —— 容器是 dockerd 里的独立进程，
		// CLI 死了它照样跑（还把端口占着、把 session.lock 占着），
		// 而面板会显示"已停止"。PoC A11 专门验的就是这条。
		if i.containerized && i.Container != nil {
			if err := i.Container.Remove(context.Background(), i.ID, true); err != nil {
				slog.Warn("删除实例容器失败，实例可能仍在运行", "instance", i.ID, "error", err)
				i.limitWarn = "删除容器失败：" + err.Error()
			}
		}
		// 杀整个进程组，避免 sh -c 包裹时残留子进程
		_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
		err := i.cmd.Process.Kill()
		i.stopRecorded = true
		go fireLifecycle(i.ID, false)
		return err
	}
	return fmt.Errorf("无进程")
}

// ReadPIDFile 读取实例的 PID 记录（无则为 0）。
//
// stateDir 是**平台状态目录**（<state_dir>/<实例ID>），不是实例目录 ——
// 见下方 pidFile 字段的说明。
func ReadPIDFile(stateDir string) int {
	b, err := os.ReadFile(filepath.Join(stateDir, "daemon.pid"))
	if err != nil {
		return 0
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil {
		return 0
	}
	return pid
}

// SendCommand 发送控制台命令。
func (i *Instance) SendCommand(cmd string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.adoptedPID != 0 {
		return fmt.Errorf("实例为接管状态（Daemon 曾重启），无法发送命令；请重启实例以恢复完整控制")
	}
	if i.status != "running" || i.stdin == nil {
		return fmt.Errorf("实例未运行")
	}
	_, err := i.stdin.Write([]byte(cmd + "\n"))
	return err
}

// consoleSubBuf 单个控制台订阅者的输出缓冲行数。
//
// 256 行在"服务端刷屏"时不够用：一次世界保存 + 插件批量日志很容易在
// 控制台把这几百行排完之前就超过它（gRPC + WS + 浏览器渲染这条链路的
// 消费速度远低于服务端往文件里写的速度）。缓冲不是根因，只是减小触发概率；
// 真正保证"不静默丢"的是 broadcast 里的丢帧通知。
const consoleSubBuf = 1024

// consoleSub 一个控制台订阅者。
//
// 为什么是结构体而不是裸 channel：需要**按订阅者**记"丢了多少行" ——
// 一个慢订阅者不该拖累别人，丢帧提示也只需要发给落后的那一个。
type consoleSub struct {
	ch      chan string
	dropped int // 上次提示之后又丢了多少行（未提示的积压）
}

// Subscribe 订阅控制台输出，返回一个 channel 和取消函数。
func (i *Instance) Subscribe() (<-chan string, func()) {
	sub := &consoleSub{ch: make(chan string, consoleSubBuf)}
	i.subMu.Lock()
	i.subs = append(i.subs, sub)
	i.subMu.Unlock()
	return sub.ch, func() {
		i.subMu.Lock()
		defer i.subMu.Unlock()
		for idx, s := range i.subs {
			if s == sub {
				i.subs = append(i.subs[:idx], i.subs[idx+1:]...)
				close(s.ch)
				break
			}
		}
	}
}

// broadcast 向所有订阅者广播一行。
//
// 注意：使用独立的 subMu，绝不使用 mu —— 因为调用方可能正持有 mu
// （Go 的 sync.Mutex 不可重入，嵌套加锁会永久死锁）。
//
// 全程持锁，不再"拷贝一份订阅者列表后解锁再发"：
// 那样写有个会**直接 panic** 的竞态 —— 拷贝发生在锁内、发送发生在锁外，
// 而取消订阅的收尾会 close(channel)。两者交错时就是 send on closed channel，
// 表现是"用户关掉控制台页签，Daemon 崩了"。
// 这里可以安心持锁，因为发送全都是非阻塞的（select + default）：
// 没有任何一条路径会在持锁期间等待。
func (i *Instance) broadcast(line string) {
	i.subMu.Lock()
	defer i.subMu.Unlock()
	for _, s := range i.subs {
		// 落后的订阅者：先把"丢了多少行"补一条可见的提示，再发当前行。
		// 宁可让用户看到"这里丢了 N 行"，也不要让他以为日志就长这样 ——
		// 静默丢帧会让人以为服务端没输出，从而去查一个不存在的问题。
		// 提示本身也可能发不进去（缓冲还满着），那就继续累计，下一条行再试。
		if s.dropped > 0 {
			notice := fmt.Sprintf(
				"\x1b[93m[控制台丢帧] 上面有 %d 行因推送不及时被跳过（实例日志文件里仍然完整，可在文件管理里查看）\x1b[0m\n",
				s.dropped)
			select {
			case s.ch <- notice:
				s.dropped = 0
			default:
			}
		}
		select {
		case s.ch <- line:
		default:
			s.dropped++
		}
	}
}

// RecentOutput 返回最近 maxLines 行控制台输出，用于控制台重连时回放历史。
// 只读取日志文件尾部（最多 consoleTailMaxBytes），避免大日志拖慢响应。
func (i *Instance) RecentOutput(maxLines int) []string {
	if maxLines <= 0 {
		return nil
	}
	path := filepath.Join(i.Dir, "logs", "console.log")

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	size := fi.Size()
	truncated := false
	if size > consoleTailMaxBytes {
		if _, err := f.Seek(size-consoleTailMaxBytes, io.SeekStart); err != nil {
			return nil
		}
		truncated = true
	}

	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return nil
	}

	text := string(b)
	if truncated {
		// 丢弃可能被截断的首行
		if idx := strings.IndexByte(text, '\n'); idx >= 0 {
			text = text[idx+1:]
		}
	}

	lines := strings.Split(text, "\n")
	// 末尾通常是空行，去掉
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l+"\n")
	}
	return out
}

// consoleTailMaxBytes 控制台历史回放时单次读取的日志上限。
const consoleTailMaxBytes = 512 * 1024

// 控制台日志轮转参数
const (
	consoleLogMaxBytes = 64 * 1024 * 1024 // 单文件上限 64MB
	consoleLogKeep     = 3                // 保留的历史文件数量（console.log.1..N）
)

// rotateIfNeeded 若日志文件超过上限则滚动（使用默认上限）。
func rotateIfNeeded(path string) {
	rotateBySize(path, consoleLogMaxBytes, consoleLogKeep)
}

// rotateBySize 按指定上限滚动日志：console.log → console.log.1 → ... → 删除最旧。
//
// 只能在进程启动前调用：运行中的 Minecraft 持有 stdout 的文件描述符，
// 此时重命名会导致其继续写入旧文件，控制台流将读到错误内容。
func rotateBySize(path string, maxBytes int64, keep int) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < maxBytes {
		return
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for n := keep - 1; n >= 1; n-- {
		old := fmt.Sprintf("%s.%d", path, n)
		next := fmt.Sprintf("%s.%d", path, n+1)
		if _, err := os.Stat(old); err == nil {
			_ = os.Rename(old, next)
		}
	}
	_ = os.Rename(path, path+".1")
}

// normMem 确保内存参数带单位（默认 G）。
func normMem(s string) string {
	if s == "" {
		return "1G"
	}
	return s
}

// resolveJavaBin 把 java_version 解析成实际要执行的 java 可执行文件。
//
// 解析不到时回退到 PATH 上的 java，而不是启动失败 —— 节点上的 JAVA 环境
// 可能由 alternatives 管理、未必出现在常见 JDK 目录下，直接失败会让
// "选错版本"变成"实例起不来"。但回退必须**可见**：日志里记一条，
// 控制台也会看到一条提示，否则用户会以为自己真的切到了 17。
func (i *Instance) resolveJavaBin() string {
	want := strings.TrimSpace(i.JavaVersion)
	if want == "" {
		return "java"
	}

	// 相对路径按**实例目录**解析。
	//
	// 用户填 "jdk/bin/java" 时他指的是实例目录里的那个（文件管理器里看到的位置），
	// 而 javaruntime.Resolve 会拿 Daemon 自己的工作目录去 stat —— Daemon 的工作目录
	// 是 /opt/mcpanel，于是永远找不到，还会静默回退到 PATH 上的 java，
	// 表现就是"我明明填了路径却没用"。这类相对路径只在实例内才有意义，
	// 所以这里补上实例目录前缀，再交给 Resolve 统一处理（它同时认可执行文件与 JDK 目录）。
	rel := strings.ContainsAny(want, "/\\") && !filepath.IsAbs(want)
	if rel {
		want = filepath.Join(i.Dir, want)
	}

	if p := javaruntime.Resolve(want); p != "" {
		switch {
		case rel:
			// 相对路径要明确说清它落到了实例目录下的哪个文件：
			// 用户填 "jdk/bin/java" 时，最想知道的就是"到底用了哪个 java"。
			i.javaNote = "使用实例目录内的 Java：" + p
		case p != want:
			i.javaNote = "使用 JDK " + strings.TrimSpace(i.JavaVersion) + "：" + p
		}
		return p
	}

	if rel {
		i.javaNote = fmt.Sprintf("实例目录下找不到 %s，已回退到 PATH 中的 java",
			strings.TrimSpace(i.JavaVersion))
	} else {
		i.javaNote = fmt.Sprintf("未在节点上找到 Java %s，已回退到 PATH 中的 java", want)
	}
	slog.Warn("Java 版本解析失败，回退到 PATH",
		"instance", i.ID, "want", strings.TrimSpace(i.JavaVersion), "resolved", want)
	return "java"
}

// JavaNote 返回 Java 版本解析的说明（供控制台提示）。
func (i *Instance) JavaNote() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.javaNote
}

// SetJavaVersion 更新期望的 JDK 版本（下次启动生效）。
func (i *Instance) SetJavaVersion(v string) {
	i.mu.Lock()
	i.JavaVersion = v
	i.mu.Unlock()
}

// renderCommand 替换启动命令模板中的占位符。
// 支持：{jar} {max_mem} {min_mem} {java} {dir}
func (i *Instance) renderCommand(tpl string) string {
	// {java} 解析成具体的 JDK 路径：自定义启动命令里写 java 的话，
	// "Java 版本"这个选择同样应当生效，否则它只在默认模板下有效，
	// 而用自定义命令的实例往往才是更需要指定版本的（比如老版本核心）。
	javaBin := i.resolveJavaBin()
	replacer := map[string]string{
		"{jar}":     i.JarPath,
		"{max_mem}": normMem(i.MaxMem),
		"{min_mem}": normMem(i.MinMem),
		"{java}":    javaBin,
		"{dir}":     i.Dir,
	}
	s := tpl
	for k, v := range replacer {
		s = strings.ReplaceAll(s, k, v)
	}
	return s
}

// ParseMemBytes 把 "3G" / "2048M" / "1.5G" 这类写法换算为字节数。
// 解析失败返回 0（视为不限制），而不是报错 —— 配额解析失败不应阻止实例启动。
func ParseMemBytes(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	mult := int64(1024 * 1024) // 默认按 M 处理
	switch {
	case strings.HasSuffix(s, "G"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "T"):
		mult = 1024 * 1024 * 1024 * 1024
		s = strings.TrimSuffix(s, "T")
	case strings.HasSuffix(s, "K"):
		mult = 1024
		s = strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		s = strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	s = strings.TrimSpace(s)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * float64(mult))
}

// watchExit 监视进程退出，用于补记「非面板操作」导致的停机。
//
// 背景：生命周期的停止回调只在 Stop()/Kill()/接管停止时触发，
// 进程自己崩溃退出不走这些路径。若不在退出时补记，关机次数会偏低，
// 且无法从统计上看出实例是否频繁崩溃。
//
// 判定方式：每秒轮询一次进程是否存活，最长观察 24 小时
// （超时后退出，避免为长期运行的实例白占一个 goroutine）。
func (i *Instance) watchExit(pid int) {
	const maxSeconds = 24 * 3600
	for n := 0; n < maxSeconds; n++ {
		time.Sleep(time.Second)

		// 实例被替换（重启后换成了新进程）时本监视器失效
		i.mu.Lock()
		cur := i.cmd
		i.mu.Unlock()
		if cur == nil || cur.Process == nil || cur.Process.Pid != pid {
			return
		}

		if IsAlive(pid) {
			continue
		}

		i.mu.Lock()
		recorded := i.stopRecorded
		i.mu.Unlock()
		if !recorded {
			// 进程消失且面板未发起停止 → 崩溃或被系统终止
			go fireLifecycle(i.ID, false)
		}
		return
	}
}
