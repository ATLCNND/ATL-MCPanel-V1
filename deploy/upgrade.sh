#!/usr/bin/env bash
# ATL-MCPanel 升级脚本
#
# 用法：
#   面板：sudo ./upgrade.sh panel  [安装目录]
#   节点：sudo ./upgrade.sh daemon [安装目录]
#   只看会做什么、不真改：追加 --dry-run
#
# 脚本假设当前目录（解压后的包目录）包含：
#   bin/dsh-panel 或 bin/dsh-daemon、deploy/、config.example.yaml（面板包另有 web-dist/）
#
# ---------------------------------------------------------------------------
# 这个脚本**只做四件事**，每件都刻意做得保守
# ---------------------------------------------------------------------------
#   1. 备份数据库（面板）—— 升级失败时这是唯一的退路
#   2. 备份当前二进制 —— 出问题可以立刻换回来
#   3. 替换二进制与前端（**绝不碰 config.yaml 与 data/**）
#   4. 重启服务并**验证它真的起来了**（起不来就自动回滚二进制）
#
# 为什么第 4 步要自动回滚：V1 升级最常见的失败是"二进制换上去、服务起不来"，
# 这时候人在机房外只能干瞪眼。留一份旧二进制并把服务拉回来，比打印一堆日志有用。
# ---------------------------------------------------------------------------
set -euo pipefail

MODE=""
INSTALL_DIR=""
DRY=""
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY=1 ;;
    panel|daemon) MODE="$arg" ;;
    -*) echo "未知选项: $arg" >&2; exit 1 ;;
    *) INSTALL_DIR="$arg" ;;
  esac
done
INSTALL_DIR="${INSTALL_DIR:-/opt/mcpanel}"

if [ "$MODE" != "panel" ] && [ "$MODE" != "daemon" ]; then
  echo "用法: $0 <panel|daemon> [安装目录] [--dry-run]" >&2
  exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "请以 root 运行（需要写 systemd 单元与重启服务）" >&2
  exit 1
fi

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
UNIT="atlmcpanel-$MODE"
SERVICE="/etc/systemd/system/${UNIT}.service"

run() {
  if [ -n "$DRY" ]; then echo "    [dry-run] $*"; else eval "$@"; fi
}

echo "==> 升级模式: $MODE"
echo "==> 安装目录: $INSTALL_DIR"
echo "==> 来源目录: $SRC_DIR"
[ -n "$DRY" ] && echo "==> ⚠️ dry-run：只打印，不做任何修改"

if [ ! -d "$INSTALL_DIR" ]; then
  echo "错误：$INSTALL_DIR 不存在。全新安装请用 install.sh。" >&2
  exit 1
fi

BIN_NAME="dsh-$MODE"
if [ ! -f "$SRC_DIR/bin/$BIN_NAME" ]; then
  echo "错误：包里缺少 bin/$BIN_NAME" >&2
  exit 1
fi

# ---------- 0. 记录升级前状态 ----------
PREV_VERSION="unknown"
if [ -x "$INSTALL_DIR/bin/$BIN_NAME" ]; then
  PREV_VERSION=$("$INSTALL_DIR/bin/$BIN_NAME" -version 2>/dev/null | head -1 || echo "unknown")
fi
NEW_VERSION=$("$SRC_DIR/bin/$BIN_NAME" -version 2>/dev/null | head -1 || echo "unknown")
echo
echo "==> 版本变化"
echo "    当前: $PREV_VERSION"
echo "    新版: $NEW_VERSION"

# 版本没变时提醒一句：覆盖升级没意义，多半是拿错了包
if [ "$PREV_VERSION" = "$NEW_VERSION" ] && [ "$PREV_VERSION" != "unknown" ]; then
  echo "    ⚠️ 版本号相同 —— 确认一下是不是拿错了包（继续执行会原样覆盖）"
fi

STAMP=$(date +%Y%m%d-%H%M%S)
BACKUP="$INSTALL_DIR/backup-$STAMP"

# ---------- 0.5 确认 systemd 单元确实属于**这个**安装目录 ----------
#
# 为什么必须查：单元名只由角色决定（atlmcpanel-panel），与安装目录无关。
# 如果机器上有两个安装（比如一个生产 /opt/mcpanel、一个测试 /tmp/x），
# 对着测试目录跑升级会**把生产的服务停掉再拉起** —— 实测踩到过：
# 升级测试目录时，日志里出现"atlmcpanel-panel 已启动"，其实是把 /opt/mcpanel 那套重启了。
# 所以先读单元的 ExecStart，确认它指的是 $INSTALL_DIR，不是就别碰服务。
UNIT_PATH="/etc/systemd/system/${UNIT}.service"
SERVICE_OK=0
if [ -f "$UNIT_PATH" ]; then
  UNIT_EXEC=$(grep -E '^ExecStart=' "$UNIT_PATH" 2>/dev/null | head -1 || true)
  case "$UNIT_EXEC" in
    *"$INSTALL_DIR"*) SERVICE_OK=1 ;;
    *)
      echo
      echo "⚠️ $UNIT_PATH 指向的不是本次的安装目录，**跳过所有服务操作**："
      echo "     单元里: $UNIT_EXEC"
      echo "     本次目录: $INSTALL_DIR"
      echo "   只替换文件，不动服务。若确实要重启，请手动 systemctl restart $UNIT。"
      ;;
  esac
else
  echo
  echo "提示：没有 $UNIT_PATH（可能是 --no-service 安装或单元名不同），跳过服务操作。"
fi

# ---------- 1. 备份数据库 ----------
DB="$INSTALL_DIR/data/mcpanel.db"
if [ "$MODE" = "panel" ] && [ -f "$DB" ]; then
  echo
  echo "==> 备份数据库"
  run "mkdir -p '$BACKUP'"
  # 用 sqlite3 的 .backup 而不是 cp：WAL 模式下直接拷 .db 可能拿到不一致的快照。
  # 没有 sqlite3 命令时退回 cp（并明确告知风险）。
  if command -v sqlite3 >/dev/null 2>&1; then
    if [ -n "$DRY" ]; then
      echo "    [dry-run] sqlite3 '$DB' \".backup '$BACKUP/mcpanel.db'\""
    else
      sqlite3 "$DB" ".backup '$BACKUP/mcpanel.db'" 2>/dev/null \
        && echo "    已用 sqlite3 .backup 生成一致性快照" \
        || { cp "$DB" "$BACKUP/mcpanel.db"; echo "    sqlite3 备份失败，已退回 cp（WAL 下可能不完整）"; }
    fi
  else
    run "cp '$DB' '$BACKUP/mcpanel.db'"
    echo "    提示：未安装 sqlite3 命令，退回 cp。WAL 模式下这不是一致性快照，"
    echo "          建议先停服务再升级，或安装 sqlite3 后重跑。"
  fi
fi

# ---------- 2. 备份当前二进制（用于回滚） ----------
echo
echo "==> 备份当前二进制"
run "mkdir -p '$BACKUP'"
if [ -f "$INSTALL_DIR/bin/$BIN_NAME" ]; then
  run "cp -p '$INSTALL_DIR/bin/$BIN_NAME' '$BACKUP/$BIN_NAME'"
  echo "    已备份到 $BACKUP/$BIN_NAME"
else
  echo "    （没有旧二进制，跳过 —— 全新装的话应该用 install.sh）"
fi

# ---------- 3. 替换二进制与前端 ----------
# ⚠️ 这一节**只**动 bin/ 与 web/dist，绝不动 config.yaml、data/、instances/、certs/
echo
echo "==> 替换二进制"
if [ -n "$DRY" ]; then
  echo "    [dry-run] install -m 0755 '$SRC_DIR/bin/$BIN_NAME' '$INSTALL_DIR/bin/$BIN_NAME'"
else
  # 先停服务：替换正在运行的二进制会报 "Text file busy"
  [ "$SERVICE_OK" -eq 1 ] && { systemctl stop "$UNIT" 2>/dev/null || true; }
  if install -m 0755 "$SRC_DIR/bin/$BIN_NAME" "$INSTALL_DIR/bin/$BIN_NAME" 2>/tmp/.upgrade-install.err; then
    echo "    已替换 $BIN_NAME"
  else
    err=$(cat /tmp/.upgrade-install.err 2>/dev/null); rm -f /tmp/.upgrade-install.err
    echo "    ❌ 替换失败：$err" >&2
    case "$err" in
      *"Text file busy"*)
        echo "    二进制正在运行且不属于本安装目录的 unit —— 请先手动停掉占用它的进程" >&2
        ;;
    esac
    exit 1
  fi
fi

if [ "$MODE" = "panel" ]; then
  if [ -d "$SRC_DIR/web-dist" ]; then
    echo "==> 替换前端静态资源"
    if [ -n "$DRY" ]; then
      echo "    [dry-run] rm -rf '$INSTALL_DIR/web/dist' && cp -r web-dist"
    else
      rm -rf "$INSTALL_DIR/web/dist"
      mkdir -p "$INSTALL_DIR/web/dist"
      cp -r "$SRC_DIR/web-dist/." "$INSTALL_DIR/web/dist/"
      echo "    已替换 web/dist"
    fi
  else
    echo "提示：包内没有 web-dist，前端保持原样"
  fi
  # 面板包里带了同版本 daemon，一并更新，保证"一键部署节点"下发的是配套版本
  if [ -f "$SRC_DIR/bin/dsh-daemon" ]; then
    run "install -m 0755 '$SRC_DIR/bin/dsh-daemon' '$INSTALL_DIR/bin/dsh-daemon'"
    echo "    已同步更新 dsh-daemon（供一键部署节点下发）"
  fi
fi

# ---------- 4. 重启并验证；失败就回滚 ----------
echo
echo "==> 重启服务"
if [ -n "$DRY" ]; then
  echo "    [dry-run] systemctl restart $UNIT"
  echo
  echo "==> dry-run 结束，未做任何修改"
  exit 0
fi

if [ "$SERVICE_OK" -ne 1 ]; then
  echo "    跳过（该 unit 不属于本安装目录或不存在）"
  echo
  echo "==> 文件替换完成"
  echo "    版本: $NEW_VERSION"
  echo "    备份: $BACKUP"
  echo "    服务需要你手动重启（见上面提示）"
  exit 0
fi

systemctl daemon-reload
systemctl restart "$UNIT"
sleep 3

if systemctl is-active --quiet "$UNIT"; then
  echo "    ✅ $UNIT 已启动"
  echo
  echo "==> 升级完成"
  echo "    版本: $NEW_VERSION"
  echo "    备份: $BACKUP"
  echo "    查看日志: journalctl -u $UNIT -n 50 --no-pager"
else
  echo "    ❌ $UNIT 启动失败" >&2
  echo >&2
  echo "--- 失败日志（最后 30 行）---" >&2
  journalctl -u "$UNIT" -n 30 --no-pager >&2 || true
  echo >&2

  if [ -f "$BACKUP/$BIN_NAME" ]; then
    echo "==> 自动回滚到升级前的二进制" >&2
    install -m 0755 "$BACKUP/$BIN_NAME" "$INSTALL_DIR/bin/$BIN_NAME"
    systemctl restart "$UNIT"
    sleep 3
    if systemctl is-active --quiet "$UNIT"; then
      echo "    ✅ 已回滚，服务恢复正常（仍是你升级前的版本）" >&2
      echo "    请把上面的失败日志发出来定位问题。" >&2
    else
      echo "    ❌ 回滚后仍未起来，需要人工介入。" >&2
      echo "    数据备份在: $BACKUP" >&2
    fi
  else
    echo "    没有可回滚的旧二进制（本次是首次安装？）" >&2
  fi
  exit 1
fi
