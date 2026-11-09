#!/usr/bin/env bash
# 容器健康检查：不依赖 curl / wget，用 bash 的 /dev/tcp 直接发一个 HTTP 请求。
#
# 为什么不用 curl：镜像刻意保持精简（静态二进制 + 基础系统），
# 为了一个健康检查多装一个 HTTP 客户端不划算，而且 curl 会拖进一堆依赖。
#
# 端口与安全入口**优先从挂载进来的配置文件里读**，读不到才用环境变量兜底：
#   为什么不让环境变量做主：配置是挂载进去的、由用户自己写，容器启动时
#   config 里的 listen 可能根本不是 25080。上一版把默认值写成 25080 且不看配置，
#   结果只要用户改过端口，健康检查就一直 unhealthy（把面板报成故障）。
#
#   ATL_PORT   兜底端口（配置里读不到时用；再没有就 25080）
#   ATL_ENTRY  兜底安全入口（配置里读不到时用）
#   ATL_CONFIG 配置文件路径（默认 /opt/mcpanel/config.yaml）
set -uo pipefail

cfg="${ATL_CONFIG:-/opt/mcpanel/config.yaml}"

read_cfg() {
  # $1 = 键名；只取顶层缩进的 `key:` 行，去掉引号与行尾注释
  [ -r "$cfg" ] || return 0
  sed -n "s/^[[:space:]]*$1:[[:space:]]*[\"']\{0,1\}\([^\"'#[:space:]]*\).*/\1/p" "$cfg" 2>/dev/null | head -n 1
}

listen_addr="$(read_cfg listen)"
port="$(printf '%s' "$listen_addr" | sed -n 's/.*:\([0-9]\{1,5\}\)$/\1/p')"
[ -n "$port" ] || port="${ATL_PORT:-25080}"

entry="$(read_cfg security_entry)"
[ -n "$entry" ] || entry="${ATL_ENTRY:-}"

path="/api/health"
[ -n "$entry" ] && path="/${entry}/api/health"

exec 3<>"/dev/tcp/127.0.0.1/${port}" 2>/dev/null || exit 1
printf 'GET %s HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nConnection: close\r\n\r\n' "$path" "$port" >&3
resp="$(head -c 512 <&3 2>/dev/null || true)"
exec 3<&- 2>/dev/null || true

# 两件事都要成立：HTTP 200 + 响应体里有 {"status":"ok",...}
# 只看端口通是不够的：面板在"数据库打不开"这类故障下端口仍然是通的。
case "$resp" in
  "HTTP/1.1 200"*|"HTTP/1.0 200"*) ;;
  *) exit 1 ;;
esac
printf '%s' "$resp" | grep -q '"status":"ok"' || exit 1
exit 0
