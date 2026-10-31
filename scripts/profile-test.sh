#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) exit 1 ;; esac
archive="dist/ormos_${os_label}_${arch_label}.tar.gz"
binary="dist/ormos_${os_label}_${arch_label}/ormos"
if [ -f "$archive" ]; then
  export ORMOS_INSTALL_TEST_ARCHIVE="$PWD/$archive"
  ORMOS_INSTALL_TEST_VERSION="$("$binary" --version)"
  export ORMOS_INSTALL_TEST_VERSION
fi
python3 -m unittest discover -s tests -p 'test_install.py' -v

go test -race -count=1 ./...
