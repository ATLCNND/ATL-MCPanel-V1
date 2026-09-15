// Package version 提供构建版本信息。
package version

import (
	"fmt"
	"runtime"
)

// 版本信息：可在构建时通过 -ldflags 覆盖
//
//	go build -ldflags "-X github.com/ATLCNND/ATL-MCPanel/internal/common/version.Version=1.2.3"
var (
	Version   = "0.7.0"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String 返回人类可读的版本串。
func String() string {
	return fmt.Sprintf("ATL-MCPanel %s (commit %s, built %s, %s)",
		Version, Commit, BuildTime, runtime.Version())
}

// Short 返回简短版本号（用于节点上报与探测）。
func Short() string {
	return Version
}
