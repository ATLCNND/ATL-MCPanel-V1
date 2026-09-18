#!/usr/bin/env bash
# ATL-MCPanel 安装脚本（面板包 / 节点包通用）
#
# 用法：
#   面板：sudo ./install.sh panel  [安装目录]
#   节点：sudo ./install.sh daemon [安装目录]
#   仅准备文件、不安装/启动服务：追加 --no-service
#
# 脚本假设当前目录（解压后的包目录）包含：
#   bin/  deploy/  config.example.yaml
#   面板包另有：web-dist/
#
# 做七件事：校验二进制 → 装二进制 → 建目录 → 写配置 → 装 systemd 单元
#           → 生成自签证书 → 启动并打印访问地址
#
# 幂等：可重复执行。**已存在的 config.yaml 与 data/ 一律保留**（升级请用 upgrade.sh）。
set -euo pipefail

MODE=""
INSTALL_DIR=""
NO_SERVICE=""
for arg in "$@"; do
  case "$arg" in
    --no-service) NO_SERVICE=1 ;;
    panel|daemon) MODE="$arg" ;;
    -*) echo "未知选项: $arg" >&2; exit 1 ;;
    *) INSTALL_DIR="$arg" ;;
  esac
done
INSTALL_DIR="${INSTALL_DIR:-/opt/mcpanel}"

if [ "$MODE" != "panel" ] && [ "$MODE" != "daemon" ]; then
  echo "用法: $0 <panel|daemon> [安装目录] [--no-service]" >&2
  exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
  echo "请以 root 运行（需要写入 /etc/systemd/system）" >&2
  exit 1
fi

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="dsh-$MODE"
UNIT="atlmcpanel-$MODE"

echo "==> 安装模式: $MODE"
echo "==> 安装目录: $INSTALL_DIR"
echo "==> 来源目录: $SRC_DIR"

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
  BINARIES=("dsh-panel")
else
  BINARIES=("dsh-daemon")
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
if [ "$MODE" = "panel" ] && [ -f "$SRC_DIR/bin/dsh-daemon" ]; then
  install -m 0755 "$SRC_DIR/bin/dsh-daemon" "$INSTALL_DIR/bin/dsh-daemon"
  echo "    已安装 dsh-daemon（供一键部署下发）"
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
# 4. 配置文件（已存在则保留）
# ---------------------------------------------------------------------------
CFG="$INSTALL_DIR/config.yaml"
if [ -f "$CFG" ]; then
  echo "==> 配置已存在，保留不动: $CFG"
else
  echo "==> 生成初始配置"
  if [ -f "$SRC_DIR/config.example.yaml" ]; then
    # 直接用带注释的完整示例：用户拿到手就看得懂每个默认值的含义，
    # 比脚本拼一个"最小配置"友好得多。
    cp "$SRC_DIR/config.example.yaml" "$CFG"

    if [ "$MODE" = "panel" ]; then
      # 面板：指向随包安装的前端；jwt_secret 留空让程序自动生成并持久化
      sed -i 's|^\(  web_dir:\).*|\1 "web/dist"|' "$CFG" 2>/dev/null || true
      sed -i 's|^\(  jwt_secret:\).*|\1 ""|' "$CFG" 2>/dev/null || true
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
      # panel_address 的示例值是本机回环，对**独立安装的节点**是错的（会误导成"不用改"）。
      # 换成显眼占位符，逼用户改一次。
      sed -i 's|^\(  panel_address:\).*|\1 "PANEL_IP:9090"   # ← 改成面板可达的地址|' "$CFG" 2>/dev/null || true
    fi
    echo "    已从 config.example.yaml 生成（已按 $MODE 调整）"
  else
    # 兜底：包里没有示例时自己写一份最小配置
    if [ "$MODE" = "panel" ]; then
      cat > "$CFG" <<'EOF'
server:
  listen: ":8080"
  grpc_listen: ":9090"
  external_url: "http://127.0.0.1:8080"
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
# 7. 环境预检提示（把"配了但不生效"的坑提前讲出来）
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
  PORT=$(grep -E '^[[:space:]]*listen:' "$CFG" 2>/dev/null | head -1 | grep -oE '[0-9]+' | head -1 || true)
  [ -z "$PORT" ] && PORT=8080

  echo " 面板地址:   http://${IP}:${PORT}"
  echo " 首次使用:   浏览器打开后注册第一个账号（自动成为管理员）"
  echo " 配置文件:   $CFG"
  echo " 数据目录:   $INSTALL_DIR/data（含数据库、PKI、备份）"
  echo
  echo " 建议下一步:"
  echo "   1) 配置 HTTPS 证书（见 docs/CERTIFICATES.md）"
  echo "   2) 为节点签发证书并部署 Daemon（见 docs/MTLS.md、docs/DEPLOYMENT.md）"
  echo "      节点机上跑节点包的 deploy/install.sh daemon"
else
  echo " 节点 ID:    见 $CFG 的 daemon.node_id"
  echo " 配置面板地址: 修改 daemon.panel_address 为面板可达地址"
  echo " 启用 mTLS:  从面板导出节点证书后设置 tls/cert_file/key_file/ca_file"
  echo
  echo " 提示: 若通过面板「节点管理 → 一键部署」安装，则上述配置会自动完成。"
fi
echo " 查看日志:   journalctl -u $UNIT -f"
echo "==============================================="
