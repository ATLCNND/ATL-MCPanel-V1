#!/usr/bin/env bash
# 构建「实例运行时基础镜像」（容器化隔离用）。
#
# 为什么不是 `docker pull debian:12-slim`：
#   * 目标节点（CentOS 7 / 内网机器）**不一定能访问 Docker Hub** —— 实测开发 VM
#     连 registry-1.docker.io / daocloud / 1ms.run / 163 全部超时；
#   * 随部署包分发要求镜像内容**可复现**：哪天上游 slim 镜像变了，
#     节点上的行为就跟着变，出了问题无法回溯。
#
# 因此这里用 debootstrap 在本地捏一个最小 rootfs，`docker import` 成镜像，
# 再 `docker save` 成 tar.gz —— 节点侧 `docker load` 即可，完全不需要出网。
#
# 产物：
#   dist/atl-mcpanel-runtime-<版本>-<架构>.tar.gz  (+ .sha256)
#
# 用法：
#   ./build-runtime-image.sh --version 0.9.14 [--suite bookworm] [--mirror URL]
#   ./build-runtime-image.sh --version 0.9.14 --arch arm64     # 需要 qemu-user-static
#
# 镜像里**故意不装** JDK：节点上的 /usr/lib/jvm 是只读挂进去的（设计如此，
# 见 docs/CONTAINERIZATION.md 3.2）。装进镜像会让镜像大一倍，还会出现
# "面板里选了 21、容器里其实是 17" 这种对不上的情况。
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)"
# suite 的选择只为一件事：**镜像的 glibc 必须 ≥ 节点上 JDK 要求的 glibc**。
#
# 这条是被实测打出来的，不是理论洁癖：先用 bookworm（glibc 2.36）做镜像，
# 把 Debian 13 发行版自带的 openjdk-21（要求 GLIBC_2.38）挂进去，
# java 直接起不来：
#   java: /lib/x86_64-linux-gnu/libc.so.6: version `GLIBC_2.38' not found
# glibc 是**向前兼容**的（老 JDK 能在新 glibc 上跑，反之不行），
# 所以镜像侧取"尽量新且是稳定版"是最优解；trixie 是 Debian 13 stable。
#
# 但这仍不能覆盖"管理员装了更新的发行版 JDK"（例如未来 Debian 14 的 JDK），
# 因此 install.sh 里还有一道**逐 JDK 自检**：容器里跑不起来哪个 JDK，
# 就明说哪个 JDK 不能用容器模式，而不是让实例起不来。
SUITE="trixie"
MIRROR="http://mirrors.aliyun.com/debian"
KEEP_ROOTFS=""

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --arch)    ARCH="${2:-}";    shift 2 ;;
    --suite)   SUITE="${2:-}";   shift 2 ;;
    --mirror)  MIRROR="${2:-}";  shift 2 ;;
    --keep-rootfs) KEEP_ROOTFS=1; shift ;;
    -h|--help) sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done
[ -n "$VERSION" ] || { echo "必须给 --version（会写进镜像 tag 与包名）" >&2; exit 1; }

IMAGE="atl-mcpanel-runtime:${VERSION}-${ARCH}"
ROOTFS="/tmp/atl-runtime-rootfs-${ARCH}"
OUT="dist/atl-mcpanel-runtime-${VERSION}-${ARCH}.tar.gz"

# 容器里需要什么、为什么：
#   bash        节点的 /bin/sh 是 bash，用户的 start.sh 多半是 bash 写法；
#               Debian 的 /bin/sh 是 dash，直接跑会踩 [[ ]] / 数组 这类语法
#   ca-certificates  插件连 HTTPS（Modrinth、各种 API）需要根证书
#   tzdata      日志时间戳要跟节点一致，否则排查时对不上
#   libstdc++6/zlib1g  Temurin JDK 的动态依赖（宿主 JDK 是动态链接的）
#   fontconfig  部分插件/服务端会初始化 AWT（地图渲染、图片处理），
#               缺字体配置时会抛 HeadlessException 之类的怪错
#   procps      插件常调用 ps/uptime 做监控
PKGS="bash,coreutils,ca-certificates,tzdata,zlib1g,libstdc++6,fontconfig,procps"

echo "==> 目标镜像 ${IMAGE}"
command -v debootstrap >/dev/null 2>&1 || {
  echo "缺少 debootstrap，请先：apt-get install -y debootstrap" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "缺少 docker" >&2; exit 1; }

echo "==> 1/4 debootstrap ${SUITE}（${ARCH}）"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS"
debootstrap --variant=minbase --arch="$ARCH" --include="$PKGS" \
  "$SUITE" "$ROOTFS" "$MIRROR"

echo "==> 2/4 精简与标注"
# 减体积：这些目录在容器里永远不会被用到，留着白占几十 MB
rm -rf "$ROOTFS/var/cache/apt"/* "$ROOTFS/var/lib/apt/lists"/* \
       "$ROOTFS/usr/share/doc"/* "$ROOTFS/usr/share/man"/* 2>/dev/null || true
mkdir -p "$ROOTFS/var/lib/apt/lists/partial"

# 版本标记：在节点上 `cat /etc/atl-runtime` 就能知道这镜像是什么时候做的。
# 排查"容器里的行为跟宿主机不一样"时，第一件事就是确认镜像版本。
cat > "$ROOTFS/etc/atl-runtime" <<EOF
image: ${IMAGE}
suite: ${SUITE} (${ARCH})
mirror: ${MIRROR}
built: $(date -u '+%Y-%m-%dT%H:%M:%SZ')
note: 只含运行时用户态，JDK 由节点 /usr/lib/jvm 只读挂入
EOF

# MC 官方容器会设 LANG，这里也设上：默认 C 语言环境下 JVM 的 file.encoding
# 在部分版本上不是 UTF-8，中文插件配置会乱码。
cat > "$ROOTFS/etc/profile.d/atl-locale.sh" <<'EOF'
export LANG=C.UTF-8
export LC_ALL=C.UTF-8
EOF
chmod 0644 "$ROOTFS/etc/profile.d/atl-locale.sh"

echo "==> 3/4 docker import（--numeric-owner 保住 rootfs 的属主）"
tar --numeric-owner -C "$ROOTFS" -c . | docker import \
  --change 'ENV LANG=C.UTF-8' \
  --change 'ENV LC_ALL=C.UTF-8' \
  --change 'ENV HOME=/data' \
  --change 'WORKDIR /data' \
  --change 'LABEL org.atl.image=atl-mcpanel-runtime' \
  --change "LABEL org.atl.version=${VERSION}" \
  - "$IMAGE"

# 常用别名：daemon 默认按名字找 `atl-mcpanel-runtime:latest`，
# 这样升级面板版本不必同步改 daemon 配置。
docker tag "$IMAGE" atl-mcpanel-runtime:latest

echo "==> 4/4 自检 + 导出"
set +e
out=$(docker run --rm -i "$IMAGE" bash -lc 'echo shell-ok; ls /etc/atl-runtime && (command -v bash && command -v ps)' 2>&1)
rc=$?
set -e
echo "$out" | sed 's/^/    /'
[ $rc -eq 0 ] || { echo "❌ 镜像自检失败（rc=$rc）" >&2; exit 1; }

mkdir -p dist
docker save "$IMAGE" | gzip -9 > "$OUT"
( cd dist && sha256sum "$(basename "$OUT")" > "$(basename "$OUT").sha256" )

echo
echo "    $OUT   $(du -h "$OUT" | cut -f1)"
echo "    $OUT.sha256"
echo
echo "节点侧用法（随部署包分发时由 install.sh 自动执行）："
echo "    docker load -i atl-mcpanel-runtime-${VERSION}-${ARCH}.tar.gz"
[ -n "$KEEP_ROOTFS" ] || rm -rf "$ROOTFS"
echo "完成。"
