#!/usr/bin/env bash
# ATL-MCPanel 安装脚本（面板包 / 节点包通用）
#
# 用法：
#   面板：sudo ./install.sh panel  [安装目录] [--port 端口]
#   节点：sudo ./install.sh daemon [安装目录]
#   仅准备文件、不安装/启动服务：追加 --no-service
#
# 说明：
#   · 面板端口默认**随机**取 20000-25499 之间的空闲端口（避开扫描热点与
#     临时端口区）。要固定端口用 --port 8080 这种方式显式指定；
#     已有配置里的端口一律沿用、不会被改动。
#   · 首个管理员账号由本脚本在安装末尾调用 `atl-panel user create` 创建并
#     打印随机口令。重复执行本脚本不会重置已有账号（-if-empty）。
#
# 脚本假设当前目录（解压后的包目录）包含：
#   bin/  deploy/  config.example.yaml
#   面板包另有：web-dist/
#
# 做八件事：校验二进制 → 装二进制 → 建目录 → 写配置 → 装 systemd 单元
#           → 生成自签证书 → 启动并打印访问地址 → 创建首个管理员
#
# 幂等：可重复执行。**已存在的 config.yaml 与 data/ 一律保留**（升级请用 upgrade.sh）。
set -euo pipefail

MODE=""
INSTALL_DIR=""
NO_SERVICE=""
PANEL_PORT="${PANEL_PORT:-}"

# 用 while + shift 而不是 for：--port 需要一个**值**参数，
# for 逐个吐 token 时拿不到它的下一个 token。
while [ $# -gt 0 ]; do
  case "$1" in
    --no-service) NO_SERVICE=1; shift ;;
    --port)
      [ $# -ge 2 ] || { echo "--port 后面要给端口号" >&2; exit 1; }
      PANEL_PORT="$2"; shift 2 ;;
    --port=*) PANEL_PORT="${1#--port=}"; shift ;;
    panel|daemon) MODE="$1"; shift ;;
    -*) echo "未知选项: $1" >&2; exit 1 ;;
    *) INSTALL_DIR="$1"; shift ;;
  esac
done
INSTALL_DIR="${INSTALL_DIR:-/opt/mcpanel}"

if [ "$MODE" != "panel" ] && [ "$MODE" != "daemon" ]; then
  echo "用法: $0 <panel|daemon> [安装目录] [--no-service] [--port 端口]" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "请以 root 运行（需要写入 /etc/systemd/system）" >&2
  exit 1
fi

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="atl-$MODE"
UNIT="atlmcpanel-$MODE"

echo "==> 安装模式: $MODE"
echo "==> 安装目录: $INSTALL_DIR"
echo "==> 来源目录: $SRC_DIR"

# ---------------------------------------------------------------------------
# 0a. 面板 HTTP 端口：默认**随机高位端口**，不再默认 8080
# ---------------------------------------------------------------------------
#
# 为什么改（2026-10-12，用户提出「8080 极易被运营商风控和遭遇扫描爆破」）：
#
#   实测这台节点 6.5 天的 journal 里有 33,664 次 SSH 失败登录 + 27,654 次
#   Invalid user，约 4,800 次/天 —— 公网机器**确实在被持续扫**。而 8080 是
#   扫描器与爆破工具的默认目标之一，在国内也属于运营商重点监测的端口区
#   （80/443/8080 等）；家宽更是普遍封 80/443 入站。
#
# 但要说清楚：**换端口只是降低被扫到的概率，不是安全边界**。真正的门锁是
# 认证与授权 —— 同一次体检里发现的那条"匿名请求即可抢注首个管理员"
# （POST /api/users）无论端口是几都成立，已单独修掉。
#
# 候选区选 20000-25499，三个边界都有理由：
#   · > 1024   不需要特权端口；
#   · < 32768  **避开 Linux 默认临时端口区**（32768-60999）—— 在那里监听会与
#              出站连接抢端口，症状是"偶发启动失败"；
#   · < 25565  避开 Minecraft 服务端端口与 frp 公网端口的默认起点
#              （25565 起、默认预留 100 个，见 frps_servers.port_start/port_end），
#              否则新建实例时可能和面板自己撞端口。
pick_panel_port() {
  local p tries=0
  while [ "$tries" -lt 40 ]; do
    # $RANDOM 是 bash 内建；本脚本 shebang 就是 bash。
    p=$(( 20000 + RANDOM % 5500 ))
    if ! ss -tln 2>/dev/null | grep -qE "[:.]${p}[[:space:]]"; then
      echo "$p"
      return 0
    fi
    tries=$((tries + 1))
  done
  return 1
}

PANEL_PORT_FROM=""
if [ "$MODE" = "panel" ]; then
  # 已有配置里正在用的端口（可能为空 = 还没装过）
  EXISTING_PORT=""
  if [ -f "$INSTALL_DIR/config.yaml" ]; then
    EXISTING_PORT=$(grep -E '^[[:space:]]*listen:' "$INSTALL_DIR/config.yaml" 2>/dev/null \
                    | head -1 | grep -oE '[0-9]+' | head -1 || true)
  fi

  if [ -n "$PANEL_PORT" ]; then
    case "$PANEL_PORT" in
      *[!0-9]*) echo "错误：--port 必须是数字，收到 $PANEL_PORT" >&2; exit 1 ;;
    esac
    if [ "$PANEL_PORT" -lt 1024 ] || [ "$PANEL_PORT" -gt 65535 ]; then
      echo "错误：--port 需在 1024-65535 之间，收到 $PANEL_PORT" >&2
      exit 1
    fi
    PANEL_PORT_FROM="命令行 --port 指定"
    # 已有配置且端口不同：**必须明说不会自动改**。
    # 否则用户看到脚本说"面板端口: 22000"，而实际监听还是 21000 —— 这种
    # "报告的和生效的不一致"正是最难查的一类问题。
    if [ -n "$EXISTING_PORT" ] && [ "$EXISTING_PORT" != "$PANEL_PORT" ]; then
      echo
      echo "    ⚠️ 已有配置里的端口是 $EXISTING_PORT，本脚本**不会**改它"
      echo "       （不擅自改一个已经在跑的部署：端口一变，面板打不开，防火墙/反代/隧道全要跟着改）。"
      echo "       要让 --port 生效，请手动改 $INSTALL_DIR/config.yaml 的 server.listen 为 $PANEL_PORT，然后："
      echo "         systemctl restart atlmcpanel-panel"
      echo
    fi
  elif [ -n "$EXISTING_PORT" ]; then
    # 已有配置 → **沿用其中的端口，绝不擅自改一个已经在跑的部署**。
    PANEL_PORT="$EXISTING_PORT"
    PANEL_PORT_FROM="沿用已有配置"
  fi

  if [ -z "$PANEL_PORT" ]; then
    if ! PANEL_PORT=$(pick_panel_port); then
      echo "错误：连续 40 次都没找到空闲端口，请用 --port 手动指定" >&2
      exit 1
    fi
    PANEL_PORT_FROM="随机选取（可用 --port 指定）"
  fi
  echo "==> 面板端口: $PANEL_PORT（$PANEL_PORT_FROM）"
  if [ "$PANEL_PORT" = "8080" ]; then
    echo "    ⚠️ 8080 是扫描器与爆破工具的默认目标，也是运营商重点监测的端口。"
    echo "       若不是刻意为之，建议改用其他端口（去掉 --port 重新执行即可）。"
  fi
fi

# ---------------------------------------------------------------------------
# 0. 先验证二进制能在这台机器上跑
#
# 为什么把这一步放在最前面：最常见的首次失败是"下错了架构的包"
# （x86 机器装了 arm64 包），症状是 `Exec format error`，
# 而那个错误埋在 systemctl 日志里很不起眼，用户很容易误判成"程序有 bug"。
# 这里直接跑一次 -version，一秒就能给出明确结论。
# ---------------------------------------------------------------------------
if [ ! -f "$SRC_DIR/bin/$BIN_NAME" ]; then
  echo "错误：包里缺少 bin/$BIN_NAME" >&2
  exit 1
fi

echo "==> 校验二进制可执行"
ARCH_MACHINE=$(uname -m)
case "$ARCH_MACHINE" in
  x86_64) MACHINE_GOARCH="amd64" ;;
  aarch64|arm64) MACHINE_GOARCH="arm64" ;;
  armv7l) MACHINE_GOARCH="arm" ;;
  *) MACHINE_GOARCH="$ARCH_MACHINE" ;;
esac

PKG_ARCH="unknown"
if [ -f "$SRC_DIR/VERSION" ]; then
  PKG_ARCH=$(grep '^platform:' "$SRC_DIR/VERSION" 2>/dev/null | awk '{print $2}' || true)
fi
echo "    本机架构: $ARCH_MACHINE（$MACHINE_GOARCH）"
echo "    包架构  : ${PKG_ARCH:-未标注}"

if ! RUN_OUT=$("$SRC_DIR/bin/$BIN_NAME" -version 2>&1); then
  echo >&2
  echo "❌ 二进制无法在这台机器上运行：" >&2
  echo "    $RUN_OUT" >&2
  echo >&2
  case "$RUN_OUT" in
    *"Exec format error"*|*"cannot execute binary file"*)
      echo "    这是**架构不匹配**：你装的包不是这台机器的架构。" >&2
      echo "    本机是 $ARCH_MACHINE，请改用 linux-$MACHINE_GOARCH 的包。" >&2
      ;;
    *)
      echo "    请把上面的输出发出来定位。" >&2
      ;;
  esac
  exit 1
fi
echo "    ✅ $RUN_OUT"

if [ -n "$PKG_ARCH" ] && [ "$PKG_ARCH" != "unknown" ] && [ "${PKG_ARCH##*/}" != "$MACHINE_GOARCH" ]; then
  echo "    ⚠️ 包标注的架构与本机不一致（可能通过模拟器运行）" >&2
fi

# ---------------------------------------------------------------------------
# 1. 目录结构
# ---------------------------------------------------------------------------
echo "==> 创建目录结构"
mkdir -p "$INSTALL_DIR"/{bin,data,instances,certs,logs}

# 安装根目录用 0711（属主 rwx、其他用户只能穿过、不能列目录）。
#
# 为什么必须是"能穿过"：节点上每个实例都以**专用系统用户**运行
#（见 daemon.instance_user），实例进程要能读到自己的实例目录与共享
# resources/ 下的 jar —— 少了 o+x，实例会以完全不相干的报错启动失败。
# 为什么不能是 0755：这样同机器上的其它用户就能列目录，
# 顺带看到 config.yaml、state/、frp/ 的名字（内容仍是 0600/0700，读不到）。
chmod 711 "$INSTALL_DIR" 2>/dev/null || true

# data/ 里放的是数据库、PKI、JWT 密钥、备份 —— 数据里有节点 SSH 凭据与 frps token，
# 不该让同机器上的其它用户读到。程序自己也会收紧 SQLite 文件（见 db.restrictSQLiteFilePerms），
# 这里把目录本身也收一下。
chmod 700 "$INSTALL_DIR/data" 2>/dev/null || true
# 已存在的数据库一起收紧（升级场景：老版本建出来的是 644）
chmod 600 "$INSTALL_DIR"/data/*.db "$INSTALL_DIR"/data/*.db-wal "$INSTALL_DIR"/data/*.db-shm 2>/dev/null || true

# mTLS 的 CA 私钥/节点私钥：这几个文件一旦被实例用户读到，
# 它就能冒充节点连面板 gRPC（也就是从"能管一台实例"升级到"操控节点通道"）。
chmod 700 "$INSTALL_DIR/certs" 2>/dev/null || true
chmod 600 "$INSTALL_DIR"/certs/*.key 2>/dev/null || true

# 实例根目录：必须让实例用户能**穿过**（进自己那层），所以是 0711 而不是 0700；
# 但不能是 0755 —— 那样同机器上别的实例用户就能列出"这台机器上有哪些实例"。
chmod 711 "$INSTALL_DIR/instances" 2>/dev/null || true
# 平台状态与 frp 工作目录：只有 root 能进（里面是 root 会去读的文件）
chmod 700 "$INSTALL_DIR/state" "$INSTALL_DIR/frp" 2>/dev/null || true
# 共享资源（jar）：只读放开给实例用户
chmod 755 "$INSTALL_DIR/resources" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 2. 二进制
# ---------------------------------------------------------------------------
if [ "$MODE" = "panel" ]; then
  BINARIES=("atl-panel")
else
  BINARIES=("atl-daemon")
fi

for b in "${BINARIES[@]}"; do
  if [ ! -f "$SRC_DIR/bin/$b" ]; then
    echo "缺少二进制 bin/$b" >&2
    exit 1
  fi
  install -m 0755 "$SRC_DIR/bin/$b" "$INSTALL_DIR/bin/$b"
  echo "    已安装 $b"
done

# 面板包同时提供 daemon 二进制：面板的"一键部署节点"要下发与自身**同版本**的 daemon，
# 版本不一致会导致节点注册后立刻被判为不兼容。
if [ "$MODE" = "panel" ] && [ -f "$SRC_DIR/bin/atl-daemon" ]; then
  install -m 0755 "$SRC_DIR/bin/atl-daemon" "$INSTALL_DIR/bin/atl-daemon"
  echo "    已安装 atl-daemon（供一键部署下发）"
fi

# 面板包同样带一份 frpc：一键部署会把"面板二进制旁边"的 frpc 一并下发到节点，
# 这样用户不用自己去 GitHub 找 frp（见 internal/panel/httpapi/nodes.go）。
if [ "$MODE" = "panel" ] && [ -f "$SRC_DIR/bin/frpc" ]; then
  install -m 0755 "$SRC_DIR/bin/frpc" "$INSTALL_DIR/bin/frpc"
  echo "    已安装 frpc（供一键部署下发，穿透用）"
fi

# ---------------------------------------------------------------------------
# 2b. 旧名兼容软链接（**过渡用，下个版本删**）
#
# 二进制在 2026-10-12 由 dsh-panel / dsh-daemon 改名为 atl-panel / atl-daemon。
# 原因：前缀 "dsh" 是**开发环境**的名字（DSH Desktop），不是产品名 —— 而项目里
# 所有对外的标识符（systemd 单元 atlmcpanel-*、证书 CN、cgroup 根、安装目录、
# 发布包名）一直是 ATL-MCPanel。那个前缀还造成过实际损害：docs/TODO-2026-09-12.md
# 记着 `pkill -f dsh-panel` 这类模式**两次误杀过生产面板**。
#
# 留一个旧名软链接，是为了不让这次改名波及已经存在的东西：
#   · 老升级（deploy/upgrade.sh 从不重写单元）留下的 ExecStart 仍写着旧名；
#   · 运维者的脚本 / 别名 / 上一版发布说明里写的就是 `dsh-panel user create`。
#
# 用 ln -sfn 会**覆盖同名的旧真实文件**，所以不会出现"两份二进制、不知道哪份在跑"。
# ---------------------------------------------------------------------------
if [ -f "$INSTALL_DIR/bin/atl-panel" ]; then
  ln -sfn atl-panel "$INSTALL_DIR/bin/dsh-panel"
  echo "    已创建兼容软链接 dsh-panel → atl-panel"
fi
if [ -f "$INSTALL_DIR/bin/atl-daemon" ]; then
  ln -sfn atl-daemon "$INSTALL_DIR/bin/dsh-daemon"
  echo "    已创建兼容软链接 dsh-daemon → atl-daemon"
fi

# ---------------------------------------------------------------------------
# 2c. 节点：安装穿透客户端 frpc（随节点包分发）
# ---------------------------------------------------------------------------
#
# 为什么要装到 /usr/local/bin：Daemon 是用 `exec.LookPath("frpc")` 找它的
#（见 internal/daemon/grpcapi/frpstest.go），只看 PATH；而 systemd 单元的默认
# PATH 里有 /usr/local/bin。所以除了放在安装目录，再放一份到 /usr/local/bin，
# 否则"包里有 frpc、Daemon 却说找不到"—— 这种不一致很难排查。
if [ "$MODE" = "daemon" ] && [ -f "$SRC_DIR/bin/frpc" ]; then
  install -m 0755 "$SRC_DIR/bin/frpc" "$INSTALL_DIR/bin/frpc"
  install -m 0755 "$SRC_DIR/bin/frpc" /usr/local/bin/frpc
  echo "    已安装 frpc（穿透客户端）：$(frpc --version 2>/dev/null | head -1 || echo '版本未知')"
fi

# ---------------------------------------------------------------------------
# 3. 前端（仅面板）
# ---------------------------------------------------------------------------
if [ "$MODE" = "panel" ]; then
  if [ -d "$SRC_DIR/web-dist" ]; then
    rm -rf "$INSTALL_DIR/web/dist"
    mkdir -p "$INSTALL_DIR/web/dist"
    cp -r "$SRC_DIR/web-dist/." "$INSTALL_DIR/web/dist/"
    echo "    已安装前端静态资源"
  elif [ -d "$INSTALL_DIR/web/dist" ]; then
    echo "    包内无 web-dist，保留现有前端"
  else
    echo "警告：包内没有 web-dist，且安装目录也没有前端 —— 面板将只有 API，没有界面" >&2
  fi
fi

# ---------------------------------------------------------------------------
# 4. 配置文件
#
# ⚠️ 2026-10-12 修「同机部署必然失败」（用户报）：
#
# 旧逻辑是"配置文件已存在就整个保留不动"。而同机部署要先后装两次：
#   1) 先装 daemon → 生成只含 `daemon:` 段的配置
#      （裁掉 server 段是对的 —— 节点不需要面板配置）
#   2) 再装 panel  → 见配置文件已存在 → 保留不动 → 面板拿不到 `server:` 段
#      → server 全走零值 → grpc_listen 落到默认 ":9090"（全网卡）、grpc_mtls 默认 false
#      → 被安全自检拒绝启动："拒绝以明文 gRPC 对外提供服务"
# 反过来的顺序（先 panel 后 daemon）同样会失败。
#
# 现在改成：**按需补齐自己那一段**（已有则原样保留，绝不覆盖用户改过的值）。
# ---------------------------------------------------------------------------
CFG="$INSTALL_DIR/config.yaml"

# 配置里是否已有某个顶层段（顶格 `key:`）
has_section() {
  [ -f "${1:-}" ] && grep -qE "^${2}:" "$1" 2>/dev/null
}

# 判定「面板与节点是否同机」。
#
# ⚠️ 不能用 daemon.panel_address 单独当判据 —— 安装脚本会把它改成占位符
#     "PANEL_IP:9090"（原设计"逼用户改一次"），于是同机部署时那个值也不是回环，
#    判定会一律落到"跨机"。（实测踩到）
#
# ⚠️ 也不能只看"本机有没有对端" —— 用户可能先同机装了、后来把节点挪到别的机器
#    （把 panel_address 改成外部 IP），这时必须按跨机处理、开 mTLS。
#
# 所以用**优先级**判定，先看用户的显式声明：
#   1) panel_address 是**明确的外部地址** → 跨机（用户说了要跨机，最高优先级）
#   2) panel_address 是回环 / 空 / 未修改的占位符 → 同机
#   3) 地址读不出来（没有该行）→ 看本机有没有对端段
#
# 跨机必须开 mTLS：明文 gRPC 下任何能连上 9090 的主机都能注册成节点，
# 而面板会拿该节点的 SSH 凭据去部署 —— 等于把部署通道交出去。
is_same_machine() {
  local addr
  addr=$(grep -E '^[[:space:]]+panel_address:' "$CFG" 2>/dev/null | head -1 \
         | sed 's/.*panel_address:[[:space:]]*//' \
         | sed 's/#.*//' \
         | tr -d '"'"'"' ' \
         | tr -d '\r')

  case "$addr" in
    # 1) 明确的外部地址 → 跨机（用户显式声明，优先于其它判据）
    10.*|192.168.*|172.*|*.local:*) return 1 ;;
    # 2) 回环 / 空 / 未修改的占位符 → 同机
    ""|127.0.0.1:*|localhost:*|127.0.0.1|localhost|PANEL_IP:*|PANEL_IP) return 0 ;;
    # 其它（域名、公网 IP 等）→ 也是最可能是跨机
    *) return 1 ;;
  esac
}

if [ -f "$CFG" ]; then
  echo "==> 配置已存在: $CFG"

  # 按需补齐：面板缺 server 段 / 节点缺 daemon 段时追加，
  # 而不是像以前那样直接跳过（那正是同机部署失败的根因）。
  if [ "$MODE" = "panel" ] && ! has_section "$CFG" server; then
    echo "    缺少 server 段 → 追加"
    if is_same_machine; then
      echo "    判定为【同机部署】：gRPC 绑回环、不启用 mTLS（明文不出本机）"
      cat >> "$CFG" <<EOF

# ---- 面板 HTTP / gRPC（安装脚本自动追加）----
# 本机同时跑着节点（daemon.panel_address 是回环）⇒ 面板的 gRPC 只需监听回环。
# 若以后把节点挪到别的机器：把 grpc_listen 改成内网地址，
# 并把 grpc_mtls 改为 true、按 docs/MTLS.md 给节点签发证书。
server:
  listen: ":$PANEL_PORT"
  grpc_listen: "127.0.0.1:9090"
  external_url: "http://127.0.0.1:$PANEL_PORT"
  web_dir: "web/dist"
  grpc_mtls: false
  pki_dir: "data/pki"
  panel_frp_dir: "data/panel-frp"
auth:
  jwt_secret: ""
EOF
    else
      echo "    判定为【跨机部署】：gRPC 绑所有网卡、**强制启用 mTLS**"
      cat >> "$CFG" <<EOF

# ---- 面板 HTTP / gRPC（安装脚本自动追加）----
# 节点在**别的机器**上 ⇒ gRPC 必须监听可被节点访问的地址，且**必须开启 mTLS**：
# 明文 gRPC 等于把"注册节点"的能力开放给任何能连上该端口的主机。
server:
  listen: ":$PANEL_PORT"
  grpc_listen: ":9090"
  external_url: "http://127.0.0.1:$PANEL_PORT"
  web_dir: "web/dist"
  grpc_mtls: true
  pki_dir: "data/pki"
  panel_frp_dir: "data/panel-frp"
auth:
  jwt_secret: ""
EOF
    fi
    chmod 600 "$CFG"
  elif [ "$MODE" = "daemon" ] && ! has_section "$CFG" daemon; then
    echo "    缺少 daemon 段 → 追加"
    HN=$(hostname 2>/dev/null | tr -cd 'a-zA-Z0-9-' | cut -c1-32 || echo node)
    [ -z "$HN" ] && HN="node"
    cat >> "$CFG" <<EOF

# ---- 节点守护进程（安装脚本自动追加）----
daemon:
  node_id: "$HN"
  # 面板地址。同机部署可保持回环；节点在别的机器时改成面板的内网地址。
  panel_address: "127.0.0.1:9090"
  # Daemon 自身 gRPC 监听。默认只绑回环：这是内部通道，
  # 监听所有网卡等于多开一个攻击面。面板与节点不同机时改成内网地址。
  grpc_listen: "127.0.0.1:9091"
  instance_dir: "instances"
  # 实例运行身份：per-instance = 每个实例一个专用系统用户（隔离最强，默认）
  instance_user: "per-instance"
  instance_user_prefix: "atl-i-"
  state_dir: ""
  frp_state_dir: ""
  cgroup_enabled: true
  cgroup_root: ""
  backup_root: ""
  # 节点 ↔ 面板 mTLS。同机（回环）时明文不出机器，可保持 false；
  # **跨机部署必须改为 true**，并按 docs/MTLS.md 配好证书。
  tls: false
  cert_file: ""
  key_file: ""
  ca_file: ""
  allow_insecure_grpc: false
EOF
    chmod 600 "$CFG"
  else
    echo "    本模式所需的段已存在，保留不动"
  fi

  # 修掉"明文 gRPC 绑全网卡"这个会被拒绝启动的危险组合（同机场景）。
  # 只有同机才自动改成回环 —— 跨机时绑回环会让远程节点连不上，那必须靠 mTLS 解决。
  if [ "$MODE" = "panel" ] && has_section "$CFG" server && is_same_machine; then
    if grep -qE '^[[:space:]]+grpc_listen:[[:space:]]*":9090"' "$CFG" 2>/dev/null && \
       ! grep -qE '^[[:space:]]+grpc_mtls:[[:space:]]*true' "$CFG" 2>/dev/null; then
      echo "    ⚠️ 检测到「明文 gRPC + 监听所有网卡」（会被安全自检拒绝启动）"
      echo "       同机部署 → 已把 grpc_listen 改为 127.0.0.1:9090"
      sed -i 's|^\([[:space:]]*grpc_listen:\)[[:space:]]*":9090".*|\1 "127.0.0.1:9090"|' "$CFG"
    fi
  fi
elif [ -f "$SRC_DIR/config.example.yaml" ]; then
    # 直接用带注释的完整示例：用户拿到手就看得懂每个默认值的含义，
    # 比脚本拼一个"最小配置"友好得多。
    cp "$SRC_DIR/config.example.yaml" "$CFG"

    if [ "$MODE" = "panel" ]; then
      # 面板：指向随包安装的前端；jwt_secret 留空让程序自动生成并持久化
      sed -i 's|^\(  web_dir:\).*|\1 "web/dist"|' "$CFG" 2>/dev/null || true
      sed -i 's|^\(  jwt_secret:\).*|\1 ""|' "$CFG" 2>/dev/null || true
      # 端口：把示例里的 8080 换成本次选定的端口。
      # 示例文件里 external_url 也是 127.0.0.1:8080，一起换掉否则两个值不一致。
      sed -i "s|^\(  listen:\).*|\1 \":$PANEL_PORT\"|" "$CFG" 2>/dev/null || true
      sed -i "s|^\(  external_url:\).*|\1 \"http://127.0.0.1:$PANEL_PORT\"|" "$CFG" 2>/dev/null || true
    else
      # 节点：只保留 daemon 段。
      #
      # ⚠️ 不要用 `sed '/^server:/,/^$/d'` 这种"区间删除"：区间**在第一个空行就结束**，
      #    而 server 段内部就有空行（HTTPS 与 mTLS 之间）→ 只删掉表头几行，
      #    剩下的 tls_cert / grpc_mtls / pki_dir 会留在节点配置里。
      #    实测就是这么漏的：节点配置文件里混着一堆面板才有的设置，用户看了会以为要填。
      #    正确做法是**只抽取 daemon 段**：从 `^daemon:` 到下一个顶层键（顶格且非 daemon）。
      awk '/^daemon:/{keep=1}
           keep && /^[A-Za-z_]/ && !/^daemon:/{keep=0}
           keep' "$SRC_DIR/config.example.yaml" > "$CFG"
      if [ ! -s "$CFG" ]; then
        echo "警告：从示例里没抽到 daemon 段，改用最小配置" >&2
        cp "$SRC_DIR/config.example.yaml" "$CFG"
      fi
      # node_id 默认带主机名，多节点时不用手工区分
      HN=$(hostname 2>/dev/null | tr -cd 'a-zA-Z0-9-' | cut -c1-32 || echo node)
      [ -z "$HN" ] && HN="node"
      sed -i "s|^\(  node_id:\).*|\1 \"$HN\"|" "$CFG" 2>/dev/null || true
      # panel_address：**只有真跨机才换成占位符**。
      #
      # 原实现无条件写成 "PANEL_IP:9090"（注释说"逼用户改一次"）——
      # 那对"节点装在别的机器"是对的，但对**同机部署**是错的：
      # 同机时面板就在本机，写占位符反而让用户以为要改，也让后续的
      # "同机/跨机"判定永远看不到回环地址（实测踩到）。
      #
      # 判定方式：本机是否已经在跑面板。
      #   · 有面板（同机部署，常见顺序：先装 panel）→ 保持示例里的回环地址
      #   · 没有面板（独立节点）→ 换成占位符，逼用户填面板地址
      if ! has_section "$CFG" server && [ ! -f "$INSTALL_DIR/bin/atl-panel" ]; then
        sed -i 's|^\(  panel_address:\).*|\1 "PANEL_IP:9090"   # ← 改成面板可达的地址|' "$CFG" 2>/dev/null || true
        echo "    未检测到本机面板 → panel_address 设为占位符，安装后请改成面板地址"
      else
        echo "    检测到本机有面板（同机部署）→ panel_address 保持回环 127.0.0.1:9090"
      fi
    fi
    echo "    已从 config.example.yaml 生成（已按 $MODE 调整）"
  else
    # 兜底：包里没有示例时自己写一份最小配置
    if [ "$MODE" = "panel" ]; then
      cat > "$CFG" <<EOF
server:
  listen: ":$PANEL_PORT"
  grpc_listen: ":9090"
  external_url: "http://127.0.0.1:$PANEL_PORT"
  web_dir: "web/dist"
db:
  driver: sqlite3
  dsn: data/mcpanel.db
auth:
  jwt_secret: ""
EOF
    else
      cat > "$CFG" <<EOF
daemon:
  node_id: "$(hostname 2>/dev/null || echo node-001)"
  panel_address: "PANEL_IP:9090"
  # 只监听回环：Daemon 的 gRPC 是"面板反向调用节点"的通道，
  # 监听所有网卡（":9091"）会让它暴露在公网上 —— 虽然要求 mTLS，
  # 但没有理由把一个内部通道摆到外面。面板与节点不同机时改成内网地址。
  grpc_listen: "127.0.0.1:9091"
  instance_dir: "instances"
  # 实例的运行身份。per-instance = 每个实例一个专用系统用户（默认，隔离最强）。
  # 详见 internal/daemon/runas 与 docs 的安全说明：实例**不允许**以 root 运行。
  instance_user: "per-instance"
  tls: false
EOF
    fi
    echo "    已生成最小配置（包内没有 config.example.yaml）"
  fi
  chmod 600 "$CFG"

# 兜底检查：本模式所需的段必须存在。缺了就直接失败并说清原因 ——
# 否则面板会带着"零值 server 段"启动，然后被安全自检以
# "拒绝以明文 gRPC 对外提供服务"拒绝，那个报错对用户很难懂。
if [ "$MODE" = "panel" ] && ! has_section "$CFG" server; then
  echo "错误：配置里没有 server 段，且无法从包内示例生成（$CFG）" >&2
  exit 1
fi
if [ "$MODE" = "daemon" ] && ! has_section "$CFG" daemon; then
  echo "错误：配置里没有 daemon 段，且无法从包内示例生成（$CFG）" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 5. 面板：自签证书
# ---------------------------------------------------------------------------
if [ "$MODE" = "panel" ] && [ ! -f "$INSTALL_DIR/certs/panel.crt" ]; then
  if command -v openssl >/dev/null 2>&1; then
    GEN="$SRC_DIR/deploy/gen-cert.sh"
    [ -f "$GEN" ] || GEN="$SRC_DIR/scripts/gen-cert.sh"
    if [ -f "$GEN" ]; then
      echo "==> 生成自签 HTTPS 证书（浏览器会提示不安全，公网使用请换正式证书）"
      if bash "$GEN" "$INSTALL_DIR/certs" >/dev/null 2>&1; then
        echo "    已生成 $INSTALL_DIR/certs/panel.crt"
      else
        echo "    证书生成失败，可稍后手动执行 deploy/gen-cert.sh" >&2
      fi
    fi
  else
    echo "提示：未安装 openssl，跳过自签证书（面板仍可用 HTTP 访问）"
  fi
fi

# ---------------------------------------------------------------------------
# 6. 节点：导入「实例运行时基础镜像」（容器化隔离用）
# ---------------------------------------------------------------------------
#
# 镜像随**节点包**分发，不依赖节点能访问 Docker Hub（实测内网与部分机房都拉不动）。
# 这里只在两件事都成立时才导入：① 节点上有 docker；② 包里带了镜像 tar。
# 两者缺一都不算错误 —— 容器化是可选能力，没有它实例仍以 native 方式运行，
# 只是面板上的容器化开关会被拒绝（并给出原因）。
if [ "$MODE" = "daemon" ]; then
  IMG_TAR=$(ls -1 "$SRC_DIR"/runtime/atl-mcpanel-runtime-*.tar.gz 2>/dev/null | head -1)

  # 先看 docker 在不在。**安装脚本不替用户装 docker**：
  #   · 它要加第三方仓库、拉上百 MB 的包、还常与 container-selinux 版本打架；
  #     在别人的生产机上做这种事，失败了很难收拾，而且和"装面板"根本不是一件事；
  #   · 真出问题时，一条发行版对应的命令比脚本里的一段自动逻辑好排查得多。
  # 但**必须把话说清楚**：没有 docker 就没有容器化隔离，而面板上会有这个开关 ——
  # 所以这里逐发行版给出可直接复制的命令，而不是含糊地说"未安装 docker"。
  if ! command -v docker >/dev/null 2>&1; then
    echo "==> 未检测到 docker：容器化隔离不可用（实例将直接运行在节点上）"
    echo "    需要它的话，按发行版执行："
    echo "      CentOS 7 / RHEL 7:"
    echo "        yum install -y yum-utils"
    echo "        yum-config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo"
    echo "        yum install -y docker-ce docker-ce-cli containerd.io   # 注意：yum 3 不支持 --nobest"
    echo "        systemctl enable --now docker"
    echo "      Debian / Ubuntu:"
    echo "        apt-get update && apt-get install -y docker.io && systemctl enable --now docker"
    echo "    装完再执行一次本脚本即可自动导入下面这个镜像："
    echo "      ${IMG_TAR:-（本包内未附带运行时镜像）}"
  elif [ -z "$IMG_TAR" ]; then
    echo "==> 节点包内未包含运行时镜像（容器化隔离将不可用）"
  else
    echo "==> 导入实例运行时基础镜像"
    if docker load -i "$IMG_TAR" 2>&1 | tail -2 | sed 's/^/    /'; then
      # 同时打一个 latest 标签：daemon 默认按 atl-mcpanel-runtime:latest 找镜像，
      # 这样升级面板版本时不必同步改节点配置。
      IMG_NAME=$(docker images --format '{{.Repository}}:{{.Tag}}' \
        | grep '^atl-mcpanel-runtime:' | grep -v ':latest$' | head -1)
      [ -n "$IMG_NAME" ] && docker tag "$IMG_NAME" atl-mcpanel-runtime:latest
      echo "    当前镜像：$(docker images --format '{{.Repository}}:{{.Tag}}' | grep '^atl-mcpanel-runtime:' | tr '\n' ' ')"
    else
      echo "    ⚠️ 镜像导入失败（容器化隔离暂不可用，不影响 native 模式运行）" >&2
    fi
  fi

  # ---------------------------------------------------------------------------
  # 6b. 节点：容器防火墙规则（**容器化可用的前提**，不是可选加固）
  # ---------------------------------------------------------------------------
  #
  # 容器访问宿主上的服务走 INPUT；少了这条规则，实例就能连到宿主绑 0.0.0.0 的
  # 服务（实测能连上 Daemon 的 gRPC 端口），"容器隔离"就只剩一半。
  #
  # 为什么做成 systemd 单元而不是在这里直接 iptables -I：
  #   · 直接加只在**执行本脚本这一刻**生效，服务器一重启就没了 ——
  #     而现象是"装完是好的，跑一阵子（重启后）隔离悄悄失效"，几乎不可能被发现；
  #   · 规则必须落在 docker 之后（docker 会重建自己的链），单元里用 After=docker.service 表达。
  # 脚本本身是幂等的（先 -C 检查），重复安装不会堆叠规则。
  if command -v iptables >/dev/null 2>&1; then
    FW_SRC="$SRC_DIR/deploy/systemd/atl-container-firewall.service"
    if [ -f "$FW_SRC" ]; then
      echo "==> 安装容器隔离防火墙规则（容器不得访问宿主服务）"
      install -m644 "$FW_SRC" /etc/systemd/system/atl-container-firewall.service
      systemctl daemon-reload
      systemctl enable atl-container-firewall >/dev/null 2>&1 || true
      systemctl restart atl-container-firewall >/dev/null 2>&1 || true
      if systemctl is-active --quiet atl-container-firewall; then
        # 复核：直接把规则打出来，而不是只说"已生效" —— 一眼能看出到底有没有落上
        echo "    规则已生效："
        iptables -S INPUT 2>/dev/null | grep -E '\-i (br-\+|docker0)' | sed 's/^/      /'
      else
        echo "    ⚠️ 规则单元未启动，请手动检查：systemctl status atl-container-firewall" >&2
      fi
    fi
  else
    echo "提示：未找到 iptables，跳过容器隔离防火墙规则"
    echo "      装了 docker 与 iptables 之后再跑一次本脚本即可补上（否则容器能访问宿主服务）"
  fi
fi

# ---------------------------------------------------------------------------
# 7. systemd
# ---------------------------------------------------------------------------
if [ -n "$NO_SERVICE" ]; then
  echo "==> 已指定 --no-service，跳过 systemd 安装（文件已就绪）"
else
  UNIT_SRC="$SRC_DIR/deploy/systemd/${UNIT}.service"
  UNIT_DST="/etc/systemd/system/${UNIT}.service"
  if [ -f "$UNIT_SRC" ]; then
    echo "==> 安装 systemd 单元"
    sed "s|__INSTALL_DIR__|$INSTALL_DIR|g" "$UNIT_SRC" > "$UNIT_DST"
    chmod 644 "$UNIT_DST"
    systemctl daemon-reload
    systemctl enable "$UNIT" >/dev/null 2>&1 || true
    systemctl restart "$UNIT"
    sleep 3
    if systemctl is-active --quiet "$UNIT"; then
      echo "    $UNIT 已启动"
      # 节点：如果面板地址还是占位符，明确说清楚 —— 否则用户看到"已启动"会以为通了，
      # 实际节点只是在等面板可达（Daemon 会退避重试，不会再变成 failed 循环）。
      if [ "$MODE" = "daemon" ] && grep -q 'PANEL_IP' "$CFG" 2>/dev/null; then
        echo
        echo "    ⚠️ 面板地址还是占位符，节点暂时连不上面板（这是正常的，它在等待）。"
        echo "       改 $CFG 里的 daemon.panel_address 为面板可达地址，然后："
        echo "         systemctl restart $UNIT"
      fi
    else
      echo "    $UNIT 启动失败，日志如下：" >&2
      journalctl -u "$UNIT" --no-pager -n 20 >&2 || true
      echo >&2
      echo "    常见原因：" >&2
      echo "      - 配置写错（daemon.node_id / panel_address）" >&2
      echo "      - 端口被占用（daemon.grpc_listen，默认 :9091）" >&2
      exit 1
    fi
  else
    echo "警告：未找到 systemd 单元模板 $UNIT_SRC" >&2
  fi
fi

# ---------------------------------------------------------------------------
# 7b. 环境预检提示（把"配了但不生效"的坑提前讲出来）
# ---------------------------------------------------------------------------
if [ "$MODE" = "daemon" ]; then
  CGROUP_TYPE=$(stat -fc %T /sys/fs/cgroup 2>/dev/null || echo unknown)
  KERNEL=$(uname -r)
  if [ "$CGROUP_TYPE" = "cgroup2fs" ]; then
    echo
    echo "ℹ️ cgroup 是 v2 —— 实例的 CPU/内存**硬配额可用**。"
  else
    # v2 不可用**不等于**配额不可用：daemon 也支持 cgroup v1（CentOS 7 这类老内核就是 v1）。
    # 早先这里直接警告"配额不会生效"，在加了 v1 支持之后就成了误导。
    # 这里按实际挂载的控制器判断，而不是只看是不是 v2。
    V1_CPU=""
    V1_MEM=""
    for p in /sys/fs/cgroup/cpu,cpuacct /sys/fs/cgroup/cpu; do
      [ -e "$p/cpu.cfs_quota_us" ] && V1_CPU="$p"
    done
    [ -e /sys/fs/cgroup/memory/memory.limit_in_bytes ] && V1_MEM="memory"
    if [ -n "$V1_CPU" ]; then
      echo
      echo "ℹ️ cgroup 是 v1（当前: $CGROUP_TYPE，内核 $KERNEL），daemon 会用 v1 施加限额："
      echo "   CPU 配额: 可用（$V1_CPU）"
      if [ -n "$V1_MEM" ]; then
        echo "   内存上限: 可用（/sys/fs/cgroup/memory）"
      else
        echo "   内存上限: **不可用**（未挂载 memory 控制器）—— 只有 CPU 配额生效"
      fi
    else
      echo
      echo "⚠️ 未检测到可用的 cgroup（既不是 v2，也没有 v1 的 cpu 控制器）。"
      echo "   实例的 CPU/内存**硬配额不会生效**，会降级为不限制并记录警告。"
      echo "   需要 cgroup v2（内核 >= 4.15 且统一层级）或 cgroup v1（挂载 cpu 与 memory）。"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# 8. 完成提示
# ---------------------------------------------------------------------------
echo
echo "==============================================="
echo " 安装完成"
echo "==============================================="
if [ "$MODE" = "panel" ]; then
  # 探测一个可用于访问的地址，省得用户自己查 IP
  IP=$(hostname -I 2>/dev/null | awk '{print $1}')
  [ -z "$IP" ] && IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{print $7; exit}')
  [ -z "$IP" ] && IP="<本机IP>"
  # 从**配置文件**里读端口而不是用 $PANEL_PORT：万一用户手改过配置，
  # 打印出来的必须是实际监听的那个端口，否则"地址打不开"从第一眼就开始误导。
  PORT=$(grep -E '^[[:space:]]*listen:' "$CFG" 2>/dev/null | head -1 | grep -oE '[0-9]+' | head -1 || true)
  [ -z "$PORT" ] && PORT="$PANEL_PORT"
  [ -z "$PORT" ] && PORT=8080

  # ---------------------------------------------------------------------------
  # 首个管理员：**只能**由本机 CLI 创建
  #
  # 面板已不提供任何免登录的注册入口（2026-10-12 安全修复）：此前 users 表为空时
  # 第一个匿名请求者会被自动建成 admin，而前端从未调用过那个接口 —— 等于给任何
  # 能访问面板端口的人留了一条隐藏的夺权路径。
  #
  # -if-empty 保证安装脚本**幂等**：重复执行绝不重置已有账号的口令。
  # ---------------------------------------------------------------------------
  echo
  echo "==> 创建首个管理员账号（面板不提供免登录注册入口，只能在本机创建）"
  if [ -x "$INSTALL_DIR/bin/atl-panel" ]; then
    ADMIN_OUT=""
    if ADMIN_OUT=$(cd "$INSTALL_DIR" && ./bin/atl-panel -config "$CFG" user create \
                     -username admin -role admin -if-empty 2>&1); then
      echo "$ADMIN_OUT" | sed 's/^/    /'
    else
      echo "    ⚠️ 自动创建失败（面板仍可正常使用）。请手动执行：" >&2
      echo "       cd $INSTALL_DIR && ./bin/atl-panel -config $CFG user create -username admin" >&2
      echo "$ADMIN_OUT" | sed 's/^/       /' >&2
    fi
  else
    echo "    ⚠️ 找不到 $INSTALL_DIR/bin/atl-panel，跳过。请手动创建管理员账号。" >&2
  fi

  echo
  echo " 面板地址:   http://${IP}:${PORT}"
  echo " 配置文件:   $CFG"
  echo " 数据目录:   $INSTALL_DIR/data（含数据库、PKI、备份）"
  echo
  echo " 建议下一步:"
  echo "   1) 立刻用上面的账号登录，并到「账号」页改掉初始口令"
  echo "   2) 配置 HTTPS 证书（见 docs/CERTIFICATES.md）—— HTTP 下登录口令是明文传输的"
  echo "   3) 为节点签发证书并部署 Daemon（见 docs/MTLS.md、docs/DEPLOYMENT.md）"
  echo "      节点机上跑节点包的 deploy/install.sh daemon"
  echo
  echo " 忘记口令:   cd $INSTALL_DIR && ./bin/atl-panel -config $CFG user passwd -username admin"
else
  echo " 节点 ID:    见 $CFG 的 daemon.node_id"
  echo " 配置面板地址: 修改 daemon.panel_address 为面板可达地址"
  echo " 启用 mTLS:  从面板导出节点证书后设置 tls/cert_file/key_file/ca_file"
  echo
  echo " 提示: 若通过面板「节点管理 → 一键部署」安装，则上述配置会自动完成。"
fi
echo " 查看日志:   journalctl -u $UNIT -f"
echo "==============================================="
