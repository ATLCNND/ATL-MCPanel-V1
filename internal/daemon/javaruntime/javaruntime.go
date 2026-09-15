// Package javaruntime 探测节点上实际安装的 JDK，并把"版本标签"解析成
// java 可执行文件的绝对路径。
//
// 为什么单独一个包：它同时被两处需要 ——
//   - mcprocess 在拼启动命令时要用它把实例的 java_version 变成真正的可执行文件；
//   - gRPC 层要把可用 JDK 列表报给面板做下拉框。
//
// 若把这部分塞进 resources（共享资源目录）里，mcprocess 就得依赖资源存储，
// 而进程管理跟"管理员上传了什么文件"毫无关系 —— 那是错误的耦合方向。
package javaruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Runtime 一个可用的 Java 运行时。
type Runtime struct {
	// Label 展示名（主版本号，如 "21"）
	Label string
	// Path java 可执行文件的绝对路径
	Path string
	// Home JAVA_HOME
	Home string
	// Version 完整版本串（来自 release 文件，取不到时为空）
	Version string
}

// jvmRoots 常见的 JDK 安装根目录。
//
// 只扫这几个固定位置而不是全盘 find：全盘搜索在节点上代价很高，
// 而且会捞出一堆无关的 java（某个软件自带的内嵌 JRE 之类）。
var jvmRoots = []string{
	"/usr/lib/jvm",
	"/usr/java",
	"/opt/java",
	"/opt/jdk",
	"/Library/Java/JavaVirtualMachines",
}

// Discover 探测节点上实际安装的 JDK（按主版本号从大到小）。
//
// 只列真实存在且可执行的 java —— 这是"Java 版本"这个字段能真正生效的前提。
// 给一个固定列表（8/11/17/21）看着更友好，但节点上没装的版本选了也起不来，
// 反而制造"我明明选了 17"的困惑。
func Discover() []Runtime {
	seen := map[string]bool{}
	out := []Runtime{}

	add := func(home string) {
		if home == "" {
			return
		}
		bin := filepath.Join(home, "bin", "java")
		// macOS 的 JDK 还要多一层 Contents/Home
		if info, err := os.Stat(bin); err != nil || info.IsDir() {
			bin = filepath.Join(home, "Contents", "Home", "bin", "java")
			home = filepath.Join(home, "Contents", "Home")
			if info, err := os.Stat(bin); err != nil || info.IsDir() {
				return
			}
		}
		if !executable(bin) {
			return
		}
		abs, err := filepath.Abs(bin)
		if err != nil {
			return
		}
		// 按**真实路径**去重：/usr/bin/java 通常是指向
		// /usr/lib/jvm/<jdk>/bin/java 的符号链接，不去重就会在列表里
		// 多出一条"Java usr"这种从 /usr 推出来的假选项。
		key := abs
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			key = real
			// 用真实路径反推 JAVA_HOME，展示名才不会变成 "usr"
			home = filepath.Dir(filepath.Dir(real))
		}
		if seen[key] {
			return
		}
		seen[key] = true
		// Path 记录**解析后的真实路径**而不是符号链接：把"Java 21"这个选择
		// 钉死在具体那个 JDK 上。否则管理员之后执行 update-alternatives
		// 把 /usr/bin/java 切到 17，所有实例会跟着静默换版本 ——
		// 而用户界面里明明还写着 21。
		out = append(out, Runtime{Path: key, Home: home, Label: labelFor(home), Version: readRelease(home)})
	}

	// PATH 上的 java 始终可用，作为兜底选项
	if p, err := exec.LookPath("java"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			add(filepath.Dir(filepath.Dir(abs)))
		}
	}

	for _, root := range jvmRoots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				add(filepath.Join(root, e.Name()))
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return MajorOf(out[i].Label) > MajorOf(out[j].Label)
	})
	return out
}

// Resolve 把 java_version 解析成 java 可执行文件的绝对路径。
//
// 返回空串表示无法解析 —— 调用方应回退到裸 `java`（交给 PATH，
// 也交给节点上的 alternatives 机制），而不是直接启动失败：
// 把"版本选了个不存在的"变成"起不来"会让实例卡在一个很难懂的错误上。
func Resolve(version string) string {
	v := strings.TrimSpace(version)
	if v == "" {
		return ""
	}
	// 直接填了路径
	if strings.ContainsAny(v, "/\\") {
		if info, err := os.Stat(v); err == nil && !info.IsDir() && executable(v) {
			return v
		}
		return ""
	}
	v = strings.TrimPrefix(strings.ToLower(v), "java") // 容忍 "java21" / "java-21"
	v = strings.TrimLeft(v, "-_ ")
	for _, rt := range Discover() {
		if rt.Label == v {
			return rt.Path
		}
	}
	return ""
}

// Fallback 返回 PATH 上的 java（探测不到任何 JDK 时用）。
func Fallback() string {
	if p, err := exec.LookPath("java"); err == nil {
		return p
	}
	return ""
}

// ---- 内部工具 ----

func executable(p string) bool {
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// labelFor 从目录名里提取主版本号作为展示名。
//
// 处理这几种真实命名：
//
//	java-21-openjdk-amd64        → 21
//	java-1.21.0-openjdk-amd64    → 21   （1 是旧式前缀，不是主版本）
//	jdk-17.0.9                   → 17
//	temurin-21.0.1               → 21
func labelFor(home string) string {
	base := filepath.Base(home)
	segs := strings.FieldsFunc(base, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, seg := range segs {
		n, err := strconv.Atoi(seg)
		if err != nil {
			continue
		}
		// "java-1.21.0" 里的 1 是前缀，跳过它取下一个数字段
		if n == 1 && i+1 < len(segs) {
			if next, err := strconv.Atoi(segs[i+1]); err == nil && next >= 5 {
				return strconv.Itoa(next)
			}
		}
		if n >= 5 { // Java 版本号从 5 起，低于它的都是目录名里的噪声
			return strconv.Itoa(n)
		}
	}
	return base
}

// MajorOf 从展示名里取出主版本号（取不到返回 -1，用于排序）。
func MajorOf(label string) int {
	n, err := strconv.Atoi(strings.TrimSpace(label))
	if err != nil {
		return -1
	}
	return n
}

// readRelease 从 JDK 的 release 文件里读版本串。
func readRelease(home string) string {
	b, err := os.ReadFile(filepath.Join(home, "release"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "JAVA_VERSION=") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "JAVA_VERSION=")), `"`)
		}
	}
	return ""
}
