#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
shellcheck install.sh scripts/profile-*.sh
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "::error::gofmt would change these files:"
  echo "$unformatted"
  exit 1
fi

go vet ./...
cargo fmt --manifest-path api/Cargo.toml --check
cargo clippy --locked --manifest-path api/Cargo.toml --all-targets -- -D warnings
