# ATL-MCPanel 面板容器镜像
#
# ---------------------------------------------------------------------------
# 构建上下文（**不是仓库根**）
# ---------------------------------------------------------------------------
# 这个 Dockerfile 期望的上下文由 deploy/docker/build.sh 组装出来：
#
#   <ctx>/Dockerfile         ← 就是本文件
#   <ctx>/healthcheck.sh
#   <ctx>/payload/atl-panel  ← 面板二进制（必填）
#   <ctx>/payload/frpc       ← 穿透客户端（可选，缺了只是面板穿透不可用）
#   <ctx>/payload/web-dist/  ← 前端静态资源（必填）
#
# 为什么不直接用仓库根做上下文：仓库里的二进制在 bin/<GOOS>-<GOARCH>/ 下、
# 前端在 web/dist 下，二者都不一定存在（干净 clone 里啥也没有）。
# 用 build.sh 先摊平成一个"发布包形状"的目录，Dockerfile 的 COPY 就永远成立 ——
# 否则 OPTIONAL 文件会让 COPY 直接失败（Docker 没有"可选 COPY"）。
#
# ---------------------------------------------------------------------------
# 基础镜像
# ---------------------------------------------------------------------------
# 官方二进制是**完全静态链接**的（见 scripts/build.sh 的说明），所以对基底的要求
# 只有「有个 /bin/sh、有 ca-certificates」两件事。默认用 Debian slim。
#
# 内网/离线环境（例如构建机访问不了 Docker Hub）可以换成本地已有的镜像：
#   BASE=atl-mcpanel-runtime:latest ./deploy/docker/build.sh
ARG BASE=debian:13-slim

FROM ${BASE}

ARG VERSION=dev
LABEL org.opencontainers.image.title="ATL-MCPanel Panel" \
      org.opencontainers.image.description="Minecraft 服务器管理面板（容器化部署）" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/ATLCNND/ATL-MCPanel-V1"

ENV DEBIAN_FRONTEND=noninteractive \
    TZ=Asia/Shanghai \
    ATL_PORT=25080

# ca-certificates：面板要出站访问 GitHub（核心下载）、frps 服务器、节点 gRPC。
# 已经是静态链接，所以**不需要** libc 之外的运行时库。
#
# 装不上时只警告不失败：离线基底（比如本地 debootstrap 出来的 runtime 镜像）里
# 往往连 apt 源都没配好，让它把整个构建卡死很不值 —— 代价只是 TLS 出站会失败。
RUN set -eux; \
    if command -v apt-get >/dev/null 2>&1; then \
      apt-get update >/dev/null 2>&1 || true; \
      apt-get install -y --no-install-recommends ca-certificates tzdata bash >/dev/null 2>&1 || \
        echo "⚠️  无法安装 ca-certificates/busybox 依赖（可能是离线构建）—— HTTPS 出站可能失败"; \
      rm -rf /var/lib/apt/lists/* 2>/dev/null || true; \
    fi

# 以**固定 UID 10001** 运行（与 systemd 部署的 atlpanel 用户是同一套设计：
# 面板不需要 root）。用数字 UID 而不是新建用户，是为了让宿主上的
# `chown -R 10001:10001 ./data` 能对得上 —— 容器里叫什么名字宿主并不关心。
RUN set -eux; \
    if command -v groupadd >/dev/null 2>&1 && ! getent group 10001 >/dev/null 2>&1; then \
      groupadd -g 10001 atlpanel 2>/dev/null || true; \
    fi; \
    if command -v useradd >/dev/null 2>&1 && ! getent passwd 10001 >/dev/null 2>&1; then \
      useradd -u 10001 -g 10001 -M -d /opt/mcpanel -s /usr/sbin/nologin atlpanel 2>/dev/null || \
      useradd -r -u 10001 -M -d /opt/mcpanel -s /bin/false atlpanel 2>/dev/null || true; \
    fi

WORKDIR /opt/mcpanel

COPY payload/ /payload/
RUN set -eux; \
    mkdir -p /opt/mcpanel/bin /opt/mcpanel/data /opt/mcpanel/certs \
             /opt/mcpanel/logs /opt/mcpanel/instances /opt/mcpanel/web/dist; \
    install -m 0755 /payload/atl-panel /opt/mcpanel/bin/atl-panel; \
    if [ -f /payload/frpc ]; then install -m 0755 /payload/frpc /opt/mcpanel/bin/frpc; fi; \
    if [ -d /payload/web-dist ]; then cp -r /payload/web-dist/. /opt/mcpanel/web/dist/; fi; \
    chown -R 10001:10001 /opt/mcpanel/data /opt/mcpanel/certs /opt/mcpanel/logs; \
    rm -rf /payload

COPY healthcheck.sh /opt/mcpanel/healthcheck.sh
COPY config.example.yaml /opt/mcpanel/config.example.yaml
RUN chmod 0755 /opt/mcpanel/healthcheck.sh

USER 10001:10001

# 面板 HTTP + 面板 gRPC。**这两个只是声明**：默认用 host 网络（见 README），
# 那时里面的端口不会被映射，实际监听什么仍由 config.yaml 决定。
EXPOSE 25080 9090

# 用 bash 的 /dev/tcp 做健康检查 —— 镜像里刻意不装 curl/wget。
# 启用安全入口时，记得同时设 ATL_ENTRY，否则健康检查打的是 /api/health 会 404。
HEALTHCHECK --interval=30s --timeout=5s --start-period=25s --retries=3 \
  CMD ["/opt/mcpanel/healthcheck.sh"]

ENTRYPOINT ["/opt/mcpanel/bin/atl-panel"]
CMD ["-config", "/opt/mcpanel/config.yaml"]
