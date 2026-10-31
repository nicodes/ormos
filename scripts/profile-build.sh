#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local
export VERSION="${VERSION:-dev}"
rm -rf dist
mkdir -p dist
native_os="$(uname -s)"
case "$(uname -m)" in x86_64|amd64) native_arch=x86_64 ;; aarch64|arm64) native_arch=arm64 ;; *) echo 'Unsupported architecture.' >&2; exit 1 ;; esac
if [ "${ORMOS_BUILD_TARGETS:-native}" != all ]; then bash scripts/profile-api-build.sh; fi
build() {
  local goos="$1" goarch="$2" os_label="$3" arch_label="$4" name out payload
  name="ormos_${os_label}_${arch_label}"
  out="dist/$name"
  payload=".artifacts/native-api/ormos-api_${os_label}_${arch_label}"
  mkdir -p "$out"
  # Each Rust payload comes from a native builder at this exact source commit.
  # The Go CLI only embeds and launches it; no Go API business logic is linked.
  python3 - "$payload" <<'PY'
import gzip,hashlib,json,pathlib,subprocess,sys
path=pathlib.Path(sys.argv[1]);meta=json.loads(path.with_suffix('.json').read_text())
assert meta['source']==subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),'native API source mismatch'
raw=path.read_bytes();assert hashlib.sha256(raw).hexdigest()==meta['sha256'],'native API digest mismatch'
pathlib.Path('internal/nativeapi/payload.gz').write_bytes(gzip.compress(raw,mtime=0))
PY
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -tags rustapi \
    -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out/ormos" .
  cp README.md LICENSE "$out/"
  tar -C "$out" -czf "dist/$name.tar.gz" ormos README.md LICENSE
  echo "  $name"
}
trap 'rm -f internal/nativeapi/payload.gz' EXIT
if [ "${ORMOS_BUILD_TARGETS:-native}" = all ]; then
  build linux amd64 Linux x86_64
  build linux arm64 Linux arm64
  build darwin amd64 Darwin x86_64
  build darwin arm64 Darwin arm64
else
  case "$native_os/$native_arch" in
    Linux/x86_64) build linux amd64 Linux x86_64 ;;
    Linux/arm64) build linux arm64 Linux arm64 ;;
    Darwin/x86_64) build darwin amd64 Darwin x86_64 ;;
    Darwin/arm64) build darwin arm64 Darwin arm64 ;;
  esac
fi
cp install.sh dist/install.sh
(cd dist && python3 - <<'PY'
import hashlib,pathlib
paths=sorted(pathlib.Path('.').glob('*.tar.gz'))+[pathlib.Path('install.sh')]
pathlib.Path('checksums.txt').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+p.name+'\n' for p in paths))
PY
)
