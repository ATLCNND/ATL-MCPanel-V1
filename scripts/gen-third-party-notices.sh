#!/usr/bin/env bash
# 给 THIRD-PARTY-NOTICES.md 补上**许可全文**（并覆盖容器基础镜像里的 Debian 组件）。
#
# 为什么需要它（DEVELOPMENT.md §14.10 的第二个缺口）：
#   原来的声明只有"组件 / 版本 / 许可类型 + 链接"的表格。而 MIT / BSD-2 / BSD-3
#   的要求是**保留版权声明与许可文本本身**，不是给个链接就算 —— 我们的主要分发
#   方式是**离线部署包**，链接根本打不开。Apache-2.0 还要求附带 NOTICE 文件。
#
# 做法：在表格之后追加一节"许可全文"，逐个组件把它的 LICENSE 原文内联进来：
#   · Go 模块：从模块缓存目录 $GOMODCACHE/<mod>@<ver>/LICENSE* 取
#   · 前端依赖：从 web/node_modules/<pkg>/LICENSE* 取
#   · 基础镜像：docker run 进镜像跑 dpkg-query 列出 Debian 包，
#     再从 /usr/share/doc/<pkg>/copyright 取（Debian 的版权汇总文件）
#
# 幂等：每次重新生成整节（以 <!-- LICENSE-TEXTS:BEGIN --> 为界），不会重复堆叠。
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1

OUT=THIRD-PARTY-NOTICES.md
IMG="${ATL_RUNTIME_IMAGE:-atl-mcpanel-runtime:latest}"
TMP=$(mktemp)
SEC=$(mktemp)
BEGIN='<!-- LICENSE-TEXTS:BEGIN -->'
END='<!-- LICENSE-TEXTS:END -->'

echo "==> 1/4 定位并清理上一次的全文节"
if [ ! -f "$OUT" ]; then echo "❌ 缺少 $OUT（先跑 gen_third_party.js 生成表格）" >&2; exit 1; fi
awk -v b="$BEGIN" -v e="$END" '
  index($0,b){skip=1} !skip{print} index($0,e){skip=0}
' "$OUT" > "$TMP"
mv "$TMP" "$OUT"

echo "==> 2/4 追加许可全文节"
{
  echo "$BEGIN"
  echo
  echo "## 附：各组件许可全文"
  echo
  echo "> 本节由 \`scripts/gen-third-party-notices.sh\` 从**实际链接进产物的依赖**中提取，"
  echo "> 与上面的表格一一对应。之所以内联全文而不是只给链接：MIT / BSD 类许可要求"
  echo "> 随副本**保留版权声明与许可文本**，而本项目以离线部署包为主要分发方式，链接不可达。"
  echo
} >> "$OUT"

pick_license() { # $1=目录 → 打印找到的许可文件名
  for n in LICENSE LICENSE.txt LICENSE.md LICENSE-MIT LICENSE-APACHE COPYING NOTICE; do
    [ -f "$1/$n" ] && { echo "$1/$n"; return; }
  done
  # 有的包放在 LICENSE-* / COPYING* 变体里
  ls "$1"/LICEN[CS]E* "$1"/COPYING* 2>/dev/null | head -1
}

GOMODCACHE=$(go env GOMODCACHE 2>/dev/null)
n_go=0; n_npm=0; n_dpkg=0

echo "### Go 模块" >> "$OUT"
echo >> "$OUT"
# 只取**非标准库**且被真正链接进来的模块
go list -deps -f '{{if not .Standard}}{{with .Module}}{{if not .Main}}{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}{{end}}' ./cmd/panel ./cmd/daemon 2>/dev/null \
  | sort -u | while IFS='|' read -r mod ver dir; do
  [ -z "${mod:-}" ] && continue
  lic=$(pick_license "$dir")
  {
    echo "#### ${mod} ${ver}"
    echo
    if [ -n "$lic" ]; then
      echo '```'
      cat "$lic"
      echo '```'
    else
      echo "> ⚠️ 未在模块目录中找到许可文件，请手工核对：\`$dir\`"
    fi
    echo
  } >> "$SEC"
done
n_go=$(grep -c '^#### ' "$SEC" 2>/dev/null || echo 0)

echo "### 前端依赖" >> "$OUT"
echo >> "$OUT"
if [ -d web/node_modules ]; then
  for p in $(node -e "const d=require('./web/package.json').dependencies||{};console.log(Object.keys(d).join(' '))" 2>/dev/null); do
    dir="web/node_modules/$p"
    [ -d "$dir" ] || continue
    ver=$(node -e "try{console.log(require('./$dir/package.json').version)}catch(e){console.log('?')}" 2>/dev/null)
    lic=$(pick_license "$dir")
    {
      echo "#### ${p} ${ver}"
      echo
      if [ -n "$lic" ]; then
        echo '```'
        cat "$lic"
        echo '```'
      else
        echo "> ⚠️ 未找到许可文件，请手工核对：\`$dir\`"
      fi
      echo
    } >> "$SEC"
  done
fi
n_npm=$(grep -c '^#### ' "$SEC" 2>/dev/null || echo 0)

echo "### 容器基础镜像内的 Debian 组件" >> "$OUT"
echo >> "$OUT"
if command -v docker >/dev/null 2>&1 && docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "镜像 \`$IMG\` 基于 Debian 用户态，其组件的版权与许可汇总如下" >> "$OUT"
  echo "（Debian 的 \`/usr/share/doc/<包>/copyright\` 是各包的版权声明与许可原文）。" >> "$OUT"
  echo >> "$OUT"
  # 只列**直接安装**的包（minbase + 我们显式装的），避免把全部依赖摊开成几百条
  pkgs=$(docker run --rm "$IMG" bash -c "dpkg-query -W -f='\${Package}|\${Version}|\${Status}\n'" 2>/dev/null \
    | grep 'install ok installed' | cut -d'|' -f1,2 | head -60)
  n_dpkg=$(echo "$pkgs" | grep -c '|' || echo 0)
  {
    echo "| 包 | 版本 | 许可（取自该包的 copyright 文件） |"
    echo "|---|---|---|"
  } >> "$OUT"
  echo "$pkgs" | while IFS='|' read -r pkg ver; do
    [ -z "${pkg:-}" ] && continue
    echo "| \`$pkg\` | $ver | Debian 版权文件（见下） |" >> "$OUT"
  done
  {
    echo
    echo "<details><summary>展开：各 Debian 包的版权与许可原文（自动提取，逐个包）</summary>"
    echo
  } >> "$OUT"
  echo "$pkgs" | while IFS='|' read -r pkg ver; do
    [ -z "${pkg:-}" ] && continue
    echo "#### deb: ${pkg} ${ver}" >> "$OUT"
    echo >> "$OUT"
    echo '```' >> "$OUT"
    docker run --rm "$IMG" bash -c "cat /usr/share/doc/$pkg/copyright 2>/dev/null | head -60" 2>/dev/null >> "$OUT"
    echo '```' >> "$OUT"
    echo >> "$OUT"
  done
  {
    echo "</details>"
    echo
  } >> "$OUT"
else
  echo "> ⚠️ 本机没有 docker 或没有镜像 \`$IMG\`，**跳过**基础镜像组件一节。" \
       "发布含镜像的包之前必须在有镜像的机器上重跑本脚本。" >> "$OUT"
  echo >> "$OUT"
fi

cat "$SEC" >> "$OUT"
echo "$END" >> "$OUT"
rm -f "$SEC"

echo "==> 3/4 自检"
total=$(grep -c '^#### ' "$OUT" || echo 0)
missing=$(grep -c '⚠️ 未' "$OUT" || echo 0)
echo "  全文条目: $total（Go $(grep -c '^#### ' <(grep -A100000 '^### Go' "$OUT" | grep -B100000 '^### 前端' ) 2>/dev/null || echo '?')）"
echo "  Debian 包: $n_dpkg"
echo "  未找到许可的条目: $missing"
echo "  文件大小: $(du -h "$OUT" | cut -f1)"

echo "==> 4/4 结论"
if [ "$total" -lt 15 ]; then
  echo "  ❌ 全文条目过少（$total），生成可能不完整" >&2; exit 1
fi
if [ "$missing" -gt 0 ]; then
  echo "  ⚠️ 有 $missing 个条目没找到许可文件，需要人工补 —— 这属于合规缺口，别忽略"
fi
echo "  ✅ 已写入 $OUT（幂等：重复运行不会堆叠）"
