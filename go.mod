module github.com/ATLCNND/ATL-MCPanel

go 1.26.0

// 工具链**下限**（不是"建议"）：go 命令会按这一行切换/下载对应 toolchain。
//
// 2026-10-24 由 go1.26.8 提到 go1.26.10：CVE-2026-111943（crypto/x509 对已泄露
// 中间 CA 的屏蔽未生效）的修复在 1.26.10 / 1.27.3。
// **只升级构建机上的 Go 不够** —— 实测这里会把 1.26.10 又"降"回 1.26.8 去构建，
// 产出的二进制自报 go1.26.8。发版后请用 atl-panel -version 核对 go_version，
// 别只看构建机上 go version 的输出。
toolchain go1.26.10

require (
	github.com/golang-jwt/jwt/v5 v5.2.2
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-sqlite3 v1.14.24
	github.com/pkg/sftp v1.13.6
	golang.org/x/crypto v0.56.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/fs v0.1.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
)
