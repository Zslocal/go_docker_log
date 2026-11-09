#!/usr/bin/env bash
# 交叉编译 linux/amd64 与 linux/arm64 静态二进制。
# 产物是纯静态 ELF（CGO_ENABLED=0），scp 到服务器就能跑，不挑 glibc 版本。
set -euo pipefail

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT="dist"
mkdir -p "$OUT"

build() {
  local arch=$1
  echo "==> linux/$arch  ${VERSION}"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o "${OUT}/dlog-linux-${arch}" ./cmd/dlog
}

build amd64
build arm64

echo
ls -lh "$OUT"
