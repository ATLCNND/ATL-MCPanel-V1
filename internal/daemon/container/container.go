// Package container 把实例跑进 docker 容器（可选的隔离模式）。
//
// 为什么要有它（2026-09-18 的 T0 修复补完了"降权"与"目录隔离"，但还差一层）：
// 实例进程虽然已经不是 root、也读不到别的实例的目录，但它仍然
//
//	① 能读宿主上任何"世界可读"的文件（/etc/passwd、其它服务的配置）；
//	② 能连宿主回环上的服务（面板 gRPC、Daemon gRPC、frps 管理口）；
//	③ 能看见宿主上所有进程（/proc 共享）；
//	④ 没有只读根、没有 capability 收敛。
//
// 容器（namespace + cgroup + 只读挂载 + cap-drop）正好补这四条。
//
// 设计要点（全部有实测依据，见 docs/CONTAINERIZATION.md 第四节）：
//
//   - **docker CLI 仍然作为子进程跑**：于是 stdin 管道（控制台命令）与
//     stdout 重定向（控制台日志）这两条现成链路完全不用改。
//   - **必须 `--user <uid>:<gid>`**：节点的 user namespace 是禁用的
//     （`user.max_user_namespaces=0`），做不了 rootless；不降权的话
//     容器里的 root 就是宿主 root，等于把 T0 修复全丢掉。
//   - **每个实例一张独立 bridge**：共用一张时实例之间能互相连端口，
//     等于在"实例互相隔离"上重新开个口子。
//   - **端口只发布到 127.0.0.1**：frpc 在宿主上，连的就是宿主回环；
//     容器因此看不到宿主的回环服务。
//   - **磁盘默认只给实例目录**（挂到 /data，只读根 + 可写 /data）。
package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 容器内外路径的对应关系：实例目录挂到 /data。
//
// 为什么不挂成宿主原路径：容器里的路径就是租户看得见的东西，
// 映射成一个固定、简短、与宿主布局无关的名字，既避免泄露宿主目录结构，
// 也让"容器模式下实例目录是 /data"这句话能写进帮助文档。
const MountPoint = "/data"

// DefaultImage 基础镜像的默认 tag（由 scripts/build-runtime-image.sh 产出）。
const DefaultImage = "atl-mcpanel-runtime:latest"

// Runtime 与 docker 打交道的入口。nil 表示不可用（未安装 docker）。
type Runtime struct {
	bin   string
	image string
}

// Detect 找到可用的 docker。返回 nil 表示这台机器不能用容器模式。
//
// 只找可执行文件、不做 `docker info`：探测本身要能在 Daemon 启动路径上快速返回，
// 真正确认能不能跑是在启动实例时（那时失败会记进实例的启动错误里）。
func Detect(image string) *Runtime {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil
	}
	if image == "" {
		image = DefaultImage
	}
	return &Runtime{bin: bin, image: image}
}

// Bin 返回 docker 可执行文件路径（供测试与提示信息）。
func (r *Runtime) Bin() string { return r.bin }

// Image 返回使用的镜像 tag。
func (r *Runtime) Image() string { return r.image }

// NameOf 容器名 / 网络名。用 atl- 前缀，避免和节点上别人起的容器撞名。
func NameOf(instanceID string) string    { return "atl-" + instanceID }
func NetworkOf(instanceID string) string { return "atl-" + instanceID + "-net" }

// ---- 生命周期 ----

// EnsureNetwork 确保实例独占的 bridge 存在（幂等）。
func (r *Runtime) EnsureNetwork(ctx context.Context, instanceID string) error {
	name := NetworkOf(instanceID)
	if r.networkExists(ctx, name) {
		return nil
	}
	if _, err := r.run(ctx, "network", "create", name); err != nil {
		// 并发创建时另一方可能刚好建成：再查一次，存在就算成功
		if r.networkExists(ctx, name) {
			return nil
		}
		return fmt.Errorf("创建容器网络失败: %w", err)
	}
	return nil
}

func (r *Runtime) networkExists(ctx context.Context, name string) bool {
	_, err := r.run(ctx, "network", "inspect", name)
	return err == nil
}

// RemoveNetwork 删除实例的网络（删实例时调用；网络不存在不算错）。
func (r *Runtime) RemoveNetwork(ctx context.Context, instanceID string) error {
	if !r.networkExists(ctx, NetworkOf(instanceID)) {
		return nil
	}
	_, err := r.run(ctx, "network", "rm", NetworkOf(instanceID))
	return err
}

// Remove 删除容器。force=true 相当于强杀（docker rm -f）。
//
// **这是容器模式下"杀死实例"的唯一正确做法**：直接对 docker CLI 进程发
// SIGKILL 只会杀掉 CLI 自己，容器里的 java 会继续跑（PoC A11 验的就是这条）。
func (r *Runtime) Remove(ctx context.Context, instanceID string, force bool) error {
	name := NameOf(instanceID)
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	_, err := r.run(ctx, args...)
	if err != nil && !r.exists(ctx, name) {
		return nil // 本来就没有，算成功
	}
	return err
}

// GracefulStop 用 docker stop 送 SIGTERM（MC 有 shutdown hook，能优雅退出）。
//
// 只在"没有 stdin 可用"时用（例如 Daemon 重启后接管了容器）：
// 正常情况下停止走 stdin 的 stop 命令，与 native 模式完全一致。
func (r *Runtime) GracefulStop(ctx context.Context, instanceID string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 30
	}
	_, err := r.run(ctx, "stop", "-t", strconv.Itoa(secs), NameOf(instanceID))
	return err
}

// State 容器状态（供接管与界面展示）。
type State struct {
	Exists  bool
	Running bool
	ID      string
	Status  string // docker 的 Status 文本（如 Up 3 minutes）
	Started string // 启动时刻（RFC3339，可能为空）
	// Pid 容器 init 进程在**宿主** PID 命名空间里的 PID。
	//
	// 容器有自己的 PID 命名空间，容器内看到的 PID 与宿主不同；
	// 这个值是从宿主视角看到的那个，所以 IsAlive/watchAdopted 这套现成机制
	// 对容器同样可用（Daemon 重启后据此判断容器还在不在）。
	Pid int
}

// Inspect 查询容器状态。容器不存在时返回 Exists=false 且 err=nil。
func (r *Runtime) Inspect(ctx context.Context, instanceID string) (State, error) {
	out, err := r.run(ctx, "inspect", "--format",
		"{{.Id}}|{{.State.Running}}|{{.State.Status}}|{{.State.StartedAt}}|{{.State.Pid}}", NameOf(instanceID))
	if err != nil {
		if !r.exists(ctx, NameOf(instanceID)) {
			return State{}, nil
		}
		return State{}, err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	st := State{Exists: true}
	if len(parts) > 0 {
		st.ID = parts[0]
	}
	if len(parts) > 1 {
		st.Running = parts[1] == "true"
	}
	if len(parts) > 2 {
		st.Status = parts[2]
	}
	if len(parts) > 3 {
		st.Started = parts[3]
	}
	if len(parts) > 4 {
		st.Pid, _ = strconv.Atoi(strings.TrimSpace(parts[4]))
	}
	return st, nil
}

func (r *Runtime) exists(ctx context.Context, name string) bool {
	_, err := r.run(ctx, "inspect", "--format", "{{.Id}}", name)
	return err == nil
}

// ImageLoaded 镜像是否已经在本地（没有就不能起容器，要明说而不是让实例起不来）。
func (r *Runtime) ImageLoaded(ctx context.Context) bool {
	_, err := r.run(ctx, "image", "inspect", "--format", "{{.Id}}", r.image)
	return err == nil
}

// Version 返回 docker 服务端版本（面板显示节点就绪状态用）。
func (r *Runtime) Version(ctx context.Context) string {
	out, err := r.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- 启动参数 ----

// Spec 启动一个实例容器所需的全部信息。
type Spec struct {
	InstanceID string
	// Dir 宿主上的实例目录（挂到 /data，唯一可写之处）。
	Dir string
	// UID/GID 容器内进程的身份（= 该实例的专用用户）。
	UID uint32
	GID uint32
	// MemoryBytes 容器内存上限（含 swap 同值，即不允许换出）；
	// CPUPercent 100 = 1 核。二者都由 Docker 实施，Daemon 的 cgroup 不再插手。
	MemoryBytes int64
	CPUPercent  int
	// Port 游戏端口（>0 时发布到宿主 127.0.0.1）。
	Port int
	// JavaHome 宿主 JDK 根目录（只读挂入；空则不挂）。
	JavaHome string
	// ResourcesDir 节点共享资源目录（只读挂入；空或不存在则不挂）。
	ResourcesDir string
	// Command 容器内要执行的命令，路径必须用**容器内路径**（如 /data/start.sh）。
	Command []string
	// ExtraEnv 额外环境变量（KEY=VALUE）。
	ExtraEnv []string
	// Image 覆盖默认镜像（一般不用）。
	Image string
}

// Argv 生成完整的 docker 命令行（第一个元素是 docker 可执行文件）。
func (r *Runtime) Argv(spec Spec) ([]string, error) {
	if spec.InstanceID == "" || spec.Dir == "" {
		return nil, errors.New("容器启动参数缺少实例 ID 或实例目录")
	}
	if len(spec.Command) == 0 {
		return nil, errors.New("容器启动参数缺少要执行的命令")
	}
	// 挂载路径必须是绝对路径。相对路径 docker 会当成"命名卷"，
	// 报的是 invalid volume specification / mount path must be absolute ——
	// 而这条错误只会出现在实例日志里，很容易误判成 docker 装坏了。
	// （实测：节点配置里 resource_dir 默认是相对路径 "resources"。）
	if !filepath.IsAbs(spec.Dir) {
		return nil, fmt.Errorf("实例目录必须是绝对路径（当前 %q）", spec.Dir)
	}
	if spec.JavaHome != "" && !filepath.IsAbs(spec.JavaHome) {
		return nil, fmt.Errorf("JDK 目录必须是绝对路径（当前 %q）", spec.JavaHome)
	}
	if spec.ResourcesDir != "" && !filepath.IsAbs(spec.ResourcesDir) {
		// 共享资源是可选项，不值得为它让实例起不来 —— 但也不能悄悄挂错，
		// 所以这里只跳过，具体原因由调用方记进日志。
		spec.ResourcesDir = ""
	}
	image := spec.Image
	if image == "" {
		image = r.image
	}

	argv := []string{r.bin, "run", "--rm", "-i", "--name", NameOf(spec.InstanceID)}

	// 降权：容器里的 root 等于宿主 root（节点禁用 user namespace，没有 rootless），
	// 所以这一条不是"加固"，而是正确性的前提。
	argv = append(argv, "--user", fmt.Sprintf("%d:%d", spec.UID, spec.GID))

	// 资源限额交给 Docker（见 docs/CONTAINERIZATION.md 3.3：
	// 同一个进程只能属于一个 cgroup，两边都写会静默失效）。
	if spec.MemoryBytes > 0 {
		mem := strconv.FormatInt(spec.MemoryBytes, 10) + "b"
		argv = append(argv, "--memory", mem, "--memory-swap", mem)
	}
	if spec.CPUPercent > 0 {
		argv = append(argv, "--cpus", strconv.FormatFloat(float64(spec.CPUPercent)/100, 'f', -1, 64))
	}
	argv = append(argv,
		"--pids-limit", "256", // 防 fork 炸弹：一个实例把节点拖垮是很容易的
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp:size=64m",
		"-v", spec.Dir+":"+MountPoint,
		"-w", MountPoint,
	)

	// JDK：只读挂入宿主目录（面板支持按实例选 Java 版本，镜像里不装 JDK）。
	if spec.JavaHome != "" {
		argv = append(argv, "-v", spec.JavaHome+":"+spec.JavaHome+":ro")
		// 发行版 JDK 的配置可能在 JAVA_HOME 之外（Debian 把 java.security
		// 放在 /etc/java-<ver>-openjdk），不挂进去会以一句误导性的
		// "Error loading java.security file" 失败 —— 见 JDKExtraMounts。
		for _, d := range JDKExtraMounts(spec.JavaHome) {
			argv = append(argv, "-v", d+":"+d+":ro")
		}
	}
	if spec.ResourcesDir != "" {
		if fi, err := os.Stat(spec.ResourcesDir); err == nil && fi.IsDir() {
			argv = append(argv, "-v", spec.ResourcesDir+":"+spec.ResourcesDir+":ro")
		}
	}
	// 时区：不挂的话容器是 UTC，服务端日志时间戳会比宿主差 8 小时，
	// 而面板、日志文件、用户截图对时间时就全对不上了。
	if fi, err := os.Stat("/etc/localtime"); err == nil && !fi.IsDir() {
		argv = append(argv, "-v", "/etc/localtime:/etc/localtime:ro")
	}

	argv = append(argv, "-e", "HOME="+MountPoint)
	if spec.JavaHome != "" {
		binDirs := spec.JavaHome + "/bin"
		argv = append(argv,
			"-e", "JAVA_HOME="+spec.JavaHome,
			"-e", "PATH="+binDirs+":/usr/local/bin:/usr/bin:/bin")
	}
	for _, kv := range spec.ExtraEnv {
		argv = append(argv, "-e", kv)
	}

	// 只发布到宿主回环：frpc 在宿主上连的就是它，而容器看不到宿主的回环服务。
	if spec.Port > 0 {
		argv = append(argv, "--publish", fmt.Sprintf("127.0.0.1:%d:%d", spec.Port, spec.Port))
	}
	argv = append(argv, "--network", NetworkOf(spec.InstanceID))
	argv = append(argv, image)
	argv = append(argv, spec.Command...)
	return argv, nil
}

// ---- JDK 外置配置探测 ----

var (
	jdkExtraMu    sync.Mutex
	jdkExtraCache = map[string][]string{}
)

// JDKExtraMounts 找出 JAVA_HOME 里**指到 JAVA_HOME 之外**的符号链接，
// 返回需要一并只读挂入的目录。
//
// 为什么需要它（实测）：Debian 的 openjdk 把安全配置放在 /etc/java-21-openjdk，
// 而 $JAVA_HOME/conf/security/java.security 只是指向它的符号链接。只挂
// /usr/lib/jvm 时这个链接在容器里是悬空的；平时不报错（java.security 是惰性读取），
// 一旦走到需要它的代码路径就抛 InternalError: Error loading java.security file ——
// 错误信息与真实原因（缺配置）对不上，非常容易被带偏。
//
// 归一化规则：目标落在 /etc、/usr、/opt 这类系统目录下时，挂它的**二级目录**
// （如 /etc/java-21-openjdk）—— 一个 JDK 通常会引出十几个同源链接，
// 逐个挂会让命令行很长；其余情况挂目标所在目录本身。
func JDKExtraMounts(javaHome string) []string {
	if javaHome == "" {
		return nil
	}
	jdkExtraMu.Lock()
	if cached, ok := jdkExtraCache[javaHome]; ok {
		jdkExtraMu.Unlock()
		return cached
	}
	jdkExtraMu.Unlock()

	home := filepath.Clean(javaHome)
	var cands []string
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		// 只解析链接本身；目标不存在（悬挂）时跳过 —— 空挂载点没有意义
		target, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil
		}
		target = filepath.Clean(target)
		if target == home || strings.HasPrefix(target, home+string(os.PathSeparator)) {
			return nil
		}
		cands = append(cands, mountRootFor(target))
		return nil
	})

	out := pruneNested(dedupe(cands))
	jdkExtraMu.Lock()
	jdkExtraCache[javaHome] = out
	jdkExtraMu.Unlock()
	return out
}

// mountRootFor 把"逃逸目标文件"归一化成要挂载的目录。
func mountRootFor(target string) string {
	dir := filepath.Dir(target)
	parts := strings.Split(strings.TrimPrefix(dir, string(os.PathSeparator)), string(os.PathSeparator))
	switch {
	case len(parts) >= 2 && (parts[0] == "etc" || parts[0] == "opt" || parts[0] == "srv"):
		// /etc/java-21-openjdk/security → /etc/java-21-openjdk
		return string(os.PathSeparator) + filepath.Join(parts[0], parts[1])
	case len(parts) >= 2 && parts[0] == "usr":
		// /usr/lib/jvm/... 已经挂过了；这里是 /usr/share/... 之类，挂二级目录
		return string(os.PathSeparator) + filepath.Join(parts[0], parts[1])
	}
	return dir
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// pruneNested 去掉被其它候选目录包含的项（挂父目录就够了）。
func pruneNested(in []string) []string {
	var out []string
	for i, a := range in {
		nested := false
		for j, b := range in {
			if i == j || a == b {
				continue
			}
			if strings.HasPrefix(a, b+string(os.PathSeparator)) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, a)
		}
	}
	return out
}

// ---- 执行 ----

func (r *Runtime) run(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.bin, args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(msg)
	}
	return out.String(), nil
}
