//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package safepath

import (
	"errors"
	"os"
	"syscall"
)

// OpenNoFollow 以"绝不跟随最后一段软链接"的方式打开文件。
//
// 边界检查与实际打开之间总有一个窗口：租户可以在那一刻把目标换成软链接
//（TOCTOU）。O_NOFOLLOW 让内核在最后一段是软链接时直接返回 ELOOP，把这个
// 窗口关掉。注意它只作用于**最终那一段**，路径中间各级仍要靠 ResolveWithin
// 的 EvalSymlinks 检查覆盖 —— 两者是互补的，不是二选一。
func OpenNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
}

// IsSymlinkRefusal 判断错误是否来自"目标是软链接"这类拒绝（ELOOP）。
func IsSymlinkRefusal(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
