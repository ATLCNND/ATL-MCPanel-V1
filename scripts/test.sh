#!/usr/bin/env bash
# 运行全部检查：go vet + go test
#
# 用法：
#   ./test.sh              # 运行全部
#   ./test.sh -run TestXxx # 只运行匹配的测试
#   ./test.sh -count=1     # 禁用缓存
set -euo pipefail
cd "$(dirname "$0")/.."

echo ">>> go vet"
go vet ./...

echo
echo ">>> go test"
go test ./... "$@"

echo
echo "✅ 全部检查通过"
