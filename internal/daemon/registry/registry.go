// Package registry 管理 Daemon 上的所有 MC 实例，并把元数据持久化到磁盘，
// 使 Daemon 重启后仍能恢复实例列表（并对遗留进程做接管）。
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/safepath"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
)

// Meta 实例元数据（持久化到 <实例目录>/instance.json）。
type Meta struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	CoreType    string `json:"core_type"`
	JavaVersion string `json:"java_version"`
	JarPath     string `json:"jar_path"`
	CPUQuota    int    `json:"cpu_quota"` // CPU 配额百分比（100 = 1 核；0 = 不限制）
	// BackupDir 该实例备份的存放目录（留空则用节点配置的 backup_root，
	// 再退回到实例目录下的 backups/）
	BackupDir string `json:"backup_dir,omitempty"`

	// MemLimit cgroup 内存上限（如 "4G"；留空 = 不限制）。
	// 需为 JVM 堆外开销留余量，建议 -Xmx × 1.3 + 512MB。
	MemLimit string `json:"mem_limit,omitempty"`

	// ---- 运行统计（供详情页展示运行时长与启停次数）----
	StartCount   int    `json:"start_count"`   // 累计开机次数
	StopCount    int    `json:"stop_count"`    // 累计关机次数（含强制关闭）
	LastStartAt  int64  `json:"last_start_at"` // 最近一次启动的 Unix 时间戳
	MaxMem       string `json:"max_mem"`
	MinMem       string `json:"min_mem"`
	Port         int32  `json:"port"`
	StartCommand string `json:"start_command"`

	// Container 该实例是否跑在 docker 容器里（容器化隔离）。
	//
	// 存在**平台状态目录**下（见 stateDir 的说明）这点很关键：
	// 它决定实例进程能不能看见宿主，若能由租户改写，就等于给了
	// "关掉自己的隔离"这个开关 —— 那正是 T0 修复要堵的口子。
	Container bool `json:"container,omitempty"`
}

// Registry 实例注册表。
type Registry struct {
	mu        sync.RWMutex
	instances map[string]*mcprocess.Instance
	baseDir   string // 实例根目录，如 /opt/mcpanel/instances
	stateDir  string // 平台状态根目录，如 /opt/mcpanel/state（见 metaFile 说明）

	// runner 实例运行身份（可为 nil：单元测试里不涉及降权）。
	runner *runas.Manager

	// container 容器运行时；nil 表示这台节点不能用容器模式（没装 docker）。
	container    *container.Runtime
	resourcesDir string // 只读挂入容器的共享资源目录
}

// New 创建注册表。
// baseDir 会被规范化为绝对路径：实例目录会作为子进程工作目录与配置路径使用，
// 相对路径在 cmd.Dir 变更后会解析错误（曾导致 frpc 找不到配置文件）。
//
// stateDir 存放**被 root 信任**的实例元数据（instance.json、daemon.pid），
// 必须在实例目录之外：实例目录归实例的运行用户所有，而这两份文件是 root
// 读来当作依据的（元数据里的 cpu_quota / mem_limit 决定资源限制，
// pid 决定停止时向谁发信号）——放在租户可写的地方等于把这两把钥匙交出去。
func New(baseDir, stateDir string) *Registry {
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}
	if stateDir == "" {
		stateDir = filepath.Join(filepath.Dir(baseDir), "state")
	}
	if abs, err := filepath.Abs(stateDir); err == nil {
		stateDir = abs
	}
	return &Registry{
		instances: make(map[string]*mcprocess.Instance),
		baseDir:   baseDir,
		stateDir:  stateDir,
	}
}

// SetRunner 注入实例运行身份管理器。
func (r *Registry) SetRunner(m *runas.Manager) { r.runner = m }

// SetContainerRuntime 注入容器运行时（Daemon 启动时调用一次）。
// rt 为 nil 表示节点上不可用容器模式。
func (r *Registry) SetContainerRuntime(rt *container.Runtime, resourcesDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.container = rt
	r.resourcesDir = resourcesDir
}

// ContainerRuntime 返回容器运行时（可能为 nil）。
func (r *Registry) ContainerRuntime() *container.Runtime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.container
}

// bindContainer 按元数据把容器模式接到实例上。
//
// meta 说要容器化但节点上没有 docker 时**不静默降级**：实例会以 native 方式
// 跑起来，而面板上明明写着"容器化"——这种"隔离看起来在、实际不在"的错配
// 比直接起不来更危险。所以这里只记录问题、由启动路径拒绝启动（见 Instance.Start
// 的容器可用性检查）。
func (r *Registry) bindContainer(inst *mcprocess.Instance, m Meta) {
	inst.SetContainer(r.container, r.resourcesDir, m.Container)
}

// Identity 返回实例的运行身份（必要时创建专用系统用户）。
//
// 已注册的实例走 Ensure（缺用户就建）；未注册的目录只做解析、不建用户 ——
// 文件管理偶尔会在"目录还在、注册记录已没了"的残留目录上操作，
// 为它专门建一个系统用户没有意义，而 Resolve 失败时返回 nil，
// 调用方（handOver）就什么都不做。
func (r *Registry) Identity(instanceID string) (*runas.Identity, error) {
	if r.runner == nil {
		return nil, nil
	}
	if _, ok := r.Get(instanceID); ok {
		return r.runner.Ensure(instanceID, r.Dir(instanceID))
	}
	return r.runner.Resolve(instanceID)
}

// registerContainerInstance 注册/接管一个容器化实例。
//
// 三条路：① 容器在跑 → 按接管注册（只读控制台 + 可停止）；
// ② 容器不在跑但实例目录里还有 native 留下的 java → 按 native 孤儿接管
// （从 native 切到容器，或上一次还是 native 时留下的）；
// ③ 都没在跑 → 按容器模式注册，等用户启动。
func (r *Registry) registerContainerInstance(m Meta, dir, sdir string) *mcprocess.Instance {
	apply := func(inst *mcprocess.Instance) {
		inst.StartCommand = m.StartCommand
		inst.JavaVersion = m.JavaVersion
		inst.CPUQuotaPercent = m.CPUQuota
		inst.BackupDir = m.BackupDir
		inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
		inst.Port = int(m.Port)
		r.bindContainer(inst, m)
		r.bindRunAs(inst)
	}

	if r.container != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		st, err := r.container.Inspect(ctx, m.ID)
		cancel()
		if err == nil && st.Running && st.Pid > 0 {
			inst := mcprocess.NewAdopted(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, st.Pid)
			apply(inst)
			slog.Info("已接管运行中的实例容器", "instance", m.ID,
				"container", container.NameOf(m.ID), "pid", st.Pid)
			// 接管来的容器**未必还有人采集输出**：实例的 stdout 原本是靠
			// 启动它的那个 `docker run` CLI 重定向进 logs/console.log 的。
			// unit 用 KillMode=control-group（默认）时，重启会把 CLI 一起收掉，
			// 而**容器本身在 dockerd 的 cgroup 里照常运行** —— 于是日志文件停在
			// 被接管的那一刻，用户对着一个"没有新内容"的控制台，很容易以为实例卡死。
			// 用 KillMode=process 时 CLI 会活下来继续写，那就不能再补一个跟随进程
			//（否则同一行会被写两遍）—— 两种情况都由 EnsureContainerLogFollow 判断。
			// （2026-09-30 起，见 mcprocess.Instance.EnsureContainerLogFollow）
			following, err := inst.EnsureContainerLogFollow()
			if err != nil {
				slog.Warn("重新跟随容器输出失败，控制台可能停在接管时刻",
					"instance", m.ID, "error", err)
				noteConsoleInterrupted(dir, m.ID)
			} else if following {
				slog.Info("容器输出已有人采集", "instance", m.ID, "container", container.NameOf(m.ID))
			}
			return inst
		}
		if err != nil {
			slog.Warn("查询实例容器状态失败，按未运行处理", "instance", m.ID, "error", err)
		}
	}

	if opid, found := mcprocess.FindOrphan(dir); found {
		inst := mcprocess.NewAdopted(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, opid)
		apply(inst)
		slog.Warn("实例标记为容器化，但检测到 native 模式的遗留进程，已按孤儿进程接管",
			"instance", m.ID, "pid", opid)
		return inst
	}

	inst := mcprocess.NewInstance(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem)
	apply(inst)
	return inst
}

// SetContainerMode 切换某实例的容器化开关（面板上的开关走这里）。
//
// 两条硬约束：
//   - **实例必须在停止状态**：运行中的实例换启动方式没有意义（新参数下次
//     启动才生效），而一旦"面板显示已改、实际还在老模式跑"，排查会很难。
//     所以这里直接拒绝，让用户先停。
//   - **开启时节点必须有可用的容器运行时**：没有就报错，而不是记一个
//     "以后会容器化"的标记 —— 那个标记会让人以为隔离已经生效。
func (r *Registry) SetContainerMode(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	if !ok {
		return fmt.Errorf("实例 %s 不存在", id)
	}
	if inst.Status() == "running" || inst.Status() == "starting" {
		return fmt.Errorf("实例正在运行，请先停止再修改容器化设置")
	}
	if enabled && r.container == nil {
		return fmt.Errorf("该节点没有可用的容器运行时（docker 未安装或基础镜像缺失），无法开启容器化")
	}

	sdir := r.StateDir(id)
	m, err := readMeta(sdir)
	if err != nil {
		return fmt.Errorf("读取实例元数据失败: %w", err)
	}
	m.Container = enabled
	if err := writeMeta(sdir, m); err != nil {
		return fmt.Errorf("写入实例元数据失败: %w", err)
	}
	inst.SetContainer(r.container, r.resourcesDir, enabled)
	slog.Info("实例容器化设置已更新", "instance", id, "container", enabled)
	return nil
}

// ContainerizedOf 该实例（元数据）是否启用了容器化。
func (r *Registry) ContainerizedOf(id string) bool {
	r.mu.RLock()
	inst, ok := r.instances[id]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return inst.Containerized()
}

// StateDir 返回某实例的平台状态目录。
func (r *Registry) StateDir(id string) string { return filepath.Join(r.stateDir, id) }

// StateRoot 返回平台状态根目录。
func (r *Registry) StateRoot() string { return r.stateDir }

// bindRunAs 给实例挂上"启动时现解析运行身份"的钩子。
func (r *Registry) bindRunAs(inst *mcprocess.Instance) {
	if r.runner == nil {
		return
	}
	inst.RunAsFor = func(instanceID string) (*runas.Identity, error) {
		return r.runner.Ensure(instanceID, filepath.Join(r.baseDir, instanceID))
	}
}

const metaFile = "instance.json"

// validInstanceID 实例 ID 的合法形态。
//
// 这里再校验一次（面板侧已经挡过一道）是因为 **ID 会直接成为目录名**：
// `filepath.Join(baseDir, m.ID)` 遇到 `../x` 会跳到实例根目录之外建目录，
// 而 Load() 只扫描 baseDir 的直接子目录 —— 那个实例之后会"消失"，
// 但磁盘上的文件还在外面。这类越界必须在这一层也堵住，
// 不能假设调用方一定是自家面板。
var validInstanceID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Create 创建实例：建立目录、写入元数据、注册到内存。
func (r *Registry) Create(m Meta) (*mcprocess.Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !validInstanceID.MatchString(m.ID) {
		return nil, fmt.Errorf("实例 ID %q 不合法：只能用小写字母、数字、连字符与下划线，且必须以字母或数字开头", m.ID)
	}
	if _, ok := r.instances[m.ID]; ok {
		return nil, fmt.Errorf("实例 %s 已存在", m.ID)
	}
	dir := filepath.Join(r.baseDir, m.ID)
	// 0700：实例目录**必须**只有它自己的运行用户（和 root）能进。
	//
	// 只把属主改成实例用户是不够的 —— 目录 0755 时，另一个实例的进程
	//（同样是普通用户，只是 uid 不同）可以穿进来读走 0644 的存档、名单、配置。
	// 这正是"每实例一个用户"的价值所在：uid 不同 + 目录 0700 = 真的隔开。
	// （上一层实例根目录则是 0711：能穿过、不能列目录。）
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建实例目录失败: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	// 平台状态目录（root 0700）：元数据与 pid 记录都放这里，见 New 的说明
	sdir := filepath.Join(r.stateDir, m.ID)
	if err := os.MkdirAll(sdir, 0o700); err != nil {
		return nil, fmt.Errorf("创建实例状态目录失败: %w", err)
	}
	if err := writeMeta(sdir, m); err != nil {
		return nil, fmt.Errorf("写入实例元数据失败: %w", err)
	}

	// 运行身份：建实例时就把专用用户建出来，让"用户不存在"这类问题
	// 在创建阶段就暴露，而不是等到用户点启动才发现。
	//
	// 失败**不**中断创建 —— 实例目录与元数据已经写好、面板那边也在等回执；
	// 真正的把关在 Start（解析不到身份就拒绝启动）。这里只记日志。
	if r.runner != nil {
		if id, err := r.runner.Ensure(m.ID, dir); err != nil {
			slog.Warn("创建实例专用运行用户失败，启动该实例前必须修好", "instance", m.ID, "error", err)
		} else {
			// 目录交给运行用户：之后服务端要在这里建 world/、写日志
			if err := runas.ChownTree(dir, id); err != nil {
				slog.Warn("设置实例目录属主失败", "instance", m.ID, "error", err)
			}
		}
	}

	inst := mcprocess.NewInstance(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem)
	inst.StartCommand = m.StartCommand
	inst.JavaVersion = m.JavaVersion
	inst.CPUQuotaPercent = m.CPUQuota
	inst.BackupDir = m.BackupDir
	inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
	inst.Port = int(m.Port) // 启动前要按它校准 server.properties
	r.bindContainer(inst, m)
	r.bindRunAs(inst)
	r.instances[m.ID] = inst
	return inst, nil
}

// noteConsoleInterrupted 在**没能重新跟随容器输出**时，往 console.log 末尾追加一句说明。
//
// 这是兜底文案，正常路径不走它：接管运行中的容器时 registry 会补一条
// `docker logs -f`（见 mcprocess.FollowContainerLogs），输出会照常接上。
// 只有当那条命令起不来（docker 不可用、容器名对不上等）时才落到这里 ——
// 那时用户看到的是一个"停在某个时间点、再也不更新"的控制台，
// 最自然的猜测是"服务端卡死了"，所以必须把原因与恢复办法写进控制台本身。
//
// 失败只记日志：写不进去（目录只读、磁盘满）不该让实例注册不上。
//
// 这里同样是"以 root 往租户可控的路径追加写"，所以与其他控制台日志出入口一样
// 要拒绝软链接：`logs -> /etc` 或 `logs/console.log -> /etc/cron.d/atl-x`
// 都会让这句平台说明落进节点上的任意文件（详细理由见 mcprocess.consoleLogPath）。
func noteConsoleInterrupted(dir, instanceID string) {
	logPath := filepath.Join(dir, "logs", "console.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		slog.Warn("写控制台说明失败", "instance", instanceID, "error", err)
		return
	}
	if _, err := safepath.ResolveWithin(dir, filepath.Dir(logPath)); err != nil {
		slog.Warn("拒绝写控制台说明：日志目录疑似软链接逃逸",
			"instance", instanceID, "dir", filepath.Dir(logPath), "error", err)
		return
	}
	f, err := safepath.OpenNoFollow(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Warn("写控制台说明失败", "instance", instanceID, "error", err)
		return
	}
	defer f.Close()
	msg := "\r\n[平台] 本实例在节点服务重启前就已启动，平台已重新接管它（状态、端口、监控都正常）。" +
		"但控制台输出无法接续 —— 重新跟随容器输出的进程没能起来，" +
		"所以上面最后一行之后不会再有新内容。在「控制台」页点一次「重启」即可恢复输出。\r\n"
	if _, err := f.WriteString(msg); err != nil {
		slog.Warn("写控制台说明失败", "instance", instanceID, "error", err)
	}
}

// Load 从磁盘恢复所有实例（Daemon 启动时调用）。
// 若实例目录中的 PID 文件指向仍存活的进程，则接管为 running 状态。
func (r *Registry) Load() (loaded int, adopted int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entries, err := os.ReadDir(r.baseDir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.baseDir, e.Name())
		sdir := filepath.Join(r.stateDir, e.Name())
		m, err := readMeta(sdir)
		if err != nil {
			continue // 非托管目录，跳过
		}
		if m.ID == "" {
			m.ID = e.Name()
		}
		if _, exists := r.instances[m.ID]; exists {
			continue
		}

		// 容器实例的接管语义与 native 不同：容器有自己的 PID 命名空间，
		// pidfile 里的数字在 docker 这边没有意义 —— 直接问 docker 更可靠。
		//
		// 这一段必须放在 native 的 pidfile 判断**之前**：容器实例启动时写的
		// pidfile 是 docker CLI 的 PID，Daemon 重启后那个 CLI 早就没了，
		// 于是会走进"没在运行"的分支 —— 而容器里的服务端其实还在跑，
		// 面板却显示已停止，用户一点启动就撞名/撞端口。
		if m.Container {
			inst := r.registerContainerInstance(m, dir, sdir)
			if inst == nil {
				continue
			}
			r.instances[m.ID] = inst
			if inst.Adopted() {
				adopted++
			}
			loaded++
			continue
		}

		pid := mcprocess.ReadPIDFile(sdir)
		if pid > 0 && mcprocess.IsAlive(pid) {
			inst := mcprocess.NewAdopted(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, pid)
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
			r.bindContainer(inst, m)
			r.bindRunAs(inst)
			r.instances[m.ID] = inst
			adopted++
		} else if opid, found := mcprocess.FindOrphan(dir); found {
			// PID 文件缺失或已失效，但实例目录下仍有 java 在跑。
			//
			// 必须接管：否则面板会认为实例「已停止」，而那个孤儿进程仍占着
			// Minecraft 的 session.lock，导致用户点击「启动」必然失败，
			// 且报出与真实原因无关的 "already locked"。
			inst := mcprocess.NewAdopted(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, opid)
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
			r.bindRunAs(inst)
			r.instances[m.ID] = inst
			adopted++
			slog.Warn("PID 记录缺失或失效，但检测到实例进程仍在运行，已按孤儿进程接管",
				"instance", m.ID, "pid", opid)
		} else {
			inst := mcprocess.NewInstance(m.ID, dir, sdir, m.JarPath, m.MaxMem, m.MinMem)
			// 必须恢复自定义启动命令：否则 Daemon 重启后实例会退回默认 java 命令
			inst.StartCommand = m.StartCommand
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
			r.bindRunAs(inst)
			r.instances[m.ID] = inst
		}
		loaded++
	}
	return loaded, adopted
}

// Get 获取实例。
func (r *Registry) Get(id string) (*mcprocess.Instance, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	inst, ok := r.instances[id]
	return inst, ok
}

// Delete 注销实例（需先停止）。实例文件保留在磁盘，仅移除元数据（可恢复）。
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	inst, ok := r.instances[id]
	if !ok {
		return fmt.Errorf("实例不存在")
	}
	if inst.Status() == "running" {
		return fmt.Errorf("实例运行中，请先停止")
	}
	sdir := filepath.Join(r.stateDir, id)
	_ = os.Remove(filepath.Join(sdir, metaFile))
	_ = os.Remove(filepath.Join(sdir, "daemon.pid"))
	delete(r.instances, id)
	// 专用系统用户一并清理。失败不报错：用户可能已被手工删掉，
	// 也可能还被别的进程占着 —— 删实例这件事不该因为顺带清理而中断。
	if r.runner != nil {
		r.runner.Remove(id)
	}
	return nil
}

// SetJarPath 更新实例使用的核心 jar 并持久化到 instance.json（重启后生效）。
func (r *Registry) SetJarPath(id, jarPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	inst, ok := r.instances[id]
	if !ok {
		return fmt.Errorf("实例不存在")
	}
	if inst.Status() == "running" {
		return fmt.Errorf("实例运行中，请先停止后再切换核心")
	}

	sdir := filepath.Join(r.stateDir, id)
	m, err := readMeta(sdir)
	if err != nil {
		return fmt.Errorf("读取实例元数据失败: %w", err)
	}
	m.JarPath = jarPath
	if err := writeMeta(sdir, m); err != nil {
		return fmt.Errorf("写入实例元数据失败: %w", err)
	}
	return nil
}

// SetJavaVersion 更新实例使用的 JDK 并持久化到 instance.json（下次启动生效）。
//
// 与 SetJarPath 不同，**不要求实例处于停止状态**：换 JDK 不会动正在跑的那个 JVM
//（它启动时就把解释器定死了），只影响下一次启动 —— 为此逼用户先停服没有意义。
// 面板会把"下次启动生效"写在界面上，不留"改了却没变"的错觉。
//
// 取值与建实例时一致：主版本号（"21"）或 java 可执行文件的绝对路径；空 = 自动（PATH）。
func (r *Registry) SetJavaVersion(id, javaVersion string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.instances[id]; !ok {
		return fmt.Errorf("实例不存在")
	}
	sdir := filepath.Join(r.stateDir, id)
	m, err := readMeta(sdir)
	if err != nil {
		return fmt.Errorf("读取实例元数据失败: %w", err)
	}
	m.JavaVersion = javaVersion
	if err := writeMeta(sdir, m); err != nil {
		return fmt.Errorf("写入实例元数据失败: %w", err)
	}
	return nil
}

// SetLimits 修改实例的 CPU / 内存上限，并持久化到实例元数据。
//
// 为什么需要它（2026-10-02）：这两个值原先**只在建实例时**由 CreateInstance 写进
// instance.json，此后面板没有任何办法改 —— 节点用户建实例时把内存上限留空
//（= 不限制），运营侧就永远收不回来，只能删库重建。
//
// 与 SetJavaVersion 同一类做法，也是同一条理由：容量上限**不是**运行中的进程
// 能自己改的东西 —— Daemon 在实例启动时把 cpu.max / memory.max 写进 cgroup，
// 运行中改这两个值不会影响已经起来的那个 JVM。所以本方法只落元数据，
// 真正生效在**下一次启动**；面板会把这个语义写在界面上。
//
// 语义与建实例时一致：cpuQuota 为百分比（100 = 1 核，0 = 不限制），
// memLimit 形如 "4G"（空串 = 不限制）。
func (r *Registry) SetLimits(id string, cpuQuota int, memLimit string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.instances[id]; !ok {
		return fmt.Errorf("实例不存在")
	}
	sdir := filepath.Join(r.stateDir, id)
	m, err := readMeta(sdir)
	if err != nil {
		return fmt.Errorf("读取实例元数据失败: %w", err)
	}
	m.CPUQuota = cpuQuota
	m.MemLimit = memLimit
	if err := writeMeta(sdir, m); err != nil {
		return fmt.Errorf("写入实例元数据失败: %w", err)
	}
	// 内存里的那份也要跟着改：/runtime 之类的接口读的是 registry 里的实例，
	// 只改文件的话，面板刚改完、界面上还是旧数字（"改了却没变"）。
	if inst, ok := r.instances[id]; ok && inst != nil {
		inst.SetLimits(cpuQuota, memLimit)
	}
	return nil
}

// List 列出所有实例 ID（有序）。
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.instances))
	for id := range r.instances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ---- 元数据读写 ----
//
// 注意这些函数的第一个参数是**平台状态目录**（<state_dir>/<实例ID>），
// 不是实例目录 —— 见 New 的说明。

func writeMeta(stateDir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// 0600：元数据里有 jar 路径、配额等，只有 root 的 Daemon 需要读它
	return os.WriteFile(filepath.Join(stateDir, metaFile), b, 0o600)
}

func readMeta(stateDir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(stateDir, metaFile))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	return m, nil
}

// Dir 返回实例目录的绝对路径。
func (r *Registry) Dir(id string) string {
	return filepath.Join(r.baseDir, id)
}

// RecordStart 记录一次成功启动：累加开机次数并记下启动时刻。
// 由 gRPC 层在启动成功后调用；写入失败只记日志，不影响实例运行。
func (r *Registry) RecordStart(id string) {
	r.bumpCounter(id, true)
}

// RecordStop 记录一次停止（正常停止与强制关闭都计入）。
func (r *Registry) RecordStop(id string) {
	r.bumpCounter(id, false)
}

func (r *Registry) bumpCounter(id string, isStart bool) {
	sdir := r.StateDir(id)
	m, err := readMeta(sdir)
	if err != nil {
		return // 非托管实例，忽略
	}
	if isStart {
		m.StartCount++
		m.LastStartAt = time.Now().Unix()
	} else {
		m.StopCount++
	}
	if err := writeMeta(sdir, m); err != nil {
		slog.Warn("写入启停统计失败", "instance", id, "error", err)
	}
}

// Runtime 返回累计启停次数与最近一次启动时间。
func (r *Registry) Runtime(id string) (startCount, stopCount int, lastStartAt int64) {
	m, err := readMeta(r.StateDir(id))
	if err != nil {
		return 0, 0, 0
	}
	return m.StartCount, m.StopCount, m.LastStartAt
}
