#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
cd app
npm run typecheck
npm test
npm run build
cd ..
python3 scripts/verify-ui-build.py
