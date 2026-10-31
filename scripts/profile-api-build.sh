#!/usr/bin/env bash
set -euo pipefail
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) echo 'Unsupported native architecture.' >&2; exit 1 ;; esac
case "$os_label/$arch_label" in
  Linux/x86_64) rust_target=x86_64-unknown-linux-musl ;;
  Linux/arm64) rust_target=aarch64-unknown-linux-musl ;;
  Darwin/x86_64) rust_target=x86_64-apple-darwin ;;
  Darwin/arm64) rust_target=aarch64-apple-darwin ;;
  *) echo 'The API supports Linux and macOS.' >&2; exit 1 ;;
esac
rustup target add --toolchain 1.94.0 "$rust_target"
if [ "$os_label" = Linux ]; then
  export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc
  export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER=musl-gcc
fi
# Build the committed UI export into each native API. The source drift check
# separately rebuilds that export; release artifacts need no Node installation.
cargo build --locked --manifest-path api/Cargo.toml --release --target "$rust_target"
mkdir -p .artifacts/native-api
cp "${CARGO_TARGET_DIR:-api/target}/$rust_target/release/ormos-api" ".artifacts/native-api/ormos-api_${os_label}_${arch_label}"
python3 - "$os_label" "$arch_label" <<'PY'
import hashlib,json,pathlib,subprocess,sys
root=pathlib.Path('.artifacts/native-api')
name='ormos-api_'+'_'.join(sys.argv[1:])
source=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
(root/(name+'.json')).write_text(json.dumps({'schema':1,'source':source,'sha256':hashlib.sha256((root/name).read_bytes()).hexdigest()},sort_keys=True)+'\n')
PY
