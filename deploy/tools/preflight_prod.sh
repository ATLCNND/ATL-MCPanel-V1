#!/usr/bin/env bash
# 生产机安装前置检查（只读，不改任何东西）
#
# 用法：
#   bash deploy/tools/preflight_prod.sh              # 面板机
#   bash deploy/tools/preflight_prod.sh node         # 节点机（额外查 docker / cgroup / 实例用户前缀）
#
# 输出：每个检查一行 ✅ / ⚠️ / ❌，最后给出 SUMMARY。
# 退出码：0 = 没有 ❌；1 = 有 ❌（. 需要人工先解决）。
set -u

MODE="${1:-panel}"
[ "$MODE" = "node" ] || [ "$MODE" = "panel" ] || { echo "用法: $0 [panel|node]"; exit 2; }

FAIL=0
OKC=0
WARNC=0
ok()   { echo "  ✅ $*"; OKC=$((OKC+1)); }
warn() { echo "  ⚠️  $*"; WARNC=$((WARNC+1)); }
bad()  { echo "  ❌ $*"; FAIL=$((FAIL+1)); }
have() { command -v "$1" >/dev/null 2>&1; }

echo "=== 0. 基本环境 ==="
echo "  主机名: $(hostname 2>/dev/null || echo '?')"
if [ -r /etc/os-release ]; then . /etc/os-release; echo "  系统:   ${PRETTY_NAME:-unknown}"; else echo "  系统:   未知"; fi
echo "  内核:   $(uname -r)  架构: $(uname -m)"
echo "  当前用户: $(id -un) (uid=$(id -u))"
[ "$(id -u)" -eq 0 ] || warn "不是 root：安装脚本需要 root（建议 sudo bash deploy/install.sh …）"

echo "=== 1. systemd ==="
if have systemctl && [ -d /run/systemd/system ]; then
  ok "systemd 可用（$(systemctl --version | head -n1)）"
  if systemctl list-unit-files 'atlmcpanel*' 2>/dev/null | grep -q atlmcpanel; then
    echo "     已存在单元："; systemctl list-unit-files 'atlmcpanel*' 2>/dev/null | sed 's/^/       /'
    warn "这台机器上已经装过面板/节点（升级请走 deploy/upgrade.sh，别重复 install）"
  else
    ok "没有旧的 atlmcpanel 单元"
  fi
else
  bad "没有 systemd：install.sh 装不了服务，只能用 --no-service 手动起（不推荐用于生产）"
fi

echo "=== 2. 端口占用（面板 8080/8443/9090，节点 9091） ==="
if have ss; then
  for p in 8080 8443 9090 9091; do
    line=$(ss -ltnp 2>/dev/null | grep -E "[:.]$p[[:space:]]" | head -n1)
    if [ -n "$line" ]; then echo "     端口 $p: $line"; warn "端口 $p 已被占用，安装前要换端口或停掉占用者"; else echo "     端口 $p: 空闲"; fi
  done
else
  warn "没有 ss（iproute2），跳过端口检查"
fi

echo "=== 3. 磁盘与内存 ==="
root_avail_kb=$(df -Pk /opt 2>/dev/null | awk 'NR==2{print $4}')
if [ -n "${root_avail_kb:-}" ]; then
  avail_gb=$((root_avail_kb/1024/1024))
  if [ "$avail_gb" -ge 10 ]; then ok "/opt 所在分区剩余 ${avail_gb}G"; else bad "/opt 所在分区只剩 ${avail_gb}G（面板+实例+备份建议 ≥10G）"; fi
else
  warn "查不到 /opt 的剩余空间"
fi
echo "     内存: $(awk '/MemTotal/{printf "%.1fG", $2/1024/1024}' /proc/meminfo)   CPU: $(nproc) 核"

echo "=== 4. 目录与用户 ==="
for d in /opt/mcpanel /opt/mcpanel/data /opt/mcpanel/instances; do
  [ -e "$d" ] && echo "     存在: $d ($(stat -c '%U:%G %a' "$d" 2>/dev/null))"
done
if id atlpanel >/dev/null 2>&1; then
  ok "面板用户 atlpanel 已存在（uid=$(id -u atlpanel)）——升级场景正常"
else
  ok "面板用户 atlpanel 不存在（首次安装时 install.sh 会建）"
fi
if have getent; then
  n=$(getent passwd | grep -c '^atl-i-' || true)
  [ "${n:-0}" -gt 0 ] && echo "     已有 ${n} 个实例运行用户（atl-i-*）"
fi

if [ "$MODE" = "node" ]; then
  echo "=== 5. 节点能力（容器 / cgroup） ==="
  if have docker; then
    if docker info >/dev/null 2>&1; then ok "docker 可用（$(docker --version)）"; else bad "docker 装了但 daemon 不可用（docker info 失败）"; fi
    if docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null | grep -q '^atl-mcpanel-runtime:'; then
      ok "运行时镜像已在本机（$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^atl-mcpanel-runtime:' | head -n3 | tr '\n' ' ')）"
    else
      warn "没有 atl-mcpanel-runtime 镜像：Minecraft 实例（容器化模式）起不来；install.sh 会尝试 docker load 包内 tar"
    fi
  else
    warn "没有 docker：Docker 类型实例与「容器化 Minecraft」都不可用（纯宿主机模式仍可跑）"
  fi
  if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    ok "cgroup v2（controllers: $(cat /sys/fs/cgroup/cgroup.controllers)）"
  else
    warn "不是 cgroup v2：内存/CPU 配额可能不生效（面板会降级为不加配额）"
  fi
  avail=$(df -Pk /opt 2>/dev/null | awk 'NR==2{print $4}'); [ -n "${avail:-}" ] && echo "     实例目录所在分区剩余 $((avail/1024/1024))G"
  for t in java docker; do
    if have "$t"; then echo "     $t: $(command -v "$t")"; fi
  done
fi

echo "=== 6. 出网（拉镜像 / 同步时间） ==="
if have ping; then ping -c1 -W2 1.1.1.1 >/dev/null 2>&1 && ok "能出网（1.1.1.1 可达）" || warn "1.1.1.1 不可达：docker 拉镜像会失败，需预先导入运行时镜像"; else warn "没有 ping，跳过网络检查"; fi
if have timedatectl; then timedatectl show -p NTPSynchronized --value 2>/dev/null | grep -q yes && ok "时间已同步（NTP）" || warn "时间未同步：证书有效期/备份时间会不准"; fi

echo
echo "SUMMARY: ok=$OKC warn=$WARNC fail=$FAIL mode=$MODE"
[ "$FAIL" -eq 0 ] && echo "PREFLIGHT_OK（可以安装）" || echo "PREFLIGHT_HAS_BLOCKERS（先解决 ❌ 项）"
exit $([ "$FAIL" -eq 0 ] && echo 0 || echo 1)
