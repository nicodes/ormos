#!/usr/bin/env bash
set -euo pipefail
# The reusable SDK remains portable; CLI supervision is Linux/macOS only.
rustup target add --toolchain 1.94.0 x86_64-pc-windows-gnu
cargo check --locked -p ormos-relay --all-features --target x86_64-pc-windows-gnu
python3 - <<'CHECK'
import subprocess
result=subprocess.run(['cargo','check','--locked','--bin','ormos','--target','x86_64-pc-windows-gnu'],text=True,capture_output=True)
assert result.returncode != 0
assert 'The ormos CLI supports Linux and macOS only' in result.stderr,result.stderr
print('Windows SDK compiles; unsupported CLI platform is explicitly refused')
CHECK
