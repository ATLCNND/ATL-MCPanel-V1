#!/usr/bin/env bash
# 二进制启动冒烟：拿一个真的 panel 二进制起来，确认它**真的能用**。
#
# ---------------------------------------------------------------------------
# 为什么必须有这一步
# ---------------------------------------------------------------------------
# 收尾 A 项原本的验收标准是「用 file 断言 statically linked」。这条标准是**不够的**：
#
#   CGO_ENABLED=0 go build  →  file 报告 "statically linked"  ✅ 断言通过
#                           但启动时：FATAL 初始化数据库失败: requires cgo to work. This is a stub
#
# 也就是说产物完全不可用，而构建流水线会一路绿灯，直到用户机器上才炸。
# 所以真正管用的断言是**启动一次**：它会打开数据库（跑迁移）、监听端口、响应 HTTP。
# 本脚本就是干这个的。
#
# 用法：scripts/smoke-binary.sh <panel 二进制路径>
# 退出码 0 = 冒烟通过
# ---------------------------------------------------------------------------
set -uo pipefail

BIN="${1:-}"
if [ -z "$BIN" ] || [ ! -x "$BIN" ]; then
  echo "用法: $0 <panel 二进制路径>" >&2
  exit 2
fi
# 转成绝对路径：下面要 cd 到临时目录跑，相对路径会失效
# （踩过：报 "bin/linux-amd64/dsh-panel: 没有那个文件或目录"，
#   看着像二进制缺失，其实是路径相对了）
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"

W=$(mktemp -d)
PID=""
cleanup() {
  # 按 PID 杀，**绝不用 pkill -f**：脚本自身的命令行里含有二进制名，
  # pkill -f 会把执行它的这个 shell 一起杀掉（HANDOFF 第九节记过这个坑）。
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    kill "$PID" 2>/dev/null
    for _ in 1 2 3 4 5; do kill -0 "$PID" 2>/dev/null || break; sleep 0.2; done
    kill -9 "$PID" 2>/dev/null
  fi
  rm -rf "$W"
}
trap cleanup EXIT

# 端口：在 20000-30000 里随机挑一个，避免和机器上已有的服务撞
PORT=$(( (RANDOM % 10000) + 20000 ))
GRPC=$(( PORT + 1 ))

mkdir -p "$W/data"
# jwt_secret 必须 >= 32 字符（config 校验会拒绝短密钥），这里用固定串即可 ——
# 冒烟用的是临时库、临时端口，进程跑完就删
cat > "$W/config.yaml" <<EOF
server:
  listen: "127.0.0.1:${PORT}"
  grpc_listen: "127.0.0.1:${GRPC}"
  external_url: "http://127.0.0.1:${PORT}"
  web_dir: ""
db:
  driver: sqlite3
  dsn: data/smoke.db
auth:
  jwt_secret: "smoke-test-secret-not-for-production-0123456789"
EOF

cd "$W" || exit 2
"$BIN" -config config.yaml > "$W/run.log" 2>&1 &
PID=$!

# ---- 1. 等它监听（最多 15s）----
listening=0
for _ in $(seq 1 75); do
  kill -0 "$PID" 2>/dev/null || break            # 进程已死，不用再等
  if (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null; then exec 3>&-; listening=1; break; fi
  sleep 0.2
done

if ! kill -0 "$PID" 2>/dev/null; then
  echo "  进程已退出，日志如下：" >&2
  sed 's/^/    /' "$W/run.log" >&2
  exit 1
fi
if [ "$listening" -ne 1 ]; then
  echo "  15s 内没有监听 ${PORT}，日志如下：" >&2
  sed 's/^/    /' "$W/run.log" >&2
  exit 1
fi

# ---- 2. 数据库真的建起来了吗（证明 SQLite 驱动可用、迁移跑过）----
# 注意：不要用 grep 去数 SQLite 文件里的 "CREATE TABLE" —— 那是二进制文件，
# 匹配结果不可靠（第一版就是这么写的，输出里出现了 "含 0\n0 处"）。
# 真正有意义的证据是"迁移条数"，下面从日志里取。
if [ ! -s "$W/data/smoke.db" ]; then
  echo "  数据库文件没有生成 —— SQLite 驱动可能不可用" >&2
  sed 's/^/    /' "$W/run.log" >&2
  exit 1
fi
DBSIZE=$(stat -c %s "$W/data/smoke.db" 2>/dev/null || echo 0)

# ---- 3. HTTP 真的响应吗 ----
# 用 bash 的 /dev/tcp 手写一个最小 HTTP 请求：目标机不保证有 curl
resp=$( (exec 3<>"/dev/tcp/127.0.0.1/${PORT}"
          printf 'GET /api/health HTTP/1.0\r\nHost: 127.0.0.1\r\n\r\n' >&3
          timeout 5 cat <&3
          exec 3>&-) 2>/dev/null | head -1 )
code=$(echo "$resp" | grep -o '[0-9][0-9][0-9]' | head -1)

# ---- 4. 迁移日志 ----
migs=$(grep -c '数据库迁移已应用' "$W/run.log" 2>/dev/null || echo 0)

echo "    监听 ${PORT} ✓ | 数据库 ${DBSIZE} 字节 ✓ | 迁移 ${migs} 条 | HTTP ${code:-无响应}"

if [ -z "$code" ]; then
  echo "  HTTP 无响应，日志：" >&2
  sed 's/^/    /' "$W/run.log" >&2
  exit 1
fi
case "$code" in
  2*|3*|4*) exit 0 ;;   # 401/403 也算通过：说明服务在正常工作（只是没带鉴权）
  *) echo "  HTTP 返回了异常状态码 ${code}，日志：" >&2
     sed 's/^/    /' "$W/run.log" >&2
     exit 1 ;;
esac
