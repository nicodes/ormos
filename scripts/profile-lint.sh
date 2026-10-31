#!/usr/bin/env bash
set -euo pipefail
shellcheck install.sh scripts/profile-*.sh
cargo fmt --all --check
cargo clippy -p ormos -p ormos-relay --all-features --all-targets --locked -- -D warnings
cargo fmt --manifest-path api/Cargo.toml --check
cargo clippy --locked --manifest-path api/Cargo.toml --all-targets -- -D warnings
