#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
govulncheck ./...
