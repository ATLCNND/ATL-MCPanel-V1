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
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 \
  -keyout panel.key -out panel.crt \
  -subj "/C=CN/O=ATL-MCPanel/CN=ATL-MCPanel" \
  -addext "subjectAltName=$SAN" 2>/dev/null

chmod 600 panel.key
chmod 644 panel.crt

echo
echo "证书已生成："
echo "  证书: $OUT_DIR/panel.crt"
echo "  私钥: $OUT_DIR/panel.key"
echo
openssl x509 -in panel.crt -noout -subject -dates -ext subjectAltName
echo
echo "在 config.yaml 中启用："
echo "  server:"
echo "    tls_listen: \":8443\""
echo "    tls_cert: \"$OUT_DIR/panel.crt\""
echo "    tls_key: \"$OUT_DIR/panel.key\""
echo
echo "自签证书浏览器会提示不受信任（属正常现象）。若要让浏览器信任，"
echo "请使用 CA 签发证书（如阿里云免费证书，见 docs/CERTIFICATES.md）。"
