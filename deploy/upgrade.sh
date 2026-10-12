#!/usr/bin/env bash
# ATL-MCPanel 升级脚本
#
# 用法：
#   面板：sudo ./upgrade.sh panel  [安装目录]
#   节点：sudo ./upgrade.sh daemon [安装目录]
#   只看会做什么、不真改：追加 --dry-run
#
# 脚本假设当前目录（解压后的包目录）包含：
#   bin/atl-panel 或 bin/atl-daemon、deploy/、config.example.yaml（面板包另有 web-dist/）
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

BIN_NAME="atl-$MODE"
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

# 备份哪个文件：通常是 bin/$BIN_NAME。但**首次跨改名升级**（0.9.x 的 dsh-* 升到
# 改名后的 atl-*）时，bin/atl-panel 还不存在 —— 只有旧名 bin/dsh-panel 那个真实文件。
# 不认它的话，这一节的备份会"跳过"，而末尾那段"启动失败就自动回滚"就会因为
# 备份不存在而形同虚设：**恰好在改名这一次升级上失去回滚能力**。
#
# 所以显式认旧名。`[ ! -L ]` 不能省：本次改名自己会造一个 dsh-* 软链接，
# 它不是"另一个版本"，只是个指向新名的入口。
PREV_BIN=""
if [ -f "$INSTALL_DIR/bin/$BIN_NAME" ]; then
  PREV_BIN="$INSTALL_DIR/bin/$BIN_NAME"
else
  for cand in "$INSTALL_DIR/bin/atl-$MODE" "$INSTALL_DIR/bin/dsh-$MODE"; do
    if [ -f "$cand" ] && [ ! -L "$cand" ]; then PREV_BIN="$cand"; break; fi
  done
fi
PREV_NAME="$(basename "${PREV_BIN:-$BIN_NAME}")"

if [ -n "$PREV_BIN" ]; then
  run "cp -p '$PREV_BIN' '$BACKUP/$PREV_NAME'"
  echo "    已备份到 $BACKUP/$PREV_NAME"
  [ "$PREV_NAME" != "$BIN_NAME" ] && echo "    （旧名二进制：这次升级会把内容装到 $BIN_NAME 上）"
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
  # 错误输出落到 mktemp 建的文件：写死 /tmp/.upgrade-install.err 时，本机任何
  # 用户都能预先建同名文件（甚至是指向别处的软链接）来影响 root 的这次写入
  #（2026-10-01 安全审查 L8）。
  ERRLOG=$(mktemp /tmp/atl-upgrade-err.XXXXXX 2>/dev/null || echo /tmp/atl-upgrade-err.$$)
  if install -m 0755 "$SRC_DIR/bin/$BIN_NAME" "$INSTALL_DIR/bin/$BIN_NAME" 2>"$ERRLOG"; then
    echo "    已替换 $BIN_NAME"
    rm -f "$ERRLOG"
  else
    err=$(cat "$ERRLOG" 2>/dev/null); rm -f "$ERRLOG"
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
  if [ -f "$SRC_DIR/bin/atl-daemon" ]; then
    run "install -m 0755 '$SRC_DIR/bin/atl-daemon' '$INSTALL_DIR/bin/atl-daemon'"
    echo "    已同步更新 atl-daemon（供一键部署节点下发）"
  fi
fi

# ---------------------------------------------------------------------------
# 3b. 旧名兼容软链接 + systemd 单元改名（**过渡用，下个版本删这一步**）
#
# 二进制在 2026-10-12 由 dsh-panel / dsh-daemon 改名成 atl-panel / atl-daemon。
#
# 为什么必须专门处理：本脚本与 install.sh 不同，**从不重写单元文件**
#（第 0.5 节只"读" ExecStart 用来校验安装目录对不对）。于是升级到改名后的版本时，
# 老单元的 ExecStart 仍指向 bin/dsh-panel，而那个路径下已经是新名字的文件 ——
# 不处理的话**升级后面板会因为"可执行文件不存在"直接起不来**，报错还埋在
# journal 里（systemd 只会说一句 status=203/EXEC）。
#
# 两道一起做，互为兜底：
#   1) 留旧名软链接：任何还引用旧名的外部脚本 / 别名继续可用；
#   2) 把单元里的旧名改成新名：让 `systemctl cat` 与实际一致，将来软链接删掉
#      也不会把服务带崩。
#
# 位置：必须排在上面**所有文件替换之后**（面板模式的 atl-daemon 是后装的），
# 否则检查 [ -f bin/atl-daemon ] 会落空、链接建不出来。
# ---------------------------------------------------------------------------
if [ -n "$DRY" ]; then
  echo "    [dry-run] ln -sfn atl-panel  '$INSTALL_DIR/bin/dsh-panel'"
  echo "    [dry-run] ln -sfn atl-daemon '$INSTALL_DIR/bin/dsh-daemon'"
  echo "    [dry-run] sed -i 's|bin/dsh-|bin/atl-|' '$UNIT_PATH'（若单元里还写着旧名）"
else
  echo
  echo "==> 旧名兼容软链接（dsh-* → atl-*）"
  if [ -f "$INSTALL_DIR/bin/atl-panel" ]; then
    ln -sfn atl-panel "$INSTALL_DIR/bin/dsh-panel"
    echo "    已创建 dsh-panel → atl-panel"
  fi
  if [ -f "$INSTALL_DIR/bin/atl-daemon" ]; then
    ln -sfn atl-daemon "$INSTALL_DIR/bin/dsh-daemon"
    echo "    已创建 dsh-daemon → atl-daemon"
  fi
fi

# 单元里若还写着旧名，就地改掉。只在 SERVICE_OK=1 时动 —— 那个标志的意思是
# "单元确实属于本次的安装目录"；为 0 说明这台机器上可能有另一套部署，动它就是误伤。
if [ "$SERVICE_OK" -eq 1 ] && grep -qE 'bin/dsh-(panel|daemon)' "$UNIT_PATH" 2>/dev/null; then
  OLD_EXEC=$(grep -E '^ExecStart=' "$UNIT_PATH" 2>/dev/null | head -1 || true)
  if [ -z "$DRY" ]; then
    echo "==> 更新 systemd 单元里的二进制名"
    sed -i 's|bin/dsh-panel|bin/atl-panel|g; s|bin/dsh-daemon|bin/atl-daemon|g' "$UNIT_PATH"
    NEW_EXEC=$(grep -E '^ExecStart=' "$UNIT_PATH" 2>/dev/null | head -1 || true)
    echo "    旧: $OLD_EXEC"
    echo "    新: $NEW_EXEC"
    if [ "$NEW_EXEC" = "$OLD_EXEC" ]; then
      # sed 没生效就是真出问题了（只读挂载、权限、单元被改得不像样），
      # 这时**必须让升级失败**：默默留下一份指向不存在文件的单元，
      # 后果是服务再也起不来，而人往往很久之后才发现。
      echo "    ❌ 单元改写没有生效，请手动把 ExecStart 改成 $INSTALL_DIR/bin/$BIN_NAME" >&2
      exit 1
    fi
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

  if [ -n "$PREV_BIN" ] && [ -f "$BACKUP/$PREV_NAME" ]; then
    echo "==> 自动回滚到升级前的二进制" >&2
    # 把备份的**内容**装回 $BIN_NAME，而不是装回它原来的文件名：
    # 上面可能已经把单元的 ExecStart 改成了新名，回滚到旧文件名会让单元继续
    # 指向不存在的东西 —— "回滚"要回的是**旧版本**，不是旧文件名。
    install -m 0755 "$BACKUP/$PREV_NAME" "$INSTALL_DIR/bin/$BIN_NAME"
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
