#!/usr/bin/env bash
# 生成并构建插件 SDK 文档站。
#
# 四步：
#   1. apidoc       —— 从 sdk/*.go 提取公开 API 面（签名/注释/分层）→ JSON
#   2. gensite      —— 把 JSON 渲染成 docs/api/*.md + 检索索引 + llms.txt
#   3. mkdocs       —— 构建静态站
#   4. copy_agent   —— 把 Markdown 源搬进站点产物（mkdocs 只渲染 .md，不复制）
#
# 为什么要脚本而不是手敲：API 参考是**生成物**，必须与源码同步，
# 否则文档会悄悄过时（这是文档站最常见的死法）。
#
# 用法：
#   tools/apidoc/build.sh          # 生成 + 构建
#   tools/apidoc/build.sh serve    # 生成 + 本地预览（热重载）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

TMP_API="${TMPDIR:-/tmp}/hologramagent-sdk-api.json"
SITE=site_build

# copy_agent_files 把 docs/ 下的 Markdown 原样复制进站点产物。
#
# 为什么必须复制：mkdocs 只把 .md **渲染**成 HTML，不会把它们放进产物目录。
# 但 agent 需要 Markdown 原文（省 token、不含主题样板），所以 llms.txt 里
# 指的 /api/tools.md 必须真实可访问。llms.txt 与 llms-full.txt 由 gensite 生成。
copy_agent_files() {
  local n=0 rel dir
  while IFS= read -r -d '' f; do
    rel="${f#docs/}"
    [ "${rel##*/}" = "README.md" ] && continue
    dir="$(dirname "$rel")"
    [ "$dir" != "." ] && mkdir -p "$SITE/$dir"
    cp "$f" "$SITE/$rel"
    n=$((n + 1))
  done < <(find docs -name '*.md' -print0)
  echo "  复制 $n 个 Markdown 到 $SITE/（供 agent 直读）"
}

echo "=== 1/4 提取 API 面 ==="
go run ./tools/apidoc -pkgdir ./sdk -out "$TMP_API"

echo "=== 2/4 渲染文档页、检索索引与 agent 入口 ==="
go run ./tools/apidoc/gensite -api "$TMP_API" -out ./docs -examples ./example

echo "=== 3/4 构建静态站 ==="
if [ "${1:-}" = "serve" ]; then
  # 预览模式也要能取到 .md（agent 入口），故先构建一次再起服务。
  mkdocs build --strict >/dev/null
  copy_agent_files
  exec mkdocs serve
fi
mkdocs build --strict

echo "=== 4/4 供 agent 直读的 Markdown ==="
copy_agent_files

echo
echo "完成。产物在 $SITE/，本地预览：tools/apidoc/build.sh serve"
echo "agent 入口：$SITE/llms.txt（目录）、$SITE/llms-full.txt（全文）"
