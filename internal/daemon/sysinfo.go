package daemon

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// detectOS 返回操作系统描述。
func detectOS() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	pretty := runtime.GOOS
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "PRETTY_NAME=") {
			pretty = strings.Trim(strings.TrimPrefix(l, "PRETTY_NAME="), `"`)
			break
		}
	}
	return pretty
}

// totalMemory 返回系统总内存（字节），通过 /proc/meminfo 读取。
func totalMemory() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "MemTotal:") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				kb, err := strconv.ParseInt(f[1], 10, 64)
				if err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}
