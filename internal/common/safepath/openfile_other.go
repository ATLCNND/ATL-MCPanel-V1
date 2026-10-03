//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package safepath

import "os"

// OpenNoFollow 在不支持 O_NOFOLLOW 的平台上退化为普通打开。
//
// 发布产物只有 linux/amd64 与 linux/arm64（musl 静态），走到这里只可能是
// "开发机上看代码"这类场景；真正的边界检查在 ResolveWithin 里。
func OpenNoFollow(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

// IsSymlinkRefusal 在不支持 O_NOFOLLOW 的平台上恒为 false。
func IsSymlinkRefusal(error) bool { return false }
