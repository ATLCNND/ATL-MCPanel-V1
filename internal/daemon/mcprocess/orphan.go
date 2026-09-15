package mcprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FindOrphan 查找仍在本实例目录下运行的 java 进程（孤儿进程）。
//
// 为什么需要它：
// 面板启动实例时把工作目录设为实例目录，因此**实例目录的 cwd 是判定的可靠依据**。
// 若强制关闭（SIGKILL）只杀掉了直接子进程而遗漏了更深层的进程，或 Daemon 的
// PID 记录与实际不符，就会出现"面板认为实例已停止、但实际有 java 在跑"的状态。
// 此时再次启动会撞上 Minecraft 的 session.lock 而失败，报出
// "already locked (possibly by other Minecraft instance?)" —— 这个错误对用户
// 毫无指导意义，因为真正的问题是**上一次的进程没死干净**。
//
// 注意：session.lock **文件**残留是无害的（它是 flock，进程退出即释放）；
// 只有存活的进程才会真正持锁。因此不要用"删除锁文件"来解决此问题。
func FindOrphan(dir string) (pid int, found bool) {
	want, err := filepath.Abs(dir)
	if err != nil {
		return 0, false
	}

	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	self := os.Getpid()

	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p == self {
			continue // 非进程目录，或自己
		}
		// cwd 指向实例目录 → 极可能是本实例的进程
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", p))
		if err != nil || cwd != want {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", p))
		if err != nil {
			continue
		}
		if isJavaServerProcess(cmdline) {
			return p, true
		}
	}
	return 0, false
}

// isJavaServerProcess 判断 cmdline 是否属于"真正在跑服务端的 java 进程"。
//
// 判定条件（需同时满足，以减少误报）：
//  1. argv[0] 的可执行文件名是 java（含路径形式 /usr/bin/java）
//  2. 命令行里出现了 .jar —— 服务端启动必然要指定核心 jar
//     （默认模式是 -jar，start.sh 模式 exec 的 java 命令同样带 -jar）
//
// 只判断"含 java"过于宽松：任何 argv[0] 被改成 java 的进程都会被误判，
// 从而错误地阻止用户启动实例。
func isJavaServerProcess(cmdline []byte) bool {
	parts := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	if len(parts) == 0 {
		return false
	}
	exe := filepath.Base(parts[0])
	if exe != "java" && !strings.Contains(exe, "java") {
		return false
	}
	for _, a := range parts[1:] {
		if strings.Contains(a, ".jar") {
			return true
		}
	}
	return false
}

// orphanStartError 生成对用户有帮助的启动失败提示。
//
// 直接抛出 Minecraft 的 "already locked" 会让人以为是存档问题；
// 这里明确告知真实原因与处置方式。
func orphanStartError(pid int) error {
	return fmt.Errorf(
		"检测到该实例仍有进程在运行（PID %d），可能是上次强制关闭后残留。\n"+
			"请先点击「强制关闭」清理，或登录节点执行：kill -9 %d\n"+
			"（这不是存档损坏，仅需结束残留进程即可）", pid, pid)
}
