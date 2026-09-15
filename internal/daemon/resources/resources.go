// Package resources 管理节点上的**共享资源**与运行环境探测。
//
// 共享资源是管理员在节点上统一上传、供本节点所有实例复用的文件
//（主要是服务端核心 jar，也可以是数据包、模组等）。
//
// 为什么要有这一层：同一台节点上十个实例，如果每个都往自己目录里塞一份
// 80MB 的 paper.jar，就是 800MB 的重复占用；升级核心时还要逐个替换。
// 统一放一份、实例按路径引用，才是这台节点上"一份资源多处复用"的做法。
//
// 与实例目录的关系：资源目录**独立于**实例根目录（见 DaemonConfig.ResourceDir），
// 因此删除实例、清理实例目录都不会波及共享资源。
package resources

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
)

// maxResourceSize 单个共享资源的大小上限（与实例 jar 上传共用同一常量）。
const maxResourceSize = grpclimits.MaxUploadBytes

// allowedExt 允许上传的资源类型。
//
// 收窄到白名单而不是"什么都收"：这是管理员往**节点**上放文件，
// 而这个目录会被实例的启动命令直接引用（`java -jar <path>`），
// 放开任意扩展名等于给了一条把可执行内容送进启动链的路。
var allowedExt = map[string]bool{
	".jar": true,
	".zip": true, // 数据包 / 模组包
	".tar": true,
	".gz":  true,
	".tgz": true,
}

// safeName 资源文件名的合法形态。
//
// 与实例 ID 同理：这个名字会参与拼路径，必须排除分隔符与 `..`。
// 允许中文与空格（管理员上传的中文名 jar 很常见），但禁止控制字符。
var safeName = regexp.MustCompile(`^[^/\\\x00-\x1f]{1,128}$`)

// File 一个共享资源。
type File struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // 节点上的绝对路径
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	// Refs 引用该资源的实例 ID（由调用方填充）
	Refs []string `json:"refs,omitempty"`
}

// Store 资源目录的读写。
type Store struct {
	dir string
}

// NewStore 创建资源存储。dir 为空时返回 nil（表示该功能不可用）。
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &Store{dir: dir}
}

// Dir 返回资源目录的绝对路径（可能尚未创建）。
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	abs, err := filepath.Abs(s.dir)
	if err != nil {
		return s.dir
	}
	return abs
}

// List 列出全部共享资源（按名称排序）。目录不存在时返回空列表而不是错误 ——
// "还没上传过任何资源"是正常状态，不是故障。
func (s *Store) List() ([]File, error) {
	if s == nil {
		return nil, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []File{}, nil
		}
		return nil, err
	}
	out := make([]File, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, File{
			Name:    e.Name(),
			Path:    filepath.Join(s.Dir(), e.Name()),
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Save 写入一个共享资源（同名覆盖）。
func (s *Store) Save(name string, content []byte) (File, error) {
	if s == nil {
		return File{}, fmt.Errorf("未配置资源目录")
	}
	name = strings.TrimSpace(name)
	if err := validateName(name); err != nil {
		return File{}, err
	}
	if len(content) == 0 {
		return File{}, fmt.Errorf("文件内容为空")
	}
	if len(content) > maxResourceSize {
		return File{}, fmt.Errorf("文件过大（%.1f MB），上限 %d MB",
			float64(len(content))/1024/1024, maxResourceSize>>20)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return File{}, fmt.Errorf("创建资源目录失败: %w", err)
	}

	dst := filepath.Join(s.Dir(), name)
	// 先写临时文件再改名：上传中断不会留下半个文件被当成有效资源
	tmp := dst + ".part"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		_ = os.Remove(tmp)
		return File{}, fmt.Errorf("写入资源失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return File{}, fmt.Errorf("保存资源失败: %w", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return File{}, err
	}
	return File{Name: name, Path: dst, Size: info.Size(), ModTime: info.ModTime().Unix()}, nil
}

// Remove 删除一个共享资源。
//
// 调用方负责先检查引用（本包不感知实例注册表）；这里只做路径与存在性校验。
func (s *Store) Remove(name string) error {
	if s == nil {
		return fmt.Errorf("未配置资源目录")
	}
	name = strings.TrimSpace(name)
	if err := validateName(name); err != nil {
		return err
	}
	dst := filepath.Join(s.Dir(), name)
	if _, err := os.Stat(dst); err != nil {
		return fmt.Errorf("资源不存在: %s", name)
	}
	return os.Remove(dst)
}

// validateName 校验资源文件名。
func validateName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("文件名无效")
	}
	if !safeName.MatchString(name) {
		return fmt.Errorf("文件名含非法字符（不能包含路径分隔符）")
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("文件名不能包含路径")
	}
	ext := strings.ToLower(filepath.Ext(name))
	if !allowedExt[ext] {
		return fmt.Errorf("不支持的文件类型 %q（仅支持 .jar / .zip / .tar / .gz / .tgz）", ext)
	}
	return nil
}

// Touch 确保资源目录存在（Daemon 启动时调用，让路径错误尽早暴露）。
func (s *Store) Touch() error {
	if s == nil {
		return nil
	}
	return os.MkdirAll(s.dir, 0o755)
}

// HumanSize 把字节数格式化成人读的形式（供日志与提示使用）。
func HumanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
