#!/usr/bin/env bash
# 构建前端到 web/dist（由 Panel 按 server.web_dir 静态托管）
set -euo pipefail
cd "$(dirname "$0")/../web"

if ! command -v npm >/dev/null 2>&1; then
  echo "错误：未找到 npm，请先安装 Node.js（>= 18）" >&2
  exit 1
fi

npm run build

echo
echo "前端构建完成：web/dist"
echo "Panel 启动时会按 config.yaml 的 server.web_dir 托管该目录（默认 web/dist）。"
echo "无需重启 Panel 即可生效（静态文件按请求读取），但浏览器需刷新页面。"
