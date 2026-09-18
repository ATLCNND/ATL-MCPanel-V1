// Package runas 管理实例进程的运行身份与文件属主。
//
// 背景（这是本包的唯一存在理由）：
//
//	实例进程原先**以 root 运行**（Daemon 是 root，exec 出来的 java / sh / frpc
//	都继承 root）。而面板允许实例所有者编辑 start.sh、也允许协作者往控制台
//	发命令 —— 这两条路都能拿到 shell，于是"能管一台实例"等价于"拿到节点 root"：
//	可以读别的实例存档、读 mTLS 私钥、动宿主机。
//
//	修法就是不让实例跑成 root，并且把"root 会去读的文件"从租户可写的目录里搬走
//	（见 config 的 StateDir / FrpStateDir 说明）—— 两者缺一不可。
//
// 本包只做运行身份这一半：分配专用系统用户、把实例目录交给它、在 exec 时注入
//
//	凭据。另一半（状态文件搬家）在 daemon 启动时与 frp 管理器里完成。
package runas

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Identity 一个实例应该以什么 uid/gid 运行。
type Identity struct {
	Username string
	UID      uint32
	GID      uint32

	// Home 传给子进程的 HOME 目录。
	//
	// 一定要显式给：per-instance 用户是用 --system 建的、没有真实家目录，
	// 而 JVM 在取不到 passwd 项时会把 user.home 落成 "/"，
	// 于是一些插件写 user.home 下的文件就会在根目录上碰壁（权限拒绝）。
	// 指到实例目录里，行为与"自己机器上开服"一致。
	Home string
}

// Mode 实例运行身份的解析模式。
type Mode int

const (
	// ModePerInstance 每实例一个系统用户（默认）。
	ModePerInstance Mode = iota
	// ModeShared 所有实例共用一个已存在的用户。
	ModeShared
	// ModeCurrent 沿用 Daemon 自身身份（Daemon 不能是 root）。
	ModeCurrent
)

// Manager 解析与创建实例运行身份。
//
// 结果会被缓存：Start 是热路径（每次开机都要用），而 useradd/getent 是子进程，
// 不该在每次启动实例时反复 fork。
type Manager struct {
	mode   Mode
	shared string
	prefix string

	mu    sync.Mutex
	cache map[string]*Identity

	// selfUID 当前进程 uid，用于 ModeCurrent 与 ModeShared 的 root 检查。
	selfUID uint32
}

// ParseMode 解析配置里的 instance_user 取值。
func ParseMode(value, prefix string) (Mode, string, error) {
	if prefix == "" {
		prefix = "atl-i-"
	}
	switch strings.TrimSpace(value) {
	case "", "per-instance":
		return ModePerInstance, prefix, nil
	case "current":
		return ModeCurrent, prefix, nil
	default:
		name := strings.TrimSpace(value)
		if strings.ContainsAny(name, "/ \t\n") {
			return 0, "", fmt.Errorf("instance_user 取值非法：%q（只能是 per-instance / current / 一个已存在的用户名）", value)
		}
		return ModeShared, prefix, nil
	}
}

// New 创建 Manager。
func New(value, prefix string) (*Manager, error) {
	mode, pfx, err := ParseMode(value, prefix)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		mode:    mode,
		prefix:  pfx,
		cache:   map[string]*Identity{},
		selfUID: uint32(os.Geteuid()),
	}
	if mode == ModeShared {
		m.shared = strings.TrimSpace(value)
	}
	return m, nil
}

// Mode 返回当前模式（供启动日志与自检展示）。
func (m *Manager) Mode() Mode { return m.mode }

// Describe 给日志用的一句话说明。
func (m *Manager) Describe() string {
	switch m.mode {
	case ModePerInstance:
		return "每实例独立系统用户（前缀 " + m.prefix + "）"
	case ModeShared:
		return "共用系统用户 " + m.shared
	default:
		return "沿用 Daemon 自身身份"
	}
}

// IsRoot 当前进程是否以 root 运行。
func IsRoot() bool { return os.Geteuid() == 0 }

// Resolve 解析某实例的运行身份。
//
// per-instance 模式下**不创建**用户，只解析；不存在就返回错误并提示调用 Ensure。
// 分开的理由：解析会在每次 Start 时发生，而创建用户是一次性的、还可能失败
// （磁盘满、uid 耗尽），不该把这两件事混在一个每次都要跑的函数里。
func (m *Manager) Resolve(instanceID string) (*Identity, error) {
	m.mu.Lock()
	if id, ok := m.cache[instanceID]; ok {
		m.mu.Unlock()
		return id, nil
	}
	m.mu.Unlock()

	var id *Identity
	var err error
	switch m.mode {
	case ModePerInstance:
		id, err = m.resolvePerInstance(instanceID)
	case ModeShared:
		id, err = m.resolveUser(m.shared)
	case ModeCurrent:
		id, err = m.resolveCurrent()
	}
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache[instanceID] = id
	m.mu.Unlock()
	return id, nil
}

// Ensure 确保实例的运行身份存在（per-instance 模式下按需创建系统用户）。
//
// 只在"创建实例"与"启动实例"时调用。
func (m *Manager) Ensure(instanceID, instanceDir string) (*Identity, error) {
	if m.mode != ModePerInstance {
		return m.Resolve(instanceID)
	}
	name := m.username(instanceID)

	// 已存在就直接用（幂等：重复建实例、Daemon 重启都不该失败）
	if id, err := m.resolveUser(name); err == nil {
		m.remember(instanceID, id)
		return id, nil
	}
	if !IsRoot() {
		return nil, fmt.Errorf(
			"实例 %s 需要专用系统用户 %s，但 Daemon 不是以 root 运行、无法创建用户；"+
				"请以 root 运行 Daemon，或把 instance_user 配成 current（仅当 Daemon 自身不是 root）",
			instanceID, name)
	}

	if err := createSystemUser(name, instanceDir); err != nil {
		// 建不出用户就不能开机：宁可起不来并说清原因，也不要"退回 root 跑"
		return nil, fmt.Errorf("创建实例专用用户 %s 失败：%w", name, err)
	}
	id, err := m.resolveUser(name)
	if err != nil {
		return nil, fmt.Errorf("创建用户 %s 后仍解析不到：%w", name, err)
	}
	m.remember(instanceID, id)
	return id, nil
}

// Remove 删除实例时清理专用用户（per-instance 模式）。
//
// 失败不返回错误：用户可能本来就不存在，或者还被别的进程占着 ——
// 删实例这件事不该因为"顺带清理用户"失败而中断。
func (m *Manager) Remove(instanceID string) {
	if m.mode != ModePerInstance {
		return
	}
	name := m.username(instanceID)
	m.mu.Lock()
	delete(m.cache, instanceID)
	m.mu.Unlock()
	if !IsRoot() {
		return
	}
	// 加 -f：用户已不存在时不算失败。不用 -r：家目录是实例目录（由调用方删），
	// 让 userdel 递归删家目录等于给"删实例"多了一条能删错目录的路。
	_ = exec.Command("userdel", "-f", name).Run()
}

func (m *Manager) username(instanceID string) string {
	// 用户名有长度与字符集限制：只保留 [a-z0-9_-]，其余折成 '-'
	// （实例 ID 本身已被校验为安全目录名，这里再加一道，避免把 ID 直接当用户名用）
	var b strings.Builder
	for _, r := range strings.ToLower(instanceID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := m.prefix + b.String()
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

func (m *Manager) resolvePerInstance(instanceID string) (*Identity, error) {
	return m.resolveUser(m.username(instanceID))
}

func (m *Manager) resolveCurrent() (*Identity, error) {
	if IsRoot() {
		// 这条检查是整个包的关键一行：以 root 跑实例就是被修掉的那个洞，
		// 所以宁可让实例起不来，也不能"照旧跑"。
		return nil, fmt.Errorf(
			"instance_user=current 意味着实例与 Daemon 同身份，而 Daemon 正以 root 运行 —— " +
				"这会重现「实例即 root」的漏洞，已拒绝启动实例。" +
				"请把 instance_user 设为 per-instance（默认），或以普通用户运行 Daemon")
	}
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("取当前用户失败：%w", err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	home := u.HomeDir
	if home == "" {
		home = "/tmp"
	}
	return &Identity{Username: u.Username, UID: uint32(uid), GID: uint32(gid), Home: home}, nil
}

func (m *Manager) resolveUser(name string) (*Identity, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("系统用户 %s 不存在：%w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("用户 %s 的 uid 无法解析：%w", name, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("用户 %s 的 gid 无法解析：%w", name, err)
	}
	if m.mode == ModeShared && uid == 0 {
		return nil, fmt.Errorf("instance_user 指向了 root，实例不允许以 root 运行")
	}
	return &Identity{Username: u.Username, UID: uint32(uid), GID: uint32(gid), Home: u.HomeDir}, nil
}

func (m *Manager) remember(instanceID string, id *Identity) {
	m.mu.Lock()
	m.cache[instanceID] = id
	m.mu.Unlock()
}

// createSystemUser 建一个系统用户。
//
// 刻意不用 shell（`sh -c "useradd ..."`）：用户名是从实例 ID 推出来的，
// 虽然已经过滤过字符集，但把"过滤"当前提、再把变量拼进 shell 命令，
// 是这个项目里不愿再出现的写法。直接 exec 参数数组，注入面为零。
func createSystemUser(name, home string) error {
	// --system：uid 取系统区间（100~999），不占用人类用户号段
	// --user-group：同名用户组，保证 chown 时只动这一个实例
	// --no-create-home：家目录就是实例目录本身，由实例创建流程负责建
	// --shell nologin：这个账号不该能登录，它只是"文件属主 + 运行身份"
	args := []string{
		"--system",
		"--user-group",
		"--no-create-home",
		"--home-dir", home,
		"--shell", nologinShell(),
		name,
	}
	out, err := exec.Command("useradd", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("useradd %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nologinShell 找一个存在的"不可登录" shell。
//
// 路径在不同发行版上不一致（Debian/Ubuntu 是 /usr/sbin/nologin，
// CentOS 7 是 /sbin/nologin，个别精简镜像只有 /bin/false），
// 写死任一个都会在另一种系统上让 useradd 直接失败。
func nologinShell() string {
	for _, p := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/bin/false", "/usr/bin/false"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/bin/false"
}

// Credential 把身份转成 exec 需要的 syscall.Credential。
func (id *Identity) Credential() *syscall.Credential {
	return &syscall.Credential{Uid: id.UID, Gid: id.GID}
}

// ---- 文件属主 ----

// Chown 把单个路径的属主改成实例身份（不存在则忽略）。
//
// 为什么要 per-write 地改属主：Daemon 以 root 写文件时，文件属主是 root，
// 而**真正要读写它的是实例进程**（另一个 uid）。开机时把目录整棵 chown 一次
// 只能保证"这次能跑"；文件管理、解压、恢复备份这些操作都会在运行期往目录里
// 塞新文件，不跟着改属主，实例下次写到同一个文件时就会 permission denied ——
// 表现是"面板里改完配置，服务器却说没权限"。
func Chown(path string, id *Identity) {
	if id == nil || path == "" {
		return
	}
	if _, err := os.Lstat(path); err != nil {
		return // 路径不存在等：不是错误
	}
	_ = os.Chown(path, int(id.UID), int(id.GID))
}

// ChownTree 递归改属主（含目录本身）。
//
// 用 Lchown 语义：符号链接本身改属主、不跟随 —— 跟随的话，一个指向 /etc 的
// 软链接会让这棵树的操作改掉树外文件的属主。
func ChownTree(root string, id *Identity) error {
	if id == nil || root == "" {
		return nil
	}
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			// 单个条目读不到（权限、竞态删除）不该让整棵树失败
			return nil
		}
		_ = os.Lchown(path, int(id.UID), int(id.GID))
		return nil
	})
}

// ChmodOwnerWrite 已移除：chown 之后属主本来就有写权限（文件 0600/0644、
// 目录 0755 都是"属主 rw(x)"），再 chmod 一次只会多一个能改坏东西的地方 ——
// 例如把 start.sh 的 0755 抹成 0644（虽然我们是用 `sh <脚本>` 调用的、
// 不依赖执行位，但没有必要冒这个险）。

// EnsureDir 创建目录并设定模式（幂等）。
//
// mode 语义直接透给 os.Chmod：实例根目录要 0755（实例用户必须能穿过去），
// 而 state / frp 目录要 0700（只有 root 能进，实例用户连列目录都不行）。
func EnsureDir(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	// MkdirAll 对已存在的目录不改模式，所以显式再来一次 ——
	// 老装机上这些目录可能是更宽松的模式，需要收紧/放开到预期值。
	return os.Chmod(path, mode)
}

// CheckTraversable 检查从根到 dir **的每一级上级目录**都允许"其他用户"穿过（o+x）。
//
// 为什么不查 dir 自己：实例目录会被 chown 给实例用户，属主权限就够它进去了，
// 不需要 o+x（0700 属主是自己完全没问题）。真正会卡住的是**上级**：
// /opt 下某层若是 0700 root，实例用户连进都进不去。
//
// 为什么要专门查：实例进程换了 uid 之后，能不能访问自己的目录取决于整条路径，
// 任何一级缺 o+x，实例就会以一个看起来完全无关的错误启动失败（java 报
// "Could not find or load main class"，或者干脆 chdir 失败），而配置、权限、
// jar 全都是好的 —— 这类问题最难查。所以宁可启动前就直说缺哪一级、该敲什么命令。
//
// 返回第一个不可穿过的目录；全部通过返回空字符串。
func CheckTraversable(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	p := filepath.Dir(abs)
	for {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			if fi.Mode().Perm()&0o001 == 0 {
				return p
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}
