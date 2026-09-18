#!/usr/bin/env bash
# 构建 panel 与 daemon（支持多架构 + 完全静态链接）。
#
# ---------------------------------------------------------------------------
# 为什么要静态链接（这是踩出来的，不是洁癖）
# ---------------------------------------------------------------------------
# 原来用 `CGO_ENABLED=1 go build` 得到的是**动态链接**二进制，它继承构建机的 glibc 版本。
# 在 Debian 13（glibc 2.41）上编出来的二进制需要 GLIBC_2.34，而 CentOS 7 只有 2.17
# → 用户机器上直接 `version 'GLIBC_2.34' not found`，节点程序根本起不来。
#
# 静态链接后不依赖目标机的 libc 版本，也不依赖发行版。
#
# ⚠️ 一个**看起来能用其实是坏的**陷阱（务必别走回头路）：
#     `CGO_ENABLED=0 go build` 也能得到 "statically linked"，但 panel 用了
#     mattn/go-sqlite3（CGO 绑定），CGO 关掉后编出来的是个 stub，
#     启动时直接报 `requires cgo to work. This is a stub`。
#     所以：**只用 `file` 断言 statically linked 是不够的**，
#     必须再跑一次真实启动（见 scripts/smoke-binary.sh）。
#     本脚本在本机架构上会自动跑那个冒烟。
#
# ---------------------------------------------------------------------------
# 用法
# ---------------------------------------------------------------------------
#   ./build.sh                          # 本机架构，静态
#   ./build.sh --all                    # linux/amd64 + linux/arm64，静态
#   ./build.sh --targets linux/arm64    # 指定目标
#   ./build.sh --dynamic                # 退回动态链接（本机调试用，产物不可分发）
#   ./build.sh --debug                  # 保留调试信息（默认 -s -w 剥掉符号）
#   ./build.sh --version 1.0.0
#
# 产物：bin/<GOOS>-<GOARCH>/dsh-panel、bin/<GOOS>-<GOARCH>/dsh-daemon
# ---------------------------------------------------------------------------
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=""
TARGETS=""
ALL=""
DYNAMIC=""
DEBUG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --targets) TARGETS="${2:-}"; shift 2 ;;
    --all)     ALL=1; shift ;;
    --dynamic) DYNAMIC=1; shift ;;
    --debug)   DEBUG=1; shift ;;
    -h|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
fi
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

# 目标平台：默认本机；--all 出两个
if [ -n "$ALL" ]; then
  TARGETS="linux/amd64 linux/arm64"
elif [ -z "$TARGETS" ]; then
  TARGETS="$(go env GOOS)/$(go env GOARCH)"
fi

# 交叉编译器：静态链接需要 C 工具链，每个目标架构各一个
cc_for() {
  case "$1" in
    linux/amd64) echo "musl-gcc" ;;
    linux/arm64) echo "aarch64-linux-gnu-gcc" ;;
    *) echo "" ;;
  esac
}

LDFLAGS_BASE="-X github.com/ATLCNND/ATL-MCPanel/internal/common/version.Version=${VERSION}"
LDFLAGS_BASE="${LDFLAGS_BASE} -X github.com/ATLCNND/ATL-MCPanel/internal/common/version.Commit=${COMMIT}"
LDFLAGS_BASE="${LDFLAGS_BASE} -X github.com/ATLCNND/ATL-MCPanel/internal/common/version.BuildTime=${BUILD_TIME}"

echo "==> 版本 ${VERSION}（commit ${COMMIT}）"
echo "==> 目标 ${TARGETS}"
if [ -n "$DYNAMIC" ]; then
  echo "==> 模式 动态（不可分发，仅本机调试）"
else
  echo "==> 模式 静态"
fi
echo

FAILED=0
for T in $TARGETS; do
  GOOS="${T%%/*}"; GOARCH="${T##*/}"
  OUT="bin/${GOOS}-${GOARCH}"
  mkdir -p "$OUT"

  LDFLAGS="$LDFLAGS_BASE"
  [ -z "$DEBUG" ] && LDFLAGS="-s -w $LDFLAGS"

  if [ -n "$DYNAMIC" ]; then
    CGO_ENABLED=1
    export CGO_ENABLED
    TAGS=""
    unset CC || true
  else
    CC_BIN=$(cc_for "$T")
    if [ -z "$CC_BIN" ]; then
      echo "❌ 没有为 ${T} 配置交叉编译器，请用 --targets 指定或加 --dynamic" >&2
      FAILED=1; continue
    fi
    if ! command -v "$CC_BIN" >/dev/null 2>&1; then
      echo "❌ 缺少 $CC_BIN。安装方式：" >&2
      echo "     Debian/Ubuntu: apt-get install -y musl-tools gcc-aarch64-linux-gnu libc6-dev-arm64-cross" >&2
      echo "     （或加 --dynamic 退回动态链接，但产物不能分发给旧系统用户）" >&2
      FAILED=1; continue
    fi
    CGO_ENABLED=1
    export CGO_ENABLED
    export CC="$CC_BIN"
    # netgo/osusergo：静态二进制不要走 cgo 的 NSS 查询（静态链接下会失败）
    TAGS="-tags netgo,osusergo"
    LDFLAGS="$LDFLAGS -linkmode external -extldflags \"-static\""
  fi

  for BIN in panel daemon; do
    NAME="dsh-${BIN}"
    printf '>>> %-12s %s/%s ' "$NAME" "$GOOS" "$GOARCH"
    # shellcheck disable=SC2086
    if GOOS="$GOOS" GOARCH="$GOARCH" go build $TAGS -ldflags "$LDFLAGS" -o "$OUT/$NAME" "./cmd/$BIN" 2>"$OUT/.build-$BIN.err"; then
      SIZE=$(du -h "$OUT/$NAME" | cut -f1)
      LINK=$(file -b "$OUT/$NAME" | grep -o 'statically linked\|dynamically linked' || echo '?')
      echo "→ $SIZE, $LINK"
    else
      echo "失败"
      sed 's/^/      /' "$OUT/.build-$BIN.err" >&2
      FAILED=1
    fi
    rm -f "$OUT/.build-$BIN.err"
  done
  unset CC || true
done

echo
if [ "$FAILED" -ne 0 ]; then
  echo "❌ 有目标构建失败" >&2
  exit 1
fi

# ---------- 断言一：静态链接 + 不引用任何 glibc 符号版本 ----------
# 注意：光看 "statically linked" **不足以**证明产物可用（CGO_ENABLED=0 的 panel 也能过），
# 所以下面还要跑真实启动冒烟。
#
# 而 "GLIBC_2.x 符号版本引用" 这条断言才是"能不能跑在旧系统上"的直接证据：
# 动态链接的二进制会带上 `GLIBC_2.34` 这类版本需求，在 CentOS 7（glibc 2.17）上
# 直接报 `version 'GLIBC_2.34' not found`。静态产物里不该出现任何 GLIBC_ 符号版本。
if [ -z "$DYNAMIC" ]; then
  echo "==> 断言：产物必须是静态链接、且不引用 glibc 符号版本"
  BAD=0
  for T in $TARGETS; do
    for BIN in dsh-panel dsh-daemon; do
      f="bin/${T%%/*}-${T##*/}/$BIN"
      [ -f "$f" ] || continue
      ok=1
      if ! file -b "$f" | grep -q 'statically linked'; then
        echo "    ❌ $f 不是静态链接：$(file -b "$f")"
        ok=0
      fi
      # `|| true` 不能省：grep 没有匹配时退出码是 1，而脚本开了 set -e + pipefail，
      # 赋值语句会因此**静默中断整个脚本**（实测：断言那一行之后什么都不打印，直接退出 1）
      g=$(strings "$f" 2>/dev/null | grep -o 'GLIBC_2\.[0-9]*' | sort -u | tr '\n' ' ' || true)
      if [ -n "$g" ]; then
        echo "    ❌ $f 引用了 glibc 符号版本：$g（在旧系统上会 not found）"
        ok=0
      fi
      [ "$ok" -eq 1 ] && echo "    ✅ $f（静态，无 glibc 版本依赖）"
      [ "$ok" -eq 0 ] && BAD=1
    done
  done
  [ "$BAD" -ne 0 ] && exit 1
fi

# ---------- 断言二：本机架构上真的能启动 ----------
# 这条才是真正管用的：它会把"CGO 关掉导致 SQLite 是 stub"这类问题当场抓住。
HOST_T="$(go env GOOS)/$(go env GOARCH)"
if [ -z "$DYNAMIC" ] && [ -f scripts/smoke-binary.sh ]; then
  case " $TARGETS " in
    *" $HOST_T "*)
      echo
      echo "==> 断言：静态产物真的能启动（本机架构 $HOST_T）"
      if bash scripts/smoke-binary.sh "bin/${HOST_T%%/*}-${HOST_T##*/}/dsh-panel"; then
        echo "    ✅ 启动冒烟通过"
      else
        echo "    ❌ 启动冒烟失败 —— 产物不可用，别拿去打包" >&2
        exit 1
      fi
      ;;
  esac
fi

echo
echo "==> 完成"
for T in $TARGETS; do
  d="bin/${T%%/*}-${T##*/}"
  [ -d "$d" ] || continue
  for BIN in dsh-panel dsh-daemon; do
    [ -f "$d/$BIN" ] || continue
    printf '    %-28s %s\n' "$T/$BIN" "$(du -h "$d/$BIN" | cut -f1)"
  done
done
echo
echo "下一步：bash scripts/build-release.sh --arch <amd64|arm64>  生成可分发的部署包"
