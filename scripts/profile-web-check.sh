#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
cd ui
npm ci
npm run typecheck
npm test
npm run build
cd ..
if ! git diff --exit-code --stat internal/ui/dist; then
  echo "::error::internal/ui/dist is not what ui/ produces. Run npm ci && npm run build inside ui/ and commit the result."
  exit 1
fi

