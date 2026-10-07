#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
repo_root="$(pwd)"
cd ui
flags=()
if [ "${CI:-}" = true ]; then flags+=(--with-deps); fi
npx --no-install playwright install "${flags[@]}" chromium
ORMOS_TEST_BINARY="$repo_root/dist/ormos_Linux_x86_64/ormos" npm run test:browser

