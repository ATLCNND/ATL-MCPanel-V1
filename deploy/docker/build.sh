#!/usr/bin/env bash
# 组装 Docker 构建上下文并构建面板镜像。
#
# 用法：
#   ./deploy/docker/build.sh                        # 用仓库里已有的产物
#   ./deploy/docker/build.sh --tag 0.11.3           # 指定镜像 tag
#   BASE=atl-mcpanel-runtime:latest ./deploy/docker/build.sh   # 离线/无 Docker Hub
#
# 前提：bin/<GOOS>-<GOARCH>/atl-panel 与 web/dist 已经存在
#   （scripts/build.sh 与 cd web && npm run build 的产物）。
#   直接拿发布包里的 bin/ 与 web-dist/ 也行 —— 脚本两种布局都认。
#
# ---------------------------------------------------------------------------
# 为什么要先摊平上下文
# ---------------------------------------------------------------------------
# Docker 的 COPY 不能"可选"：写 `COPY bin/frpc /…` 时 frpc 不存在会让整个构建失败。
# 而 frpc 恰恰是可选件（没有它只是面板穿透不可用）。所以这里先把要进镜像的东西
# 复制到一个临时 payload 目录，Dockerfile 只 COPY 这个目录，形状永远一致。
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT="$(pwd)"

TAG="${TAG:-atl-mcpanel-panel:${VERSION:-latest}}"
BASE_IMG="${BASE:-debian:13-slim}"
KEEP=""

while [ $# -gt 0 ]; do
  case "$1" in
    --tag) TAG="${2:-}"; shift 2 ;;
    --tag=*) TAG="${1#--tag=}"; shift ;;
    --base) BASE_IMG="${2:-}"; shift 2 ;;
    --base=*) BASE_IMG="${1#--base=}"; shift ;;
    --keep) KEEP=1; shift ;;
    -*) echo "未知选项: $1" >&2; exit 1 ;;
    *) shift ;;
  esac
done

# ---------------------------------------------------------------- 找产物
find_panel() {
  for p in "$ROOT/bin/atl-panel" "$ROOT"/bin/*/atl-panel; do
    [ -f "$p" ] && { echo "$p"; return 0; }
  done
  return 1
}
find_web() {
  for p in "$ROOT/web-dist" "$ROOT/web/dist"; do
    [ -d "$p" ] && [ -f "$p/index.html" ] && { echo "$p"; return 0; }
  done
  return 1
}
find_frpc() {
  for p in "$ROOT/bin/frpc" "$ROOT"/bin/*/frpc; do
    [ -f "$p" ] && { echo "$p"; return 0; }
  done
  return 1
}

PANEL_BIN="$(find_panel)" || { echo "找不到 atl-panel 二进制：先跑 scripts/build.sh" >&2; exit 1; }
WEB_DIR="$(find_web)"      || { echo "找不到前端产物（web/dist 或 web-dist/）：先在 web/ 下 npm run build" >&2; exit 1; }
FRPC_BIN="$(find_frpc || true)"

echo "==> 面板二进制: $PANEL_BIN"
echo "==> 前端目录:   $WEB_DIR"
if [ -n "$FRPC_BIN" ]; then echo "==> frpc:       $FRPC_BIN"; else echo "==> frpc:       无（面板穿透将不可用）"; fi

CTX="$(mktemp -d "${TMPDIR:-/tmp}/atl-docker-ctx.XXXXXX")"
if [ -z "$KEEP" ]; then trap 'rm -rf "$CTX"' EXIT; else echo "==> 保留上下文: $CTX"; fi

mkdir -p "$CTX/payload/atldist"
cp "$PANEL_BIN" "$CTX/payload/atl-panel"
[ -n "$FRPC_BIN" ] && cp "$FRPC_BIN" "$CTX/payload/frpc"
mkdir -p "$CTX/payload/web-dist"
cp -r "$WEB_DIR/." "$CTX/payload/web-dist/"
rmdir "$CTX/payload/atldist" 2>/dev/null || true
cp "$ROOT/deploy/docker/Dockerfile" "$ROOT/deploy/docker/healthcheck.sh" "$CTX/"
cp "$ROOT/config.example.yaml" "$CTX/"

echo "==> 基础镜像:   $BASE_IMG"
echo "==> 目标 tag:   $TAG"
docker build \
  --build-arg "BASE=${BASE_IMG}" \
  --build-arg "VERSION=${VERSION:-dev}" \
  -t "$TAG" "$CTX"

echo
echo "==> 构建完成: $TAG"
docker image inspect "$TAG" --format '    大小: {{.Size}} 字节（{{.Size}} / 1048576 MB 见下）' 2>/dev/null || true
docker images "$TAG" --format '    {{.Repository}}:{{.Tag}}  {{.Size}}  {{.CreatedSince}}'
echo
echo "　下一步（见 deploy/docker/README.md）："
echo "   1) 准备 data/ certs/ config.yaml 三个挂载点，chown -R 10001:10001 data certs"
echo "   2) docker compose -f deploy/docker/docker-compose.yml up -d"
echo "   3) 首个管理员：docker exec -it atl-mcpanel-panel /opt/mcpanel/bin/atl-panel \\"
echo "        -config /opt/mcpanel/config.yaml user create -username admin -role admin -if-empty"
