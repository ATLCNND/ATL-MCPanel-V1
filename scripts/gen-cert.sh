#!/usr/bin/env bash
# 生成面板 HTTPS 自签证书（用于内网/测试；暴露公网建议替换为 CA 签发证书）
#
# 用法：
#   ./gen-cert.sh [输出目录] [额外SAN...]
# 示例：
#   ./gen-cert.sh /opt/mcpanel/certs panel.example.com 10.0.0.5
#
# 生成的证书含 SAN（现代浏览器不再接受仅 CN 的证书）：
#   DNS:localhost  DNS:panel.local  IP:127.0.0.1  <本机所有 IPv4>  <额外参数>
#
# ---------------------------------------------------------------------------
# ⚠️ 为什么用配置文件而不是 `openssl -addext`（2026-09-17 在 CentOS 7 上踩到）
# ---------------------------------------------------------------------------
# `-addext` 是 **OpenSSL 1.1.1** 才引入的选项，而 CentOS 7 自带的是 **1.0.2k**。
# 用它写 SAN，在 CentOS 7 上直接报 `unknown option -addext`，**证书根本没生成** ——
# 而脚本原本把那句错误 `2>/dev/null` 吞掉了，于是表现为
# "安装一路成功、面板也 active，但 443 不监听、日志里才有一行
#  open certs/panel.crt: no such file or directory"。
#
# 改用 `-config` 指向一个临时配置文件：1.0.2 与 1.1.1+ 都支持，行为一致。
# ---------------------------------------------------------------------------
set -euo pipefail

OUT_DIR="${1:-/opt/mcpanel/certs}"
shift || true

if ! command -v openssl >/dev/null 2>&1; then
  echo "错误：未找到 openssl，请先安装（apt-get install -y openssl）" >&2
  exit 1
fi

mkdir -p "$OUT_DIR"
cd "$OUT_DIR"

# 组装 SAN
SAN="DNS:localhost,DNS:panel.local,IP:127.0.0.1"
# 本机 IPv4（排除回环）
for ip in $(hostname -I 2>/dev/null || true); do
  case "$ip" in
    *:*) continue ;;                       # 跳过 IPv6
    127.*) continue ;;                     # 跳过回环
  esac
  SAN="$SAN,IP:$ip"
done
# 额外参数：含字母视为域名，否则视为 IP
for extra in "$@"; do
  case "$extra" in
    *[a-zA-Z]*) SAN="$SAN,DNS:$extra" ;;
    *)          SAN="$SAN,IP:$extra" ;;
  esac
done

echo "生成证书，SAN = $SAN"

# 临时配置文件（用完就删；放在输出目录里，权限收好）
CNF="$OUT_DIR/.san.cnf"
trap 'rm -f "$CNF"' EXIT
cat > "$CNF" <<EOF
[req]
distinguished_name = dn
x509_extensions    = v3_req
prompt             = no

[dn]
C  = CN
O  = ATL-MCPanel
CN = ATL-MCPanel

[v3_req]
basicConstraints = CA:FALSE
keyUsage         = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName   = $SAN
EOF

# 不吞错误：生成失败要让调用方看见（否则就是"装完却用不了"）
if ! openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
      -keyout panel.key -out panel.crt -config "$CNF" >/dev/null 2>"$OUT_DIR/.gencert.err"; then
  echo "❌ 证书生成失败：" >&2
  sed 's/^/    /' "$OUT_DIR/.gencert.err" >&2
  rm -f "$OUT_DIR/.gencert.err"
  exit 1
fi
rm -f "$OUT_DIR/.gencert.err"

chmod 600 panel.key
chmod 644 panel.crt

echo
echo "证书已生成："
echo "  证书: $OUT_DIR/panel.crt"
echo "  私钥: $OUT_DIR/panel.key"
echo

# `openssl x509 -ext` 也是较新版本才有的选项，这里统一用 -text + grep，兼容 1.0.2
openssl x509 -in panel.crt -noout -subject -dates
openssl x509 -in panel.crt -noout -text 2>/dev/null \
  | grep -A1 'Subject Alternative Name' | tail -1 | sed 's/^ */  SAN:/'

echo
echo "在 config.yaml 中启用："
echo "  server:"
echo "    tls_listen: \":8443\""
echo "    tls_cert: \"$OUT_DIR/panel.crt\""
echo "    tls_key: \"$OUT_DIR/panel.key\""
echo
echo "自签证书浏览器会提示不受信任（属正常现象）。若要让浏览器信任，"
echo "请使用 CA 签发证书（如阿里云免费证书，见 docs/CERTIFICATES.md）。"
echo
echo "（本机 openssl：$(openssl version)）"
