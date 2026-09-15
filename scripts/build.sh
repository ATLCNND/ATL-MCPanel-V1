#!/usr/bin/env bash
# 构建脚本：编译 panel 与 daemon 两个二进制到 bin/ 目录
#
# 用法：
#   ./build.sh                    # 使用 git 版本信息构建
#   ./build.sh --version 1.2.3    # 指定版本号
#   GOOS=linux GOARCH=amd64 ./build.sh
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
fi
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

LDFLAGS="-X github.com/ATLCNND/ATL-MCPanel/internal/common/version.Version=${VERSION}"
LDFLAGS="${LDFLAGS} -X github.com/ATLCNND/ATL-MCPanel/internal/common/version.Commit=${COMMIT}"
LDFLAGS="${LDFLAGS} -X github.com/ATLCNND/ATL-MCPanel/internal/common/version.BuildTime=${BUILD_TIME}"

mkdir -p bin

echo ">>> 编译 dsh-panel  (版本 ${VERSION}, commit ${COMMIT})"
CGO_ENABLED=1 go build -ldflags "${LDFLAGS}" -o bin/dsh-panel ./cmd/panel

echo ">>> 编译 dsh-daemon"
CGO_ENABLED=1 go build -ldflags "${LDFLAGS}" -o bin/dsh-daemon ./cmd/daemon

echo ">>> 构建完成："
ls -lh bin/

echo
echo "提示：SQLite 驱动依赖 CGO（gcc）。交叉编译请设置对应的 CC。"
