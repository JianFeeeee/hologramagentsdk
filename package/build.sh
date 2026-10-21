#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_DIR="${PROJECT_ROOT}/build"
VERSION="${VERSION:-$(git -C "$PROJECT_ROOT" describe --tags --dirty 2>/dev/null || echo "0.7.1")}"
GO="${GO:-$(command -v go 2>/dev/null || echo "/home/jianf/go1.26.5/go/bin/go")}"
# 宿主平台必须在**本脚本 export GOOS/GOARCH 之前**取定。
# 否则 `go env GOOS` 会返回被 export 的目标平台（此前 `build.sh all all`
# 就是因此拿 darwin 二进制在 linux 上跑，报 cannot execute binary file）。
NATIVE_GOOS="$(env -u GOOS -u GOARCH "$GO" env GOOS 2>/dev/null || uname -s | tr 'A-Z' 'a-z')"
NATIVE_GOARCH="$(env -u GOOS -u GOARCH "$GO" env GOARCH 2>/dev/null || uname -m)"
case "$NATIVE_GOARCH" in x86_64|amd64) NATIVE_GOARCH="amd64" ;; aarch64|arm64) NATIVE_GOARCH="arm64" ;; esac
case "$NATIVE_GOOS" in darwin|linux|windows) ;; *) NATIVE_GOOS="linux" ;; esac
GOCACHE="${GOCACHE:-}"
GOPATH="${GOPATH:-}"

TARGET="${1:-native}"
COMPONENT="${2:-all}"

case "$TARGET" in
  native)   GOOS="" GOARCH="" ;;
  linux/amd64)  GOOS=linux   GOARCH=amd64 ;;
  linux/arm64)  GOOS=linux   GOARCH=arm64 ;;
  darwin/amd64) GOOS=darwin  GOARCH=amd64 ;;
  darwin/arm64) GOOS=darwin  GOARCH=arm64 ;;
  windows/amd64) GOOS=windows GOARCH=amd64 ;;
  all)
    "$0" linux/amd64   "$COMPONENT"
    "$0" linux/arm64   "$COMPONENT"
    "$0" darwin/amd64  "$COMPONENT"
    "$0" darwin/arm64  "$COMPONENT"
    "$0" windows/amd64 "$COMPONENT"
    exit 0
    ;;
  *)
    echo "Unknown target: $TARGET"
    echo "Usage: $0 [native|linux/amd64|linux/arm64|darwin/amd64|darwin/arm64|windows/amd64|all] [all|hmapdev|examples]"
    exit 1
esac

if [ -n "${GOOS:-}" ]; then
  SUFFIX="${GOOS}_${GOARCH}"
  export GOOS GOARCH
fi
export CGO_ENABLED=0
[ -n "$GOCACHE" ] && export GOCACHE
[ -n "$GOPATH" ] && export GOPATH

mkdir -p "$BUILD_DIR"

build_hmapdev() {
  local src="tools/hmapdev"
  local out="$BUILD_DIR/hmapdev${SUFFIX:+_$SUFFIX}"
  if [ "$GOOS" = "windows" ]; then out="${out}.exe"; fi

  echo "[BUILD] hmapdev ${GOOS:-linux}/${GOARCH:-amd64} → $out"
  cd "$PROJECT_ROOT/$src"
  "$GO" build -trimpath -ldflags "-X github.com/JianFeeeee/hologramagentsdk/meta.Version=${VERSION}" \
    -o "$out" .
  echo "  OK ($(du -h "$out" | cut -f1))"
  cd "$PROJECT_ROOT"
}

# 示例插件产物随 SDK 一起发。
#
# 为什么必须发：插件二进制与内核是**协议绑定**的（ProtocolVersion + 统一共享
# 内存区魔数）。SDK 升版常伴随协议变化，只发工具链不发示例产物，使用者很可能
# 拿旧产物去装，表现是握手失败（魔数不匹配）——看起来像「插件坏了」而不是
# 「版本不配套」。
#
# 用**宿主可执行**的那把工具链（而非 PATH 里的），保证产物与本次发版同源。
#
# 为什么不能用目标平台的那把：示例的跨平台构建是由 hmapdev 的 `--target GOOS/GOARCH`
# 完成的，被执行的进程本身必須能在当前机器上跑。拿目标平台的二进制去跑只会得到
# “cannot execute binary file: Exec format error”（`build.sh all all` 在 darwin 处断过）。
build_examples() {
  local dev
  dev="$BUILD_DIR/hmapdev_${NATIVE_GOOS}_${NATIVE_GOARCH}"
  [ "$NATIVE_GOOS" = "windows" ] && dev="${dev}.exe"
  # 宿主工具链缺失时先补建（`all` 的第一个目标可能不是宿主平台）。
  if [ ! -x "$dev" ]; then
    echo "[BUILD] 先补建宿主工具链 ${NATIVE_GOOS}/${NATIVE_GOARCH}（示例的跨平台由 --target 完成）"
    ( unset GOOS GOARCH; bash "$0" "${NATIVE_GOOS}/${NATIVE_GOARCH}" hmapdev ) || return 1
  fi
  if [ ! -x "$dev" ]; then
    echo "[BUILD] 无法构建示例：缺少宿主可执行的工具链 $dev" >&2
    echo "        先跑： $0 ${NATIVE_GOOS}/${NATIVE_GOARCH} hmapdev" >&2
    return 1
  fi
  echo "[BUILD] example plugins ${GOOS:-linux}/${GOARCH:-amd64} → $BUILD_DIR/examples（用 ${NATIVE_GOOS}/${NATIVE_GOARCH} 的工具链交叉构建）"
  PLUGINDEV="$dev" VERSION="$VERSION" bash "$PROJECT_ROOT/package/build-examples.sh" "$TARGET" "$BUILD_DIR/examples"
  echo "  OK"
}

case "$COMPONENT" in
  all)
    # 工具链必须先建完：示例用它来构建（同源保证协议一致）。
    build_hmapdev
    build_examples
    ;;
  hmapdev) build_hmapdev ;;
  examples)  build_examples ;;
  *)
    echo "Unknown component: $COMPONENT"
    exit 1
    ;;
esac
