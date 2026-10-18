#!/usr/bin/env bash
# 给 SDK 发版打包**示例插件**的 .hmap 产物。
#
# 为什么要在 SDK 仓库里发示例插件的 hmap：
#   插件二进制与内核是**协议绑定**的（internal/plugin/proc/protocol.go 的
#   ProtocolVersion + 统一共享内存区魔数）。SDK 升版往往同时意味着协议变化，
#   而示例插件（qq/memo/browser/…）是使用者最常直接安装的东西。
#   如果 SDK 只发工具链不发示例产物，使用者要么自己重编、要么用到与本版 SDK
#   不匹配的旧产物——后者的表现是握手失败（协议/魔数不匹配），而且看起来像
#   「插件坏了」而不是「版本不配套」。
#
# 用法：
#   package/build-examples.sh [TARGET] [OUT_DIR]
#     TARGET   native(默认) | linux/amd64 | linux/arm64 | darwin/amd64 | darwin/arm64 | windows/amd64 | all
#     OUT_DIR  产物目录（默认 build/examples）
#
# 产物：
#   <OUT_DIR>/<name>_<goos>_<goarch>.hmap   每个示例插件一份
#   <OUT_DIR>/SHA256SUMS                    全部产物齐全**之后**才计算
#   <OUT_DIR>/MANIFEST.txt                  版本、协议版本、产自哪个 commit
#
# 纪律（与本项目其它构建脚本一致）：
#   1. 判成功看**产物是否存在**，不看退出码——hmapdev 对部分错误只打印不退出。
#   2. SHA256SUMS 必须在全部产物生成完毕后一次算完，边打边算会漏掉后生成的包。
set -uo pipefail

SDK_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET="${1:-native}"
OUT_DIR="${2:-$SDK_ROOT/build/examples}"
GO="${GO:-$(command -v go 2>/dev/null || echo go)}"

case "$TARGET" in
  native)        GOOS=""; GOARCH="" ;;
  linux/amd64)   GOOS=linux;   GOARCH=amd64 ;;
  linux/arm64)   GOOS=linux;   GOARCH=arm64 ;;
  darwin/amd64)  GOOS=darwin;  GOARCH=amd64 ;;
  darwin/arm64)  GOOS=darwin;  GOARCH=arm64 ;;
  windows/amd64)
    # 明确拒绝，而不是让调用方拿到一句深层 Go 编译错误。
    # 协议 2 的统一共享内存区只移植到了 Unix：内核 internal/plugin/proc/
    # shmpass_windows.go 仍是旧的 SHM_STAGE/SHM_EVTRING 两段布局，
    # 插件模板 proc_shm_windows.go 也缺 attachUnifiedShm。
    echo "windows 目标暂不支持：协议 2 的统一共享内存区未移植到 Windows（内核与插件模板均缺实现）。" >&2
    exit 1
    ;;
  all)
    echo "本脚本一次只构建一个平台；请由 package/build.sh 传入具体目标。" >&2
    exit 1
    ;;
  *)
    echo "Unknown target: $TARGET" >&2
    echo "Usage: $0 [native|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|all] [OUT_DIR]" >&2
    exit 1
    ;;
esac

export CGO_ENABLED=0

# 按平台逐个构建，**不用** bundle 模式：
#   - bundle 会连 windows 一起编，而协议 2 的统一共享区尚未移植到 Windows
#     （内核 shmpass_windows.go 仍是旧的两段布局），必然失败；
#   - 逐平台构建每个目标都产出一份 .hmap，正是发版要附的产物。
# 平台名解析成本脚本后面用（校验和与 MANIFEST 都要写清楚是哪个平台）。
if [ -z "${GOOS:-}" ]; then
  GOOS="$(go env GOOS)"; GOARCH="$(go env GOARCH)"
fi

# 1) 先保证工具链可用：示例必须用**本仓当前源码**构建，否则产物协议与这一版 SDK 不符。
#    允许外部指定（发版脚本会在跨平台构建后把刚产出的工具链路径传进来）。
# 工具链二进制名由 plugindev 改为 hmapdev；旧变量名 PLUGINDEV 仍兼容。
#
# ★ 下面三处必须读 **HMAPDEV**（上一行刚解析出的规范名）。
#   曾经读的是 PLUGINDEV：那样只有调用方恰好传旧名时才工作，
#   而新名 HMAPDEV 只被赋给 HMAPDEV 本身、PLUGINDEV 仍是未定义，
#   在 `set -u` 下第一处判断就 "PLUGINDEV: unbound variable" 直接退出。
#   实测（2026-09-29）：HMAPDEV=... → exit 1；PLUGINDEV=... → 20/20 成功。
#   发版路径传的是旧名（build.sh:92）故一直没暴露——正是「兼容」二字
#   写在注释里、却没在代码里做到的那种缺陷。
HMAPDEV="${HMAPDEV:-${PLUGINDEV:-$SDK_ROOT/build/hmapdev}}"

# ★ 工具链二进制**过期就必须重编**，不能只看"文件在不在"。
#
# 原来的判据是 `[ ! -x "$HMAPDEV" ]`：只要二进制存在就跳过构建。
# 于是改了 hmapdev 源码后重新打包，用的仍是**旧工具链** ——
# 而日志看起来完全正常（每个包都"成功"）。
#
# 实测代价（2026-10-18）：给 Lua 打包路径加上了 skill 收集，
# 但 build/hmapdev 还是前一天编的，重建 22 个包后 luademo 里
# skill 条目仍然为 0。我一度以为修复没生效，实际是工具链没换。
#
# 现在：源码比二进制新就重编。
needs_build=0
if [ ! -x "$HMAPDEV" ]; then
  needs_build=1
elif [ -n "$(find "$SDK_ROOT/tools/hmapdev" -name '*.go' -newer "$HMAPDEV" -print -quit 2>/dev/null)" ]; then
  echo "[examples] hmapdev 源码比二进制新，重新构建 ..."
  needs_build=1
fi
if [ "$needs_build" = "1" ]; then
  echo "[examples] 先构建 hmapdev ..."
  ( cd "$SDK_ROOT/tools/hmapdev" && "$GO" build -o "$HMAPDEV" . ) || {
    echo "[examples] hmapdev 构建失败，无法继续" >&2; exit 1; }
fi
if [ ! -x "$HMAPDEV" ]; then
  echo "[examples] hmapdev 不存在或不可执行：$HMAPDEV" >&2
  exit 1
fi

echo "=== 协议 ==="
echo "  ProtocolVersion = $(grep -m1 '^const ProtocolVersion' "$SDK_ROOT/../internal/plugin/proc/protocol.go" 2>/dev/null | grep -oE '[0-9]+' || echo '?（本仓非内核仓，跳过）')"

mkdir -p "$OUT_DIR"
# 清掉上一次的校验和：残留的 SHA256SUMS 会掩盖本次缺产物。
rm -f "$OUT_DIR"/SHA256SUMS "$OUT_DIR"/MANIFEST.txt

ok=0
fail=0
failed_names=""

for dir in "$SDK_ROOT"/example/*/; do
  # ★ 入口判据：Go 插件是 plugin.go，**Lua 插件是 main.lua**。
  #
  # 原判据只有 `[ -f plugin.go ]` ⇒ **Lua 插件被静默跳过**
  # （实测：luademo 是 Lua 插件，entry=main.lua，不产 .hmap 也不报错，
  #   发版产物就少一个插件而没人发现）。改为认两种入口。
  #
  # 用 plg.json 的 entry 做最终判定更准，但**入口文件存在性**足以区分
  # "这是不是一个插件工程"；真正的入口名由 hmapdev 从 plg.json 读。
  if [ ! -f "$dir/plugin.go" ] && [ ! -f "$dir/main.lua" ]; then
    continue
  fi
  name="$(basename "$dir")"

  # 清掉旧产物：残留会让人（和本脚本）误判成功。
  rm -rf "$dir/build" "$dir/dist"

  out=$( cd "$dir" && "$HMAPDEV" build --no-bundle --target "$GOOS/$GOARCH" 2>&1 )
  rc=$?

  # 判据是**退出码 + 产物存在**，两者都要。
  #   只看退出码：hmapdev 曾经出错也退 0（已修，但脚本不该依赖它「现在」是对的）。
  #   只看产物：部分平台失败时会留下上一次的产物，看起来像成功。
  #
  # ★★ 必须挑**本次目标平台**的产物，不能取 `head -1`。
  #
  # ## 为什么（实测缺陷，2026-10-17）
  #
  # `--target` 的语义是**追加**（docs/guide/multi-platform.md 明确写「追加一个目标」），
  # 而示例的 plg.json 普遍声明 `targets: "linux/amd64"`。于是
  # `build --no-bundle --target linux/arm64` 会**同时**产出
  # `..._linux_amd64.hmap` 与 `..._linux_arm64.hmap`。
  #
  # 原代码取 `ls | head -1` ⇒ **按文件名字典序拿到 amd64 那份**
  # ⇒ 打 arm64 包时实际装进去的是 **x86-64 二进制**，
  # 而文件名却写着 `linux_arm64_...`（**文件名与内容不符**）。
  # 实测：arm64 产物 `plugin.bin` 的 ELF machine = 0x003e（x86-64，应为 0x00b7=aarch64）。
  # 这种包在 arm64 机器上只会以 "Exec format error" 失败，而排查方向会被文件名带偏。
  #
  # ⇒ 精确匹配 `<name>_<goos>_<goarch>.hmap`；匹配不到则视为失败（不静默退回任意产物）。
  hmap="$(ls "$dir"/dist/*_"${GOOS}_${GOARCH}".hmap 2>/dev/null | head -1)"
  if [ -z "$hmap" ]; then
    # Lua 插件的产物名是 <name>_lua.hmap（无平台后缀，Lua 不编译）。
    hmap="$(ls "$dir"/dist/*_lua.hmap 2>/dev/null | head -1)"
  fi
  if [ $rc -eq 0 ] && [ -n "$hmap" ]; then
    # 保留插件自己声明的产物名（它用的是 plg.json 的 name_en，是插件的身份），
    # 只在前面加平台前缀避免多平台互相覆盖。
    dest="$OUT_DIR/${GOOS}_${GOARCH}_$(basename "$hmap")"
    cp "$hmap" "$dest"
    printf "✓ %-14s → %s (%s)\n" "$name" "$(basename "$dest")" "$(du -h "$dest" | cut -f1)"
    ok=$((ok + 1))
  else
    printf "✗ %-14s 构建失败 (rc=%d)\n" "$name" "$rc"
    echo "$out" | tail -6 | sed 's/^/      /'
    fail=$((fail + 1))
    failed_names="$failed_names $name"
  fi
done

echo
echo "示例产物: 成功 $ok / 失败 $fail"
[ -n "$failed_names" ] && echo "失败:$failed_names"

# 有失败就不算发版闭环：宁可整个中断，也不要发出「少几个插件」的包。
if [ $fail -ne 0 ]; then
  echo "[examples] 有示例构建失败，不生成 SHA256SUMS" >&2
  exit 1
fi

# 2) 全部产物齐了才算校验和。
( cd "$OUT_DIR" && sha256sum ./*.hmap > SHA256SUMS )

VERSION="${VERSION:-$(git -C "$SDK_ROOT" describe --tags --dirty 2>/dev/null || echo unknown)}"
COMMIT="${COMMIT:-$(git -C "$SDK_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
{
  echo "sdk_version: $VERSION"
  echo "sdk_commit:  $COMMIT"
  echo "target:      $GOOS/$GOARCH"
  echo "plugins:     $ok"
  echo "built_at:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo
  echo "这些 .hmap 与本版 SDK 的插件协议绑定，必须与同版本内核配套安装。"
  echo "校验：sha256sum -c SHA256SUMS"
} > "$OUT_DIR/MANIFEST.txt"

echo "[examples] 产物: $OUT_DIR"
echo "[examples] 清单: $OUT_DIR/MANIFEST.txt"
echo "[examples] 校验: $OUT_DIR/SHA256SUMS"
