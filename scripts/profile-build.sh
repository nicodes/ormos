#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
export VERSION="${VERSION:-dev}"
set -euo pipefail
rm -rf dist
mkdir -p dist

build() {
  goos="$1"; goarch="$2"; os_label="$3"; arch_label="$4"
  name="ormos_${os_label}_${arch_label}"
  out="dist/$name"
  mkdir -p "$out"
  # CGO off so the binary is static and runs on any distro; -trimpath so
  # the archive does not carry the builder's paths.
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build \
    -trimpath -ldflags "-s -w -X main.version=$VERSION" \
    -o "$out/ormos" .
  cp README.md LICENSE "$out/"
  tar -C "$out" -czf "dist/$name.tar.gz" ormos README.md LICENSE
  echo "  $name"
}

build linux  amd64 Linux  x86_64
build linux  arm64 Linux  arm64
build darwin amd64 Darwin x86_64
build darwin arm64 Darwin arm64

cp install.sh dist/install.sh
(cd dist && sha256sum -- *.tar.gz install.sh > checksums.txt)

