#!/usr/bin/env bash
# 生成 protobuf 代码（在已装 protoc + protoc-gen-go + protoc-gen-go-grpc 的环境运行）
# Debian 安装：apt-get install -y protobuf-compiler
#             go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#             go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
set -euo pipefail

cd "$(dirname "$0")/.."

# go_package 含完整 module 路径，--go_out 指向项目根，配合 module 选项自动定位
protoc \
  --proto_path=proto \
  --go_out=. \
  --go_opt=module=github.com/ATLCNND/ATL-MCPanel \
  --go-grpc_out=. \
  --go-grpc_opt=module=github.com/ATLCNND/ATL-MCPanel \
  proto/mcpanel.proto

echo "protobuf 代码已生成到 internal/proto/mcpanel/"