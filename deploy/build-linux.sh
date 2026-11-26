#!/usr/bin/env bash
# 在开发机构建 Linux 单二进制，产出 dist/ 下的发布件与校验和。
# 用法：./deploy/build-linux.sh [版本号]
set -euo pipefail

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/dist"

cd "$ROOT"
mkdir -p "$OUT"

echo "==> 版本: $VERSION"
for target in linux/amd64 linux/arm64; do
  os="${target%/*}"
  arch="${target#*/}"
  name="jobworkspace-${os}-${arch}"
  echo "==> 构建 $name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o "$OUT/$name" .
done

cd "$OUT"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum jobworkspace-* > sha256sums.txt
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 jobworkspace-* > sha256sums.txt
fi

echo
echo "==> 产物"
ls -lh "$OUT"
echo
cat "$OUT/sha256sums.txt" 2>/dev/null || true
