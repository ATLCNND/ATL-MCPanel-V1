// Package registry 管理 Daemon 上的所有 MC 实例，并把元数据持久化到磁盘，
// 使 Daemon 重启后仍能恢复实例列表（并对遗留进程做接管）。
package registry

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
)

// Meta 实例元数据（持久化到 <实例目录>/instance.json）。
type Meta struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	CoreType     string `json:"core_type"`
	JavaVersion  string `json:"java_version"`
	JarPath      string `json:"jar_path"`
	CPUQuota     int    `json:"cpu_quota"` // CPU 配额百分比（100 = 1 核；0 = 不限制）
	// BackupDir 该实例备份的存放目录（留空则用节点配置的 backup_root，
	// 再退回到实例目录下的 backups/）
	BackupDir string `json:"backup_dir,omitempty"`

	// MemLimit cgroup 内存上限（如 "4G"；留空 = 不限制）。
	// 需为 JVM 堆外开销留余量，建议 -Xmx × 1.3 + 512MB。
	MemLimit string `json:"mem_limit,omitempty"`

	// ---- 运行统计（供详情页展示运行时长与启停次数）----
	StartCount  int   `json:"start_count"`   // 累计开机次数
	StopCount   int   `json:"stop_count"`    // 累计关机次数（含强制关闭）
	LastStartAt int64 `json:"last_start_at"` // 最近一次启动的 Unix 时间戳
	MaxMem       string `json:"max_mem"`
	MinMem       string `json:"min_mem"`
	Port         int32  `json:"port"`
	StartCommand string `json:"start_command"`
}

// Registry 实例注册表。
type Registry struct {
	mu        sync.RWMutex
	instances map[string]*mcprocess.Instance
	baseDir   string // 实例根目录，如 /opt/mcpanel/instances
}

// New 创建注册表。
// baseDir 会被规范化为绝对路径：实例目录会作为子进程工作目录与配置路径使用，
// 相对路径在 cmd.Dir 变更后会解析错误（曾导致 frpc 找不到配置文件）。
func New(baseDir string) *Registry {
	if abs, err := filepath.Abs(baseDir); err == nil {
		baseDir = abs
	}
	return &Registry{
		instances: make(map[string]*mcprocess.Instance),
		baseDir:   baseDir,
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建实例目录失败: %w", err)
	}
	if err := writeMeta(dir, m); err != nil {
		return nil, fmt.Errorf("写入实例元数据失败: %w", err)
	}

	inst := mcprocess.NewInstance(m.ID, dir, m.JarPath, m.MaxMem, m.MinMem)
	inst.StartCommand = m.StartCommand
	inst.JavaVersion = m.JavaVersion
	inst.CPUQuotaPercent = m.CPUQuota
	inst.BackupDir = m.BackupDir
	inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
	inst.Port = int(m.Port) // 启动前要按它校准 server.properties
	r.instances[m.ID] = inst
	return inst, nil
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
		m, err := readMeta(dir)
		if err != nil {
			continue // 非托管目录，跳过
		}
		if m.ID == "" {
			m.ID = e.Name()
		}
		if _, exists := r.instances[m.ID]; exists {
			continue
		}

		pid := mcprocess.ReadPIDFile(dir)
		if pid > 0 && mcprocess.IsAlive(pid) {
			inst := mcprocess.NewAdopted(m.ID, dir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, pid)
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
			r.instances[m.ID] = inst
			adopted++
		} else if opid, found := mcprocess.FindOrphan(dir); found {
			// PID 文件缺失或已失效，但实例目录下仍有 java 在跑。
			//
			// 必须接管：否则面板会认为实例「已停止」，而那个孤儿进程仍占着
			// Minecraft 的 session.lock，导致用户点击「启动」必然失败，
			// 且报出与真实原因无关的 "already locked"。
			inst := mcprocess.NewAdopted(m.ID, dir, m.JarPath, m.MaxMem, m.MinMem, m.StartCommand, opid)
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
			r.instances[m.ID] = inst
			adopted++
			slog.Warn("PID 记录缺失或失效，但检测到实例进程仍在运行，已按孤儿进程接管",
				"instance", m.ID, "pid", opid)
		} else {
			inst := mcprocess.NewInstance(m.ID, dir, m.JarPath, m.MaxMem, m.MinMem)
			// 必须恢复自定义启动命令：否则 Daemon 重启后实例会退回默认 java 命令
			inst.StartCommand = m.StartCommand
			inst.JavaVersion = m.JavaVersion
			inst.CPUQuotaPercent = m.CPUQuota
			inst.BackupDir = m.BackupDir
			inst.MemLimitBytes = mcprocess.ParseMemBytes(m.MemLimit)
			inst.Port = int(m.Port)
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
	_ = os.Remove(filepath.Join(r.baseDir, id, metaFile))
	_ = os.Remove(filepath.Join(r.baseDir, id, "daemon.pid"))
	delete(r.instances, id)
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

	dir := filepath.Join(r.baseDir, id)
	m, err := readMeta(dir)
	if err != nil {
		return fmt.Errorf("读取实例元数据失败: %w", err)
	}
	m.JarPath = jarPath
	if err := writeMeta(dir, m); err != nil {
		return fmt.Errorf("写入实例元数据失败: %w", err)
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

func writeMeta(dir string, m Meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, metaFile), b, 0o644)
}

func readMeta(dir string) (Meta, error) {
	var m Meta
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
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
	dir := r.Dir(id)
	m, err := readMeta(dir)
	if err != nil {
		return // 非托管实例，忽略
	}
	if isStart {
		m.StartCount++
		m.LastStartAt = time.Now().Unix()
	} else {
		m.StopCount++
	}
	if err := writeMeta(dir, m); err != nil {
		slog.Warn("写入启停统计失败", "instance", id, "error", err)
	}
}

// Runtime 返回累计启停次数与最近一次启动时间。
func (r *Registry) Runtime(id string) (startCount, stopCount int, lastStartAt int64) {
	m, err := readMeta(r.Dir(id))
	if err != nil {
		return 0, 0, 0
	}
	return m.StartCount, m.StopCount, m.LastStartAt
}
