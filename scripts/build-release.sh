#!/usr/bin/env bash
# 打包「面板包」与「节点控制包」。
#
# 产物（dist/ 下）：
#   atl-mcpanel-<版本>-linux-<arch>-panel.tar.gz   + .sha256
#   atl-mcpanel-<版本>-linux-<arch>-node.tar.gz    + .sha256
#
# 为什么分两个包：
#   - 面板包给"管理机"，要带前端静态资源，体积大；
#   - 节点包给"每台实例机"，只要 daemon，越小越好分发。
#   节点包**不含**面板的东西（前端、面板单元），避免误把管理机的角色装到节点上。
#
# ---------------------------------------------------------------------------
# 用法
# ---------------------------------------------------------------------------
#   ./build-release.sh                    # 本机架构
#   ./build-release.sh --arch all         # amd64 + arm64 都出
#   ./build-release.sh --arch arm64
#   ./build-release.sh --version 1.0.0
#   ./build-release.sh --skip-build       # 复用 bin/ 里已有的二进制
#   ./build-release.sh --skip-web         # 复用 web/dist（不重跑 npm）
# ---------------------------------------------------------------------------
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
ARCH=""
SKIP_BUILD=""
SKIP_WEB=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --arch) ARCH="${2:-}"; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --skip-web) SKIP_WEB=1; shift ;;
    -h|--help) sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
fi
if [ -z "$ARCH" ]; then
  ARCH="$(go env GOARCH)"
fi
case "$ARCH" in
  all) ARCHES="amd64 arm64" ;;
  amd64|arm64|arm) ARCHES="$ARCH" ;;
  *) echo "不支持的 --arch: $ARCH（可用 amd64 / arm64 / all）" >&2; exit 1 ;;
esac

COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

echo "==> 版本 ${VERSION}（commit ${COMMIT}）"
echo "==> 架构 ${ARCHES}"
echo

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
  echo "错误：缺少 web/dist，请先构建前端（bash scripts/build-web.sh）或去掉 --skip-web" >&2
  exit 1
fi
echo "==> 前端产物 $(du -sh web/dist | cut -f1)"

# ---------- 2. 二进制 ----------
if [ -z "$SKIP_BUILD" ]; then
  echo
  echo "==> 构建二进制（静态）"
  # 逐个架构调用 build.sh：build.sh 的 --targets 只接一个值，
  # 一次塞 "linux/amd64 linux/arm64" 会把第二个当成未知参数
  # （实测报 "未知参数: linux/arm64"）。
  for a in $ARCHES; do
    bash scripts/build.sh --version "$VERSION" --targets "linux/$a"
  done
fi

# ---------- 3. 组装 ----------
# 清理上一次的产物，但**保留运行时镜像 tar**：
#   · 它由 scripts/build-runtime-image.sh 单独产出（要 debootstrap，几分钟）；
#   · 一上来 `rm -rf dist` 会把它删掉，于是"随包分发镜像"这件事会静默失效 ——
#     打包照常成功，只是节点包里少了 runtime/，到节点上才发现容器化用不了。
#
# 清理范围还要**按架构收窄**（2026-10-01 踩到）：原来是"删掉所有 `-linux-*` 产物"，
# 于是 `--arch amd64` 打完包、再来一次 `--arch arm64`，会把刚打好的 amd64 包一起删掉 ——
# 发布时以为四个包都在，dist 里其实只剩最后那一个。
# `.sha256` 原来也是全局删，会连带删掉运行时镜像的校验和（那份不由本脚本重新生成）。
mkdir -p dist
for a in $ARCHES; do
  find dist -maxdepth 1 -name "atl-mcpanel-*-linux-${a}-*" -exec rm -rf {} + 2>/dev/null || true
  find dist -maxdepth 1 -name "atl-mcpanel-*-linux-${a}-*.tar.gz.sha256" -delete 2>/dev/null || true
done

# 放进包里的用户向文档（HANDOFF / V1-CLOSEOUT / V2* 这类内部文档一律不进包）
PKG_DOCS_PANEL="DEPLOYMENT.md CERTIFICATES.md MTLS.md ARCHITECTURE.md API.md"
PKG_DOCS_NODE="DEPLOYMENT.md MTLS.md CERTIFICATES.md"

# copy_frpc 把**与包同架构**的 frpc 放进包里（$1=架构 $2=目标 bin 目录 $3=用途说明）。
#
# 为什么单独抽出来并校验架构（2026-10-01 踩到）：原来是
#   frpc="dist/frpc-linux-${arch}"; [ -f "$frpc" ] || frpc="dist/frpc"
# 也就是**找不到就退回 dist/frpc** —— 而那份是 amd64 的。于是 arm64 包里
# 装着一个 x86-64 的 frpc：文件在、大小对、`bash -n` 也过得去，
# 只有真在 arm64 机器上跑才会报 "cannot execute binary file"，
# 而现象是"穿透怎么都起不来"，跟"包不全"完全不像。
#
# 现在的规则：优先用带架构后缀的那份；退回 dist/frpc 时**先验架构**，
# 不匹配就**不复制**并给出显式警告 —— 宁可不带（用户能自己放），
# 也不带一个在目标机器上根本跑不起来的二进制。
copy_frpc() {
  local arch="$1" dest="$2" why="$3"
  local f="dist/frpc-linux-${arch}"
  [ -f "$f" ] || f="dist/frpc"
  if [ ! -f "$f" ]; then
    echo "    提示：未找到 frpc（该包不含穿透客户端，$why）" >&2
    return 0
  fi
  local want="x86-64"
  [ "$arch" = "arm64" ] && want="aarch64"
  if ! file -b "$f" | grep -q "$want"; then
    echo "    ⚠️ $f 不是 ${arch} 架构（$(file -b "$f" | cut -c1-40)…），**不放进本包**：" >&2
    echo "       请把 ${arch} 的 frpc 放到 dist/frpc-linux-${arch} 后重跑。" >&2
    return 0
  fi
  cp "$f" "$dest/frpc"
  echo "    含 frpc $(du -h "$f" | cut -f1)（$why）"
}

make_pkg() {
  local arch="$1" kind="$2"
  local name="atl-mcpanel-${VERSION}-linux-${arch}-${kind}"
  local out="dist/${name}"
  local srcbin="bin/linux-${arch}"

  [ -d "$srcbin" ] || { echo "❌ 缺少 $srcbin，先构建" >&2; return 1; }

  mkdir -p "$out/bin" "$out/deploy/systemd" "$out/docs"

  # 二进制
  if [ "$kind" = "panel" ]; then
    cp "$srcbin/dsh-panel" "$out/bin/"
    # 面板的一键部署节点要用**同版本**的 daemon，所以面板包里也放一份
    [ -f "$srcbin/dsh-daemon" ] && cp "$srcbin/dsh-daemon" "$out/bin/"
    # frpc 也要放一份：一键部署会把"面板二进制旁边"的 frpc 一并下发到节点
    # （见 httpapi/nodes.go 的 readSiblingBinary），这样穿透开箱可用。
    copy_frpc "$arch" "$out/bin" "供一键部署下发"
    cp -r web/dist "$out/web-dist"
  else
    cp "$srcbin/dsh-daemon" "$out/bin/"
    # frpc 随节点包分发（**穿透开箱可用**的前提）。
    #
    # 为什么随包发：Daemon 用 `exec.LookPath("frpc")` 找它（见 internal/daemon/grpcapi/
    # frpstest.go），节点上没有 frpc 时"穿透"整块不可用；而"自己去 GitHub 下 frp 再解压"
    # 对用户是纯粹的额外负担 —— 节点包本来就带运行时镜像了，多带一个 frpc 最省事。
    # 找不到时只提示、不算失败：穿透是可选能力。
    copy_frpc "$arch" "$out/bin" "穿透客户端"
    # 容器隔离防火墙规则随节点包分发：它是**容器化可用的前提**（容器能访问宿主
    # 服务就等于没隔离），且必须由 systemd 在 docker 之后落地，不能只写在文档里。
    cp deploy/systemd/atl-container-firewall.service "$out/deploy/systemd/" 2>/dev/null || true

    # 实例运行时基础镜像（容器化隔离用）随**节点包**分发。
    #
    # 为什么不放到面板包里：只有跑实例的机器才需要它，而它有 44MB ——
    # 放进面板包会让每台管理机都白白多下载一次。
    # 镜像不存在时只提示、不算失败：容器化是可选能力，没有它节点照常跑 native。
    img="dist/atl-mcpanel-runtime-${VERSION}-${arch}.tar.gz"
    if [ -f "$img" ]; then
      mkdir -p "$out/runtime"
      cp "$img" "$out/runtime/"
      echo "    含运行时镜像 $(du -h "$img" | cut -f1)"
    else
      echo "    提示：未找到 $img（容器化隔离将不可用）" >&2
      echo "          生成：bash scripts/build-runtime-image.sh --version ${VERSION} --arch ${arch}" >&2
    fi
  fi

  # 部署脚本与单元
  cp deploy/install.sh "$out/deploy/"
  cp deploy/upgrade.sh "$out/deploy/"
  if [ "$kind" = "panel" ]; then
    cp deploy/systemd/atlmcpanel-panel.service "$out/deploy/systemd/"
    cp scripts/gen-cert.sh "$out/deploy/"
  else
    cp deploy/systemd/atlmcpanel-daemon.service "$out/deploy/systemd/"
  fi
  chmod +x "$out/deploy/"*.sh 2>/dev/null || true

  # 配置示例
  cp config.example.yaml "$out/"

  # 门面文件
  for f in README.md CHANGELOG.md LICENSE THIRD-PARTY-NOTICES.md; do
    [ -f "$f" ] && cp "$f" "$out/"
  done

  # 文档
  local d
  if [ "$kind" = "panel" ]; then d="$PKG_DOCS_PANEL"; else d="$PKG_DOCS_NODE"; fi
  for f in $d; do [ -f "docs/$f" ] && cp "docs/$f" "$out/docs/"; done

  # VERSION
  cat > "$out/VERSION" <<EOF
version: ${VERSION}
commit: ${COMMIT}
built: ${BUILD_TIME}
platform: linux/${arch}
package: ${kind}
EOF

  chmod +x "$out/bin/"* 2>/dev/null || true

  # 打包
  ( cd dist && tar czf "${name}.tar.gz" "${name}" )
  ( cd dist && sha256sum "${name}.tar.gz" > "${name}.tar.gz.sha256" )
  rm -rf "$out"

  printf '    %-52s %s\n' "${name}.tar.gz" "$(du -h "dist/${name}.tar.gz" | cut -f1)"
}

echo
echo "==> 组装部署包"
for a in $ARCHES; do
  make_pkg "$a" panel
  make_pkg "$a" node
done

# ---------- 4. 校验：包内容物是否齐全、有没有串味 ----------
echo
echo "==> 校验包内容"
FAIL=0
# 只校验**部署包**：dist/ 里现在还有运行时镜像 tar（atl-mcpanel-runtime-*.tar.gz），
# 它不是部署包，按部署包的标准去查必然"缺文件"（实测把打包直接判成了失败）。
for f in dist/atl-mcpanel-*-linux-*.tar.gz; do
  [ -f "$f" ] || continue
  list=$(tar tzf "$f")
  base=$(basename "$f" .tar.gz)
  need="LICENSE VERSION config.example.yaml deploy/install.sh deploy/upgrade.sh README.md"
  if echo "$base" | grep -q -- '-panel$'; then
    need="$need bin/dsh-panel web-dist/index.html deploy/systemd/atlmcpanel-panel.service"
  else
    need="$need bin/dsh-daemon deploy/systemd/atlmcpanel-daemon.service"
    # 节点包里不该有面板专属文件
    if echo "$list" | grep -qE 'bin/dsh-panel|web-dist'; then
      echo "    ❌ $base 里混进了面板专属文件" >&2
      FAIL=1
    fi
  fi
  for n in $need; do
    echo "$list" | grep -qx "$base/$n" || { echo "    ❌ $base 缺少 $n" >&2; FAIL=1; }
  done
done
[ "$FAIL" -eq 0 ] && echo "    ✅ 所有包的必需文件齐全，且没有互相串味"

# ---------- 5. 校验：包里的脚本语法正确 ----------
echo
echo "==> 校验脚本语法"
for f in dist/atl-mcpanel-*-linux-*.tar.gz; do
  base=$(basename "$f" .tar.gz)
  tmp=$(mktemp -d)
  tar xzf "$f" -C "$tmp"
  for s in "$tmp/$base/deploy/"*.sh; do
    bash -n "$s" 2>/dev/null || { echo "    ❌ $(basename "$s") 语法错误" >&2; FAIL=1; }
  done
  rm -rf "$tmp"
done
[ "$FAIL" -eq 0 ] && echo "    ✅ 全部通过 bash -n"

echo
if [ "$FAIL" -ne 0 ]; then
  echo "❌ 打包未通过校验" >&2
  exit 1
fi

echo "==> 完成"
# 用 du 而不是 `ls | awk '{print $9}'`：ls 的日期列在"近期文件/远期文件"下字段数不同，
# $9 会取空（实测打印出来只剩大小，文件名整列是空的）。
for f in dist/*.tar.gz; do
  [ -f "$f" ] || continue
  printf '    %-56s %s\n' "$(basename "$f")" "$(du -h "$f" | cut -f1)"
done
echo "    （另有同名 .sha256 校验和文件）"
echo
echo "分发："
echo "  scp dist/atl-mcpanel-${VERSION}-linux-amd64-panel.tar.gz root@<管理机>:/tmp/"
echo "  ssh root@<管理机> 'tar xzf /tmp/*-panel.tar.gz -C /tmp && /tmp/atl-mcpanel-*/deploy/install.sh panel'"
