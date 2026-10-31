#!/usr/bin/env bash
set -euo pipefail
export VERSION="${VERSION:-dev}"
if [ "${ORMOS_BUILD_TARGETS:-native}" = all ]; then
  python3 scripts/package-cli.py all
else
  bash scripts/profile-api-build.sh
  python3 scripts/package-cli.py native
fi
