#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
if [ -f dist/ormos_Linux_x86_64.tar.gz ]; then
  export ORMOS_INSTALL_TEST_ARCHIVE="$PWD/dist/ormos_Linux_x86_64.tar.gz"
  ORMOS_INSTALL_TEST_VERSION="$(dist/ormos_Linux_x86_64/ormos --version)"
  export ORMOS_INSTALL_TEST_VERSION
fi
python3 -m unittest discover -s tests -p 'test_install.py' -v

go test -race -count=1 ./...
