#!/usr/bin/env bash
# 为 SDK 仓的 example 插件批量打 .hmap 包，产出可直接随 release 发布的插件包。
#
# 背景：release 此前只发 homed/waiter 二进制与 hmapdev 工具链，**不发插件包**。
# 用户要用某个插件，得自己装 Go 1.25、拉依赖、装 hmapdev、逐个 build —— 这是
# 「开箱即用」名不副实的根源。本脚本把这一步前置到发布流程里。
#
# 用法：
#   ./build_plugin_bundles.sh                    # 全部 example
#   ./build_plugin_bundles.sh weather qq memo    # 指定插件
#   OUT=../dist/plugins ./build_plugin_bundles.sh
#
# 环境：
#   HMAPDEV    hmapdev 可执行文件（默认取 PATH 上的 hmapdev）
#   OUT        产物目录。默认取**内核仓**的 dist/plugins（upload_assets.py 认这个位置），
#              以便直接随 release 发布；不在内核仓内时回退到 SDK 仓的 dist/plugins。
#   JOBS       并行度（默认 CPU 核数）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# 默认产物落到内核仓的 dist/plugins。判定方式：从 SDK 目录向上找“含 internal/ 与
# go.mod”的目录（即内核仓根），找不到就用 SDK 仓自己的 dist/plugins。
# 为何不写死 ../../：SDK 仓在主仓里是 third_party/hologramagentsdk，但也可以被单独
# clone 出来，写死相对路径会把产物丢到仓外或 third_party/dist。
default_out() {
  local d="$SDK_DIR"
  for _ in 1 2 3 4; do
    d="$(cd "$d/.." && pwd)"
    if [ -f "$d/go.mod" ] && [ -d "$d/internal" ]; then
      echo "$d/dist/plugins"; return
    fi
  done
  echo "$SDK_DIR/dist/plugins"
}

EX_DIR="$SDK_DIR/example"
OUT="${OUT:-$(default_out)}"
HMAPDEV="${HMAPDEV:-hmapdev}"

command -v "$HMAPDEV" >/dev/null 2>&1 || {
  echo "error: 找不到 hmapdev（设 HMAPDEV=/path/to/hmapdev 或用 'hmapdev sdk install' 装）" >&2
  exit 1
}

mkdir -p "$OUT"

# 收集候选插件：有 plg.json 才可构建
all=()
for d in "$EX_DIR"/*/; do
  n="$(basename "$d")"
  [ -f "$d/plg.json" ] || continue
  all+=("$n")
done

# 参数指定则取交集（并校验名字有效，避免拼错静默跳过）
if [ "$#" -gt 0 ]; then
  want=("$@")
  sel=()
  for w in "${want[@]}"; do
    found=""
    for n in "${all[@]}"; do [ "$n" = "$w" ] && found=1 && break; done
    [ -n "$found" ] || { echo "error: 未知插件 '$w'（可用: ${all[*]}）" >&2; exit 1; }
    sel+=("$w")
  done
  all=("${sel[@]}")
fi

echo "=== 打包 ${#all[@]} 个插件 → $OUT ==="
echo "    hmapdev: $("$HMAPDEV" --version 2>/dev/null | head -1 || echo "$HMAPDEV")"

build_one() {
  local name="$1"
  local dir="$EX_DIR/$name"
  local log="$OUT/.$name.log"

  # hmapdev build 必须在插件目录内跑（它读当前目录的 plg.json）
  if ! (cd "$dir" && "$HMAPDEV" build >"$log" 2>&1); then
    echo "  ✗ $name 构建失败（见 $log）"
    return 1
  fi

  # 产物有三种形态，不能只认 _bundle.hmap：
  #   1) <name>_bundle.hmap      多平台 bundle（plg.json 里 bundle: true）
  #   2) <name>_<os>_<arch>.hmap 单平台（bundle 关掉时，如 qq）
  #   3) <name>_lua.hmap         Lua 插件（不编译 Go，如 luademo）
  #
  # 注意用 if 而非 `[ -z ] && found=$(ls...)`：在 set -e 下，
  # 一次 ls 无匹配就会让整个子 shell 直接退出，根本走不到后面的兜底。
  local found=""
  local cand
  for pat in "$dir"/dist/*_bundle.hmap "$dir"/dist/*.hmap "$dir"/*_bundle.hmap; do
    if [ -z "$found" ]; then
      cand="$(ls -t $pat 2>/dev/null | head -1 || true)"
      [ -n "$cand" ] && found="$cand"
    fi
  done
  if [ -z "$found" ]; then
    echo "  ✗ $name 未产出 .hmap（见 $log）"
    return 1
  fi

  cp -f "$found" "$OUT/"
  local sz bn
  bn="$(basename "$found")"
  sz="$(stat -c%s "$OUT/$bn" 2>/dev/null || stat -f%z "$OUT/$bn")"
  # 标注形态：单平台/Lua 包与多平台 bundle 不同，发布时要能一眼看出
  local tag=""
  case "$bn" in
    *_bundle.hmap) tag="bundle" ;;
    *_lua.hmap)    tag="lua   " ;;
    *)             tag="单平台" ;;
  esac
  printf "  ✓ %-16s %7.1f MB  %s\n" "$name" "$(echo "$sz" | awk '{print $1/1048576}')" "$tag"
  rm -f "$log"
}

fail=0
pids=()
for n in "${all[@]}"; do
  # 有 nproc 就限并发，没有就串行
  if command -v nproc >/dev/null 2>&1; then
    while [ "$(jobs -rp | wc -l)" -ge "${JOBS:-$(nproc)}" ]; do wait -n 2>/dev/null || true; done
  fi
  ( build_one "$n" ) &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p" || fail=$((fail+1)); done

echo
echo "=== 产出 ==="
ls -la "$OUT"/*.hmap 2>/dev/null | awk '{printf "  %-46s %8.1f MB\n", $9, $5/1048576}' || echo "  （无）"

# 汇总校验和，便于随 release 一起发布与验证
if ls "$OUT"/*.hmap >/dev/null 2>&1; then
  ( cd "$OUT" && sha256sum ./*.hmap > SHA256SUMS.plugins )
  echo
  echo "=== 校验和 → $OUT/SHA256SUMS.plugins ==="
  cat "$OUT/SHA256SUMS.plugins" | sed 's/^/  /'
fi

if [ "$fail" -gt 0 ]; then
  echo
  echo "error: $fail 个插件构建失败" >&2
  exit 1
fi
