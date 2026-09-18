#!/usr/bin/env bash
# 生成 protobuf 代码（在已装 protoc + protoc-gen-go + protoc-gen-go-grpc 的环境运行）
#
# Debian 安装：
#   apt-get install -y protobuf-compiler
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
#
# 两个插件装在 $GOPATH/bin（通常是 /root/go/bin），不在默认 PATH 里时要先
#   export PATH="$PATH:/root/go/bin"
# 否则 protoc 会报 "protoc-gen-go: program not found or is not executable"。
#
# ⚠️ 本文件必须是 **LF** 换行。曾经是 CRLF，在 Linux 上直接报
#    `set: pipefail: 无效的选项名`（`pipefail\r` 不是合法选项名），
#    而报错位置在 set 那行、内容和 pipefail 完全无关，很容易查错方向。
set -euo pipefail

cd "$(dirname "$0")/.."

# go_package 含完整 module 路径：--go_out 指向项目根，配合 module 选项自动定位
protoc \
  --proto_path=proto \
  --go_out=. \
  --go_opt=module=github.com/ATLCNND/ATL-MCPanel \
  --go-grpc_out=. \
  --go-grpc_opt=module=github.com/ATLCNND/ATL-MCPanel \
  proto/mcpanel.proto

echo "protobuf 代码已生成到 internal/proto/mcpanel/"
