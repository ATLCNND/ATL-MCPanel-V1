#!/usr/bin/env bash
# 部署后自检脚本：验证面板与节点是否正常工作
#
# 用法：
#   ./smoke-test.sh                          # 检查本机默认部署
#   ./smoke-test.sh http://panel:8080 user pass
#
# 退出码 0 表示全部通过，非 0 表示存在失败项（可用于 CI / 上线检查）。
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080}"
USER="${2:-}"
PASS="${3:-}"

PASS_COUNT=0
FAIL_COUNT=0
TOKEN=""

ok()   { echo "  ✓ $1"; PASS_COUNT=$((PASS_COUNT+1)); }
bad()  { echo "  ✗ $1"; FAIL_COUNT=$((FAIL_COUNT+1)); }
skip() { echo "  – $1（跳过）"; }

# 统一请求封装：支持 http/https，自签证书不校验
req() {
  local method="$1" path="$2" data="${3:-}"
  local args=(-q -S -O /dev/stdout --method="$method" --timeout=10)
  case "$BASE" in https://*) args+=(--no-check-certificate) ;; esac
  [ -n "$TOKEN" ] && args+=(--header="Authorization: Bearer $TOKEN")
  if [ -n "$data" ]; then
    args+=(--header="Content-Type: application/json" --body-data="$data")
  fi
  wget "${args[@]}" "${BASE}${path}" 2>/dev/null
}

code() {
  local method="$1" path="$2"
  local args=(-q -S -O /dev/null --method="$method" --timeout=10)
  case "$BASE" in https://*) args+=(--no-check-certificate) ;; esac
  [ -n "$TOKEN" ] && args+=(--header="Authorization: Bearer $TOKEN")
  wget "${args[@]}" "${BASE}${path}" 2>&1 | grep -m1 'HTTP/' | awk '{print $2}'
}

echo "==============================================="
echo " ATL-MCPanel 部署自检"
echo " 目标: $BASE"
echo "==============================================="
echo

echo "【1. 基础连通性】"
if [ "$(code GET /api/health)" = "200" ]; then
  ok "健康检查接口"
  echo "     $(req GET /api/health)"
else
  bad "健康检查接口不可达 —— 面板未运行或地址不对"
  echo
  echo "自检终止：无法连接面板"
  exit 1
fi

if [ -n "$(req GET / | tr -d '\0' | head -c 200)" ]; then
  ok "前端页面可访问"
else
  bad "前端页面为空（检查 server.web_dir 与 web/dist 是否存在）"
fi

echo
echo "【2. 认证】"
if [ -z "$USER" ] || [ -z "$PASS" ]; then
  skip "未提供账号密码，跳过需登录的检查"
  echo
  echo "提示：传入账号密码可执行完整检查： $0 $BASE admin 你的密码"
  echo
  echo "==============================================="
  printf " 结果: %d 通过 / %d 失败\n" "$PASS_COUNT" "$FAIL_COUNT"
  echo "==============================================="
  [ "$FAIL_COUNT" -eq 0 ] || exit 1
  exit 0
fi

LOGIN="$(req POST /api/auth/login "{\"username\":\"$USER\",\"password\":\"$PASS\"}")"
TOKEN="$(echo "$LOGIN" | grep -o '"token":"[^"]*"' | cut -d'"' -f4)"
if [ -n "$TOKEN" ]; then
  ok "管理员登录"
else
  bad "登录失败：$LOGIN"
  echo
  echo "自检终止：无法登录"
  exit 1
fi

echo
echo "【3. 核心接口】"
for spec in \
  "/api/instances|实例列表" \
  "/api/nodes|节点列表" \
  "/api/alerts|告警中心" \
  "/api/pki|PKI 状态" \
  "/api/panel-backups|面板备份" \
  "/api/users|用户管理" \
  "/api/audit-logs|审计日志" \
  "/api/frps|frps 服务器" \
  "/api/tunnels|穿透隧道" \
  "/api/panel-tunnel|面板穿透" \
  ; do
  path="${spec%%|*}"; label="${spec##*|}"
  c="$(code GET "$path")"
  [ "$c" = "200" ] && ok "$label" || bad "$label (HTTP $c)"
done

echo
echo "【4. mTLS 与安全配置】"
PKI="$(req GET /api/pki)"
if echo "$PKI" | grep -q '"grpc_mtls":true'; then
  ok "Panel↔Daemon mTLS 已启用"
else
  bad "mTLS 未启用（config.yaml 中 server.grpc_mtls 应为 true）"
fi
if echo "$PKI" | grep -q '"initialized":true'; then
  ok "CA 已初始化"
else
  bad "PKI 未初始化"
fi

echo
echo "【5. 节点状态】"
NODES="$(req GET /api/nodes)"
ONLINE=$(echo "$NODES" | grep -o '"status":"online"' | wc -l)
TOTAL=$(echo "$NODES" | grep -o '"id":' | wc -l)
if [ "$TOTAL" -eq 0 ]; then
  bad "尚未登记任何节点（通过「节点管理」添加）"
elif [ "$ONLINE" -eq "$TOTAL" ]; then
  ok "全部 $TOTAL 个节点在线"
else
  bad "$TOTAL 个节点中仅 $ONLINE 个在线"
fi

echo
echo "【6. 活跃告警】"
ALERTS="$(req GET /api/alerts)"
ACTIVE=$(echo "$ALERTS" | grep -o '"active_count":[0-9]*' | cut -d: -f2)
if [ "${ACTIVE:-0}" -eq 0 ]; then
  ok "无待处理告警"
else
  bad "存在 $ACTIVE 条待处理告警（请在面板「告警」中查看）"
fi

echo
echo "【7. 实例状态】"
INST="$(req GET /api/instances)"
COUNT=$(echo "$INST" | grep -o '"instance_id":' | wc -l)
if [ "$COUNT" -eq 0 ]; then
  skip "尚未创建实例"
else
  RUNNING=$(echo "$INST" | grep -o '"live_status":"running"' | wc -l)
  ok "共 $COUNT 个实例，其中 $RUNNING 个运行中"
  # 检查是否存在面板状态与实际不一致的实例
  if echo "$INST" | grep -q '"live_status":"unknown"'; then
    bad "存在无法探测状态的实例（节点可能不可达）"
  fi
fi

echo
echo "==============================================="
printf " 结果: %d 通过 / %d 失败\n" "$PASS_COUNT" "$FAIL_COUNT"
echo "==============================================="

[ "$FAIL_COUNT" -eq 0 ] || exit 1
echo "✅ 自检全部通过"
