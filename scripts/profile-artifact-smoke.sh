#!/usr/bin/env bash
set -euo pipefail
export VERSION="${VERSION:-dev}"
set -euo pipefail
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
os_label="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) arch_label=x86_64 ;; aarch64|arm64) arch_label=arm64 ;; *) exit 1 ;; esac
tar -xzf "dist/ormos_${os_label}_${arch_label}.tar.gz" -C "$tmp"
got="$("$tmp/ormos" --version)"
echo "$got"

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


"$tmp/ormos" ui --help | grep -F "local workspace"
