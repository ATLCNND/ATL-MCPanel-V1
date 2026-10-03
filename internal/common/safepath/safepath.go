// Package safepath 提供"把用户可控的路径安全地钉在某个目录内"的公共逻辑。
//
// 为什么单独抽一个包：Daemon 里有一整族"以 root 身份、按租户给的路径做文件
// 操作"的地方 —— 文件管理、上传下载、解压、备份与恢复、控制台日志、world
// 统计。它们各自的实现里都做过**词法**边界检查（Clean + 前缀比较），但词法
// 检查挡不住软链接：实例目录的内容完全由租户控制（文件管理能建文件，启动
// 脚本与插件能建软链接），`ln -s /etc/cron.d/x pwn` 之后对 `pwn` 的读写会穿过
// 检查落到目录之外，而 os.WriteFile / os.ReadFile / os.OpenFile **都会跟随
// 软链接**。2026-10-01 的安全审查在四处独立地发现了同一类问题，所以这里给出
// 唯一一份实现，供所有调用点复用。
package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrEscape 目标路径解析后落在允许范围之外。
var ErrEscape = errors.New("路径超出允许访问的目录范围")

// Within 判断 child 是否位于 parent 之内（含相等）。
//
// 必须带分隔符比较：`/data/inst1` 与 `/data/inst10` 用朴素前缀比较会误判为
// "在内"。空 parent 恒为 false。
func Within(parent, child string) bool {
	p, err1 := filepath.Abs(parent)
	c, err2 := filepath.Abs(child)
	if err1 != nil || err2 != nil {
		return false
	}
	if p == c {
		return true
	}
	return strings.HasPrefix(c, p+string(filepath.Separator))
}

// EvalDeepest 解析 path 中**已存在的那一部分**的软链接，并把不存在的尾段接回去。
//
// 直接对整个 path 调 EvalSymlinks 在"目标还没建出来"时会报 ENOENT，而写新
// 文件/建新目录正是常见操作；因此逐级向上找到第一个存在的祖先，解析它，再把
// 中间缺失的各级按原顺序拼回。这样目标已存在时等价于 EvalSymlinks(path)，
// 目标不存在时至少把**已存在的父链**里的软链接暴露出来参与边界检查。
func EvalDeepest(path string) (string, error) {
	cur := path
	var missing []string
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// 退到根仍然不存在（理论上不会发生）：按原样返回，交给调用方报错
			return path, nil
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// ResolveReal 返回 target 解析软链接后的真实路径（含边界检查前的形态）。
// root 自身也可能是软链接（例如 /opt/instances -> /data/instances），所以基准
// 也要取解析后的形态，否则正常路径会被误判成越界。
func ResolveReal(root, target string) (realRoot, realTarget string, err error) {
	realRoot = root
	if r, e := filepath.EvalSymlinks(root); e == nil {
		realRoot = r
	}
	realTarget, err = EvalDeepest(target)
	if err != nil {
		return "", "", err
	}
	return realRoot, realTarget, nil
}

// ResolveWithin 把 root 下的路径 target 做**真实路径**（解析软链接）边界检查，
// 越界返回 ErrEscape。
//
// 调用方通常先用词法方式把用户输入拼成 target（`filepath.Join(root, cleaned)`），
// 再调这里做第二道检查 —— 两道都在才安全：词法那道负责"净化而不是报错"，
// 这一道负责挡住软链接。
func ResolveWithin(root, target string) (string, error) {
	if !Within(root, target) {
		return "", ErrEscape
	}
	realRoot, realTarget, err := ResolveReal(root, target)
	if err != nil {
		return "", err
	}
	if !Within(realRoot, realTarget) {
		return "", ErrEscape
	}
	return realTarget, nil
}

// RelWithin 返回 target 相对于 root 的**真实**相对路径（斜杠分隔、已解析软链接）。
// 用于"解析之后再按相对路径判一次名单"这类二次校验（例如实例内的受保护文件）。
func RelWithin(root, target string) (string, bool) {
	realRoot, realTarget, err := ResolveReal(root, target)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}
