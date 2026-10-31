#!/usr/bin/env bash
set -euo pipefail
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

cargo test --workspace --all-features --locked
cargo build --locked --bin ormos
ORMOS_CLI="${CARGO_TARGET_DIR:-target}/debug/ormos" python3 -m unittest discover -s tests -p 'test_cli.py' -v
