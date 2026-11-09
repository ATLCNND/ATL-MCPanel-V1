package instancekind

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Spec 非 Minecraft 类型（docker / console）的启动规格。
//
// 这个结构会**原样落库**（面板 instances.spec 列）并随创建请求发给 Daemon；
// 它以 JSON 存在，因此新增字段必须保持向后兼容（老记录里没有的字段按零值处理）。
//
// 安全边界（重要）：这里**不允许**出现宿主机任意路径。
//   - Mounts 的 Host 必须是"实例目录下的相对子路径"，由 Daemon 拼成绝对路径并做
//     越界检查（safepath）；否则一个租户就能 `-v /:/host` 把整个节点挂进容器。
//   - Ports 的 Host 端口只能由面板的端口分配给出（穿透/直出），不由用户自由填写。
type Spec struct {
	// Image 容器镜像（docker 类型必填）。
	Image string `json:"image,omitempty"`
	// Cmd 覆盖镜像的启动命令（容器内 argv；空 = 用镜像自带的）。
	Cmd []string `json:"cmd,omitempty"`
	// Entrypoint 覆盖 entrypoint（空 = 用镜像自带的）。
	Entrypoint []string `json:"entrypoint,omitempty"`
	// Mounts 卷挂载：容器内路径 ↔ 实例目录下的相对子路径。
	Mounts []Mount `json:"mounts,omitempty"`
	// Ports 端口映射：宿主端口（面板分配）↔ 容器内端口。
	Ports []PortMap `json:"ports,omitempty"`
	// Env 环境变量（KEY=VALUE，键名受限）。
	Env []string `json:"env,omitempty"`
	// WorkingDir 容器内工作目录（空 = /data，即实例目录的挂载点）。
	WorkingDir string `json:"workdir,omitempty"`
	// ReadOnlyRoot 容器根文件系统只读（默认 true；镜像需要写根目录时显式关掉）。
	ReadOnlyRoot *bool `json:"read_only_root,omitempty"`
}

// Mount 一条卷挂载。
type Mount struct {
	// Host 实例目录下的相对路径（"." 或 "" 表示实例目录本身）。
	Host string `json:"host"`
	// Target 容器内绝对路径。
	Target string `json:"target"`
	// ReadOnly 只读挂载。
	ReadOnly bool `json:"read_only,omitempty"`
}

// PortMap 一条端口映射。
type PortMap struct {
	Host      int  `json:"host"`      // 宿主机端口（面板分配，1..65535）
	Container int  `json:"container"` // 容器内端口（1..65535）
	UDP       bool `json:"udp,omitempty"`
}

// 镜像名：允许 registry:port/path:tag 与 digest。
// 刻意不接受空串与空白字符 —— 镜像名直接进 docker 命令行，是命令注入的入口之一。
var imageRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@-]{0,254}$`)

// 环境变量名：POSIX 风格。
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// Encode 序列化成 JSON（空 Spec 返回空串，便于"这一列没有内容"的语义）。
func (s Spec) Encode() (string, error) {
	if s.IsZero() {
		return "", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// IsZero 是否完全为空。
func (s Spec) IsZero() bool {
	return s.Image == "" && len(s.Cmd) == 0 && len(s.Entrypoint) == 0 &&
		len(s.Mounts) == 0 && len(s.Ports) == 0 && len(s.Env) == 0 &&
		s.WorkingDir == "" && s.ReadOnlyRoot == nil
}

// DecodeSpec 从 JSON 还原（空串 → 零值）。
func DecodeSpec(s string) (Spec, error) {
	var out Spec
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return Spec{}, fmt.Errorf("实例规格不是合法 JSON: %w", err)
	}
	return out, nil
}

// Validate 校验规格是否可用于该类型。
//
// 只做**与类型无关的结构校验**（镜像名形态、端口范围、挂载目标必须绝对路径、
// 环境变量键名）。实例目录越界检查由 Daemon 在拼路径时用 safepath 完成 ——
// 面板不知道实例目录在哪，也不该知道。
func (s Spec) Validate(k Kind) error {
	k = Normalize(string(k))
	if !k.Has(CapImage) && s.Image != "" {
		return fmt.Errorf("%s 类型的实例不需要镜像", k.Label())
	}
	if k.Has(CapImage) && strings.TrimSpace(s.Image) == "" {
		return fmt.Errorf("请填写容器镜像（如 nginx:alpine）")
	}
	if s.Image != "" && !imageRe.MatchString(s.Image) {
		return fmt.Errorf("镜像名 %q 不合法：只允许字母、数字与 . _ / : @ -", s.Image)
	}
	if !k.Has(CapVolumes) && len(s.Mounts) > 0 {
		return fmt.Errorf("%s 类型的实例不支持卷挂载", k.Label())
	}
	if !k.Has(CapPorts) && len(s.Ports) > 0 {
		return fmt.Errorf("%s 类型的实例不支持自定义端口映射", k.Label())
	}
	if !k.Has(CapEnv) && len(s.Env) > 0 {
		return fmt.Errorf("%s 类型的实例不支持环境变量", k.Label())
	}

	seen := map[int]bool{}
	for i, m := range s.Mounts {
		if strings.HasPrefix(m.Host, "/") || strings.HasPrefix(m.Host, `\`) ||
			strings.Contains(m.Host, "..") {
			return fmt.Errorf("第 %d 条挂载的源路径 %q 必须是实例目录下的相对路径", i+1, m.Host)
		}
		if !strings.HasPrefix(m.Target, "/") {
			return fmt.Errorf("第 %d 条挂载的目标路径 %q 必须是容器内的绝对路径", i+1, m.Target)
		}
		if strings.Contains(m.Target, "..") {
			return fmt.Errorf("第 %d 条挂载的目标路径不允许出现 ..", i+1)
		}
	}
	for i, p := range s.Ports {
		if p.Host < 1 || p.Host > 65535 {
			return fmt.Errorf("第 %d 条端口映射的宿主端口 %d 超出范围（1-65535）", i+1, p.Host)
		}
		if p.Container < 1 || p.Container > 65535 {
			return fmt.Errorf("第 %d 条端口映射的容器端口 %d 超出范围（1-65535）", i+1, p.Container)
		}
		key := p.Container
		if p.UDP {
			key += 100000 // 同端口号 TCP/UDP 可以共存
		}
		if seen[key] {
			return fmt.Errorf("容器端口 %d 重复映射", p.Container)
		}
		seen[key] = true
	}
	for i, kv := range s.Env {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return fmt.Errorf("第 %d 条环境变量 %q 必须是 KEY=VALUE", i+1, kv)
		}
		if !envKeyRe.MatchString(kv[:eq]) {
			return fmt.Errorf("第 %d 条环境变量的键名 %q 不合法（只能字母、数字、下划线，且不以数字开头）", i+1, kv[:eq])
		}
		// 值里禁止换行：环境变量会进 docker 命令行参数，换行没有正当用途却容易被
		// 用来把一行参数拆成两条。
		if strings.ContainsAny(kv[eq+1:], "\r\n\x00") {
			return fmt.Errorf("第 %d 条环境变量的值不允许换行或控制字符", i+1)
		}
	}
	if s.WorkingDir != "" && !strings.HasPrefix(s.WorkingDir, "/") {
		return fmt.Errorf("容器内工作目录 %q 必须是绝对路径", s.WorkingDir)
	}
	// 启动命令只挡控制字符：命令本身就是用户给的任意 argv（这正是通用 Docker 的用途），
	// 所以不限制内容，只保证它不会把一条 docker 参数拆成两条。
	for i, a := range s.Cmd {
		if strings.ContainsAny(a, "\r\n\x00") {
			return fmt.Errorf("启动命令的第 %d 段含有换行或控制字符", i+1)
		}
	}
	for i, a := range s.Entrypoint {
		if strings.ContainsAny(a, "\r\n\x00") {
			return fmt.Errorf("entrypoint 的第 %d 段含有换行或控制字符", i+1)
		}
	}
	return nil
}

// CleanHostSub 归一化挂载源路径（"." / "" → 实例目录本身）。
func (m Mount) CleanHostSub() string {
	h := strings.TrimSpace(m.Host)
	if h == "" || h == "." || h == "./" {
		return "."
	}
	return path.Clean(h)
}

// CleanMountHost 校验并归一化"实例目录内相对路径"。
//
// 与 CleanHostSub 的区别：这个函数**拒绝**绝对路径、`..` 与空路径，
// 返回的一定是可安全 Join 到实例目录后的相对子路径（"." 代表目录本身）。
// Daemon 组卷时用它把关，面板保存规格时也用它做早期校验。
func CleanMountHost(rel string) (string, error) {
	h := strings.TrimSpace(rel)
	if h == "" || h == "." || h == "./" {
		return ".", nil
	}
	if strings.HasPrefix(h, "/") || strings.HasPrefix(h, `\`) {
		return "", fmt.Errorf("挂载源路径 %q 必须是实例目录下的相对路径（不能以 / 开头）", rel)
	}
	clean := path.Clean(h)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("挂载源路径 %q 不能跳出实例目录", rel)
	}
	return clean, nil
}
