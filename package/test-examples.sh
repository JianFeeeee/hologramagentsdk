#!/usr/bin/env bash
# 跑 example/*/ 下的单元测试。
#
# ## 为什么需要这个脚本（而不是在 ci.yml 里写一行 go test）
#
# `example/*/` 每个都是**独立 go module**（各有自己的 go.mod）。根模块的
# `go test ./...` 不会进入它们 —— 这是 Go 的模块边界，不是配置问题。
# 于是本仓 CI 从没跑过任何 example 测试：2026-10-21 实测，干净克隆下
# commondev 有 8 条红、recoverydiag 有 1 条红，而 CI 全绿 —— 因为根本没跑。
#
# 唯一正确的做法是**逐目录跑**。写进脚本而不是 YAML：让"哪些目录算有测试"、
# "EMPTY 怎么判"这些规则只有一处实现（YAML 里塞几十行 shell 既没有语法
# 检查，也与 package/build-examples.sh 的风格不一致）。
#
# ## 用法
#
#   package/test-examples.sh                  # 全部有测试的目录
#   package/test-examples.sh commondev qq     # 只跑指定目录
#
# 环境：
#   GO        go 可执行文件（默认 PATH 上的 go）
#   TIMEOUT   每个目录的超时（默认 10m，与 ci.yml 其它 go test 步骤一致）
#
# ## 判成功的口径（三条，缺一不可）
#
#   1. `go test` 退出码为 0
#   2. 该目录**至少有一条测试真的 PASS**（不是 0 条、不是全 skip）
#   3. 没有任何目录被静默跳过
#
# 第 2 条是这里最容易做错、也最要紧的一条：
# 「全 skip」和「全绿」在 `go test` 输出里**长得几乎一样**（都是 ok），
# 而含义完全相反。本仓真吃过这个亏（rawfile/ 不进版本库 ⇒ 相关判据在
# CI 上无条件 skip ⇒ 一片 ok）。所以这里显式数条数：
#
#     PASS 数 == 0            → EMPTY，非零退出（跑了但一条都没真跑）
#     PASS 数 > 0 且有 SKIP   → 通过，但把 skip 名单打进日志（让人看得见）
#
# ## 不加 -short
#
# -short 会让判据按"快"来裁剪，而不是按"该不该验"来裁剪。用 -short 换绿灯
# 是本末倒置：要么这条判据该跑，那就跑；要么它不该跑，那就删掉或明确 skip。
set -uo pipefail

SDK_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-$(command -v go 2>/dev/null || echo go)}"
TIMEOUT="${TIMEOUT:-10m}"

if ! command -v "$GO" >/dev/null 2>&1; then
  echo "error: 找不到 go（GO=$GO）" >&2
  exit 1
fi

# 选定要跑的目录：判据是「目录里有 *_test.go」。
#
# ★ 不按"有 go.mod"筛：24 个示例全都有 go.mod，但只有一部分有测试 ——
#   对没有测试的目录跑 `go test ./...` 只会输出 "no test files" 并退 0，
#   那是噪声而不是检查项。
# ★ 只认 example/<一层>/：示例里还有嵌套目录（如 examples/custom-cli/），
#   它们不是独立 module，不该被当成测试单元。
targets=()
if [ "$#" -gt 0 ]; then
  for name in "$@"; do
    d="$SDK_ROOT/example/$name"
    if [ ! -d "$d" ]; then
      echo "error: 没有这个示例目录: $d" >&2
      exit 1
    fi
    targets+=("$d")
  done
else
  for d in "$SDK_ROOT"/example/*/; do
    [ -d "$d" ] || continue
    if compgen -G "${d}*_test.go" >/dev/null; then
      targets+=("${d%/}")
    fi
  done
fi

if [ "${#targets[@]}" -eq 0 ]; then
  echo "error: 一个带 *_test.go 的示例目录都没找到（在 $SDK_ROOT/example/ 下）。" >&2
  echo "       这本身就说明筛选逻辑坏了，不能当成「没有测试要跑」而通过。" >&2
  exit 1
fi

echo "=== example 单元测试（逐 module 跑，共 ${#targets[@]} 个目录）==="
echo "    go:      $("$GO" version)"
echo "    timeout: $TIMEOUT"

fail=0
empty=0
failed_names=""
empty_names=""
summary=()

for d in "${targets[@]}"; do
  name="$(basename "$d")"
  printf '\n──── %s ────\n' "$name"

  # 只跑一次，且带 -v：
  #   -count=1  关掉结果缓存（缓存命中会打印 "(cached)" 并复用上次结果，
  #             而 CI 要的是"现在这份代码过不过"）
  #   -v        必须带：不带 -v 时 go 不打印每条结果，就数不出 PASS/SKIP 条数，
  #             而"跑了 0 条"与"全过"的退出码都是 0 —— 那正是假绿的来源。
  #             日志长一点是值得的：出问题时要看的就是这些。
  log="$(mktemp)"
  ( cd "$d" && "$GO" test ./... -count=1 -timeout "$TIMEOUT" -v ) >"$log" 2>&1
  rc=$?

  passes=$(grep -c '^--- PASS:' "$log")
  skips=$(grep -c '^--- SKIP:' "$log")
  fails=$(grep -c '^--- FAIL:' "$log")

  if [ "$rc" -ne 0 ]; then
    printf '  ❌ FAIL（rc=%s，失败 %s 条）\n' "$rc" "$fails"
    grep -E '^(--- FAIL|    [a-z0-9_]+_test\.go:)' "$log" | head -40 | sed 's/^/      /'
    fail=$((fail + 1))
    failed_names="$failed_names $name"
    summary+=("$(printf '%-14s FAIL  pass=%-3s skip=%-3s fail=%s' "$name" "$passes" "$skips" "$fails")")
    rm -f "$log"
    continue
  fi

  # ★ 关键判据：跑了但一条都没真跑 ⇒ EMPTY（假绿），不能让 ok 蒙过去。
  if [ "$passes" -eq 0 ]; then
    printf '  ❌ EMPTY：目录里有 *_test.go，但实际 PASS 0 条（skip=%s）\n' "$skips"
    echo "      这通常是判据被无条件 skip 掉了（例如它依赖的资源不在版本库里）。" >&2
    echo "      不是「没有测试」，而是测试没在验任何东西。" >&2
    grep -E '^--- SKIP:' "$log" | head -20 | sed 's/^/      /'
    empty=$((empty + 1))
    empty_names="$empty_names $name"
    summary+=("$(printf '%-14s EMPTY pass=0   skip=%-3s' "$name" "$skips")")
    rm -f "$log"
    continue
  fi

  printf '  ✅ PASS（pass=%s skip=%s）\n' "$passes" "$skips"
  if [ "$skips" -gt 0 ]; then
    # 不静默：skip 的名单必须出现在日志里。它们是"这次没验的东西"，
    # 而不是"验过且通过的东西" —— 两者混在一起就是假绿。
    printf '  ↷ 跳过 %s 条（这些**没有**被验证）：\n' "$skips"
    grep -E '^--- SKIP:' "$log" | sed 's/^/      /'
  fi
  summary+=("$(printf '%-14s PASS  pass=%-3s skip=%-3s' "$name" "$passes" "$skips")")
  rm -f "$log"
done

echo
echo "=== 汇总 ==="
for line in "${summary[@]}"; do
  echo "  $line"
done

echo
if [ "$fail" -ne 0 ] || [ "$empty" -ne 0 ]; then
  [ "$fail" -ne 0 ] && echo "[examples-tests] 失败:$failed_names" >&2
  [ "$empty" -ne 0 ] && echo "[examples-tests] 空跑（假绿）:$empty_names" >&2
  echo "[examples-tests] 不通过：有失败或空跑的目录（详见上方日志）" >&2
  exit 1
fi

echo "[examples-tests] 全部通过（${#targets[@]} 个目录）"
