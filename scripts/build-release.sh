#!/usr/bin/env bash
# 打包脚本：构建并生成可分发压缩包
#
# 产物：dist/atlmcpanel-<版本>-<平台>.tar.gz
#   bin/dsh-panel  bin/dsh-daemon
#   web/dist/            前端静态资源
#   deploy/              systemd 单元与安装脚本
#   config.example.yaml  配置示例
#   docs/                文档（含部署与证书指南）
#   VERSION              版本信息
#
# 用法：
#   ./build-release.sh                 # 自动取 git 版本
#   ./build-release.sh --version 1.2.3
#   ./build-release.sh --skip-web      # 跳过前端构建（前端产物已存在时）
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
SKIP_WEB=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --skip-web) SKIP_WEB=1; shift ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
fi

ARCH=$(go env GOARCH)
OS=$(go env GOOS)
NAME="atlmcpanel-${VERSION}-${OS}-${ARCH}"
OUT="dist/${NAME}"

echo "==> 版本: ${VERSION}"
echo "==> 平台: ${OS}/${ARCH}"
echo "==> 输出: ${OUT}"

# ---------- 1. 前端 ----------
if [ -z "$SKIP_WEB" ]; then
  if command -v npm >/dev/null 2>&1; then
    echo "==> 构建前端"
    ( cd web && npm run build )
  else
    echo "警告：未找到 npm，跳过前端构建（将使用现有 web/dist）" >&2
  fi
fi

if [ ! -d web/dist ]; then
  echo "错误：缺少 web/dist，请先构建前端或去掉 --skip-web" >&2
  exit 1
fi

# ---------- 2. 后端 ----------
echo "==> 构建后端二进制"
bash scripts/build.sh --version "$VERSION"

# ---------- 3. 组装 ----------
echo "==> 组装发布包"
rm -rf "$OUT"
mkdir -p "$OUT"/{bin,web,deploy,docs}

cp bin/dsh-panel bin/dsh-daemon "$OUT/bin/"
cp -r web/dist "$OUT/web/dist"
cp -r deploy/. "$OUT/deploy/"
cp config.example.yaml "$OUT/"
cp README.md "$OUT/" 2>/dev/null || true

# 文档：部署、证书、mTLS 是关键运维资料，一并打包
for f in DEPLOYMENT.md CERTIFICATES.md MTLS.md ARCHITECTURE.md API.md; do
  [ -f "docs/$f" ] && cp "docs/$f" "$OUT/docs/"
done

cat > "$OUT/VERSION" <<EOF
version: ${VERSION}
commit: $(git rev-parse --short HEAD 2>/dev/null || echo unknown)
built: $(date -u '+%Y-%m-%dT%H:%M:%SZ')
platform: ${OS}/${ARCH}
EOF

chmod +x "$OUT/deploy/install.sh" "$OUT/bin/"* 2>/dev/null || true

# ---------- 4. 打包 ----------
echo "==> 生成压缩包"
( cd dist && tar czf "${NAME}.tar.gz" "${NAME}" )

# 校验和，便于分发后核对完整性
( cd dist && sha256sum "${NAME}.tar.gz" > "${NAME}.tar.gz.sha256" )

echo
echo "==> 完成"
ls -lh "dist/${NAME}.tar.gz" "dist/${NAME}.tar.gz.sha256"
echo
echo "部署到目标机器："
echo "  scp dist/${NAME}.tar.gz root@<主机>:/tmp/"
echo "  ssh root@<主机> 'tar xzf /tmp/${NAME}.tar.gz -C /tmp && /tmp/${NAME}/deploy/install.sh panel'"
