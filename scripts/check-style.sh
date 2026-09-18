#!/usr/bin/env bash
# 代码风格门禁：gofmt 干净 + 没有 UTF-8 BOM。
#
# 为什么需要它：这两类问题**编译器和测试都不会报**，只有人肉看 review 才能发现，
# 于是就会一直烂下去。实测这个仓库在 2026-09-16 之前有 53 个文件不是 gofmt 干净的、
# 另有 11 个文件带 BOM（其中 2 个 .go 文件正是"永远不可能 gofmt 干净"的原因）。
#
# 用法：
#   bash scripts/check-style.sh          # 检查（CI / 提交前跑）
#   gofmt -w cmd internal                # 修复 gofmt 问题
#   node F:/server/strip_bom.js --write  # 修复 BOM（该脚本在私有工具目录）
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0

# ---------- 1. gofmt ----------
if ! command -v gofmt >/dev/null 2>&1; then
  echo "警告：未找到 gofmt，跳过格式检查（需要 Go 工具链）" >&2
else
  out=$(gofmt -l cmd internal 2>&1)
  if [ -n "$out" ]; then
    echo "❌ 以下文件不是 gofmt 格式（在项目根目录执行 gofmt -w cmd internal 修复）：" >&2
    echo "$out" >&2
    fail=1
  fi
fi

# ---------- 2. UTF-8 BOM ----------
# BOM 在 .sh 里会让 `#!/usr/bin/env bash` 变成 "bad interpreter: No such file or directory"，
# 在 .go 里会让文件永远无法通过 gofmt。PowerShell 5.1 的 `Set-Content -Encoding utf8`
# 就是最常见的来源 —— 在 Windows 上编辑过文件后务必跑一遍这个检查。
bom=$(grep -rlP '^\xEF\xBB\xBF' \
  --include='*.go' --include='*.sh' --include='*.ts' --include='*.tsx' \
  --include='*.css' --include='*.html' --include='*.yaml' --include='*.yml' \
  --include='*.json' --include='*.md' --include='*.proto' --include='*.service' \
  cmd internal web/src proto scripts deploy docs 2>/dev/null)
if [ -n "$bom" ]; then
  echo "❌ 以下文件带 UTF-8 BOM（去掉开头 3 个字节即可）：" >&2
  echo "$bom" >&2
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "✅ 风格门禁通过（gofmt 干净、无 BOM）"
else
  echo "" >&2
  echo "风格门禁未通过。这两类问题测试不会发现，但会让别人 clone 下来就踩坑。" >&2
fi
exit "$fail"
