#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
export VERSION="${VERSION:-dev}"
set -euo pipefail
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
tar -xzf dist/ormos_Linux_x86_64.tar.gz -C "$tmp"
got="$("$tmp/ormos" --version)"
echo "$got"

# `ormos --version` prints the bare version and nothing else, so this is
# an exact string match rather than a prefix. A requested version must
# be reported exactly -- that is a release, and the archive claiming a
# different version than the tag is the whole failure this guards.
#
# The default is not checked against the literal "dev". main.go falls
# back to the module version in its build info when nothing was baked
# in, so a pull request can legitimately report a pseudo-version.
# Asserting "dev" here would be the check being wrong rather than the
# binary.
if [ "$VERSION" != "dev" ]; then
  if [ "$got" != "$VERSION" ]; then
    echo "::error::built binary reports '$got', expected '$VERSION'"
    exit 1
  fi
elif [ -z "$got" ]; then
  echo "::error::built binary reported no version at all"
  exit 1
fi

protocol="$("$tmp/ormos" --protocol-version)"
echo "protocol $protocol"
if [ "$protocol" != "4" ]; then
  echo "::error::built binary advertises protocol '$protocol', expected the v4 release contract"
  exit 1
fi

