#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
repo_root="$(pwd)"
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) exit 1 ;; esac
cd ui
flags=()
if [ "${CI:-}" = true ]; then flags+=(--with-deps); fi
npx --no-install playwright install "${flags[@]}" chromium
ORMOS_TEST_BINARY="$repo_root/dist/ormos_${os_label}_${arch_label}/ormos" npm run test:browser

