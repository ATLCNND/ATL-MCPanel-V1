#!/usr/bin/env bash
# ATL-MCPanel 安装脚本
#
# 用法：
#   面板：sudo ./install.sh panel  [安装目录]
#   节点：sudo ./install.sh daemon [安装目录]
#   仅准备文件、不安装/启动服务：追加 --no-service
#
# 脚本假设当前目录（或压缩包解压目录）包含：
#   bin/dsh-panel  bin/dsh-daemon  web/dist/  config.example.yaml  deploy/
#
# 特性：
#   - 幂等：可重复执行以升级（会保留现有 config.yaml 与数据）
#   - 自动生成自签证书（面板）、systemd 单元、目录结构
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
echo "==> 安装模式: $MODE"
echo "==> 安装目录: $INSTALL_DIR"
echo "==> 来源目录: $SRC_DIR"

# ---------- 1. 目录结构 ----------
echo "==> 创建目录结构"
mkdir -p "$INSTALL_DIR"/{bin,data,instances,certs,logs}

# ---------- 2. 二进制 ----------
if [ "$MODE" = "panel" ]; then
  BINARIES=("dsh-panel")
else
  BINARIES=("dsh-daemon")
fi

for b in "${BINARIES[@]}"; do
  if [ ! -f "$SRC_DIR/bin/$b" ]; then
    echo "缺少二进制 bin/$b，请先执行 scripts/build.sh" >&2
    exit 1
  fi
  install -m 0755 "$SRC_DIR/bin/$b" "$INSTALL_DIR/bin/$b"
  echo "    已安装 $b"
done

# 部署节点时同时提供 daemon 二进制（面板一键部署需要与自身版本一致的 daemon）
if [ "$MODE" = "panel" ] && [ -f "$SRC_DIR/bin/dsh-daemon" ]; then
  install -m 0755 "$SRC_DIR/bin/dsh-daemon" "$INSTALL_DIR/bin/dsh-daemon"
  echo "    已安装 dsh-daemon（供一键部署下发）"
fi

# ---------- 3. 前端 ----------
if [ "$MODE" = "panel" ]; then
  if [ -d "$SRC_DIR/web/dist" ]; then
    rm -rf "$INSTALL_DIR/web/dist"
    mkdir -p "$INSTALL_DIR/web/dist"
    cp -r "$SRC_DIR/web/dist/." "$INSTALL_DIR/web/dist/"
    echo "    已安装前端静态资源"
  else
    echo "警告：未找到 web/dist，面板将不托管前端界面" >&2
  fi
fi

# ---------- 4. 配置文件（已存在则保留） ----------
CFG="$INSTALL_DIR/config.yaml"
if [ -f "$CFG" ]; then
  echo "==> 配置已存在，保留不动: $CFG"
else
  echo "==> 生成初始配置"
  if [ "$MODE" = "panel" ]; then
    cat > "$CFG" <<EOF
server:
  listen: ":8080"
  grpc_listen: ":9090"
  external_url: "http://127.0.0.1:8080"
  web_dir: "web/dist"
  # HTTPS（建议暴露公网前启用，证书可用自签或 CA 签发；见 docs/CERTIFICATES.md）
  tls_listen: ""
  tls_cert: ""
  tls_key: ""
  trust_proxy: false
  # Panel ↔ Daemon 双向认证（生产必须为 true；见 docs/MTLS.md）
  grpc_mtls: true
  pki_dir: "data/pki"
  panel_frp_dir: "data/panel-frp"

daemon:
  node_id: "node-001"
  panel_address: "127.0.0.1:9090"
  grpc_listen: ":9091"
  instance_dir: "instances"
  tls: false
  cert_file: ""
  key_file: ""
  ca_file: ""

db:
  driver: sqlite3
  dsn: data/mcpanel.db
  backup_enabled: true
  backup_keep: 7
  backup_interval: "24h"

auth:
  # 留空则自动生成随机密钥并持久化到 data/jwt.secret（推荐）
  jwt_secret: ""
EOF
  else
    cat > "$CFG" <<EOF
daemon:
  node_id: "node-001"
  panel_address: "PANEL_IP:9090"
  grpc_listen: ":9091"
  instance_dir: "instances"
  tls: false
  cert_file: ""
  key_file: ""
  ca_file: ""
EOF
  fi
  chmod 600 "$CFG"
  echo "    已生成 $CFG（请按需修改）"
fi

# ---------- 5. 面板：自签证书 ----------
if [ "$MODE" = "panel" ] && [ ! -f "$INSTALL_DIR/certs/panel.crt" ]; then
  if command -v openssl >/dev/null 2>&1 && [ -f "$SRC_DIR/scripts/gen-cert.sh" ]; then
    echo "==> 生成自签 HTTPS 证书（内网/测试用）"
    bash "$SRC_DIR/scripts/gen-cert.sh" "$INSTALL_DIR/certs" >/dev/null 2>&1 || \
      echo "    证书生成失败，可稍后手动执行 scripts/gen-cert.sh" >&2
  fi
fi

# ---------- 6. systemd ----------
if [ -n "$NO_SERVICE" ]; then
  echo "==> 已指定 --no-service，跳过 systemd 安装（文件已就绪）"
else
UNIT_SRC="$SRC_DIR/deploy/systemd/atlmcpanel-$MODE.service"
UNIT_DST="/etc/systemd/system/atlmcpanel-$MODE.service"
if [ -f "$UNIT_SRC" ]; then
  echo "==> 安装 systemd 单元"
  sed "s|__INSTALL_DIR__|$INSTALL_DIR|g" "$UNIT_SRC" > "$UNIT_DST"
  chmod 644 "$UNIT_DST"
  systemctl daemon-reload
  systemctl enable "atlmcpanel-$MODE" >/dev/null 2>&1 || true
  systemctl restart "atlmcpanel-$MODE"
  sleep 3
  if systemctl is-active --quiet "atlmcpanel-$MODE"; then
    echo "    atlmcpanel-$MODE 已启动"
  else
    echo "    atlmcpanel-$MODE 启动失败，请查看日志：" >&2
    journalctl -u "atlmcpanel-$MODE" --no-pager -n 20 >&2
    exit 1
  fi
else
  echo "警告：未找到 systemd 单元模板 $UNIT_SRC" >&2
fi
fi

# ---------- 7. 完成提示 ----------
echo
echo "==============================================="
echo " 安装完成"
echo "==============================================="
if [ "$MODE" = "panel" ]; then
  echo " 面板地址:   http://<本机IP>:8080"
  echo " 首次使用:   浏览器打开后注册第一个账号（自动成为管理员）"
  echo " 配置文件:   $CFG"
  echo " 数据目录:   $INSTALL_DIR/data（含数据库、PKI、备份）"
  echo
  echo " 建议下一步:"
  echo "   1) 配置 HTTPS 证书（见 docs/CERTIFICATES.md）"
  echo "   2) 为节点签发证书并部署 Daemon（见 docs/MTLS.md、docs/DEPLOYMENT.md）"
else
  echo " 节点 ID:    见 $CFG 的 daemon.node_id"
  echo " 配置面板地址: 修改 daemon.panel_address 为面板可达地址"
  echo " 启用 mTLS:  从面板导出节点证书后设置 tls/cert_file/key_file/ca_file"
  echo
  echo " 提示: 若通过面板「节点管理 → 一键部署」安装，则上述配置会自动完成。"
fi
echo " 查看日志:   journalctl -u atlmcpanel-$MODE -f"
echo "==============================================="
