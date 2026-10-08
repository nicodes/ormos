#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local

# relay is imported by the hosted backend and must stay portable.
# This is the one thing here that is proven by compiling.
GOOS=windows go build ./relay/...

# Each target names its arch explicitly: several of these have no
# valid default, and `GOOS=x ${var} go list` does not work — bash
# recognises assignment prefixes before expansion, so the expanded
# word is run as a command. env takes them as arguments instead.
#
# android and ios are on this list because they are the two a plain
# `linux || darwin` tag SILENTLY INCLUDES: Go sets the linux tag on
# android and the darwin tag on ios, so the agent selected and built
# there — on kernels with a different security model, different PTY
# behaviour, and no CI at all. The tags say `(linux && !android) ||
# (darwin && !ios)` for that reason, and this is what holds them to it.
for target in \
  windows/amd64 freebsd/amd64 openbsd/amd64 netbsd/amd64 \
  solaris/amd64 dragonfly/amd64 illumos/amd64 aix/ppc64 \
  plan9/amd64 js/wasm wasip1/wasm android/arm64 ios/arm64; do
  goos="${target%/*}"
  goarch="${target#*/}"
  got="$(env GOOS="$goos" GOARCH="$goarch" go list ./... 2>/dev/null || true)"
  if [ "$got" != "github.com/nicodes/ormos/relay" ]; then
    echo "::error::$target selects more than relay -- something in the agent is buildable on a platform nothing tests:"
    echo "$got"
    exit 1
  fi
done

# Both supported platforms still build, so this can never pass by
# excluding everything.
GOOS=linux go build ./...
GOOS=darwin go build ./...
echo "linux and darwin build; every other platform selects relay alone"

