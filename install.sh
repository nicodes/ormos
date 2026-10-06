#!/bin/sh
# Install a published Ormos binary; no Go or Node toolchain is needed.
set -eu

release=latest
install_dir=${ORMOS_INSTALL_DIR:-"$HOME/.local/bin"}
repo_url=https://github.com/nicodes/ormos
work_dir=
stage_dir=

fail() { printf 'ormos: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: install.sh [--version vX.Y.Z] [--install-dir /absolute/path]

Defaults: latest published release, ~/.local/bin.
Run this command again to update. No services or Tailscale routes are started.
HELP
}
cleanup() {
    [ -z "$work_dir" ] || rm -rf "$work_dir"
    [ -z "$stage_dir" ] || rm -rf "$stage_dir"
}
trap cleanup 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version|--install-dir)
            [ "$#" -ge 2 ] || fail "$1 needs a value."
            case "$1" in
                --version) release=$2 ;;
                --install-dir) install_dir=$2 ;;
            esac
            shift 2
            ;;
        --help|-h) usage; exit 0 ;;
        *) fail "Unknown option: $1 (try --help)." ;;
    esac
done
case "$install_dir" in /*) ;; *) fail 'The install directory must be an absolute path.' ;; esac

for tool in curl tar mktemp awk grep; do
    command -v "$tool" >/dev/null 2>&1 || fail "Install $tool first."
done
if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
else
    fail 'A SHA-256 utility (sha256sum or shasum) is required.'
fi

case "$(uname -s)" in
    Linux) platform=Linux ;;
    Darwin) platform=Darwin ;;
    *) fail 'Supported operating systems: Linux and macOS.' ;;
esac
case "$(uname -m)" in
    x86_64|amd64) arch=x86_64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) fail 'Supported CPU architectures: x86_64 and arm64.' ;;
esac
# Prefer the native Apple Silicon build when invoked from a Rosetta shell.
if [ "$platform" = Darwin ] && [ "$arch" = x86_64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
    arch=arm64
fi

fetch() {
    curl --fail --silent --show-error --location --retry 3 \
        --connect-timeout 10 --max-time 300 --proto '=https' --proto-redir '=https' "$@"
}
if [ "$release" = latest ]; then
    # Resolve GitHub's redirect instead of depending on its anonymous API quota.
    latest_url=$(fetch --output /dev/null --write-out '%{url_effective}' "$repo_url/releases/latest") || fail 'Could not find the latest release.'
    case "$latest_url" in
        "$repo_url/releases/tag/"*) release=${latest_url##*/} ;;
        *) fail 'No published release was found.' ;;
    esac
fi
case "$release" in v*) ;; *) release=v$release ;; esac
printf '%s\n' "$release" | LC_ALL=C grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || fail 'Use a release version such as v0.2.0.'

asset=ormos_${platform}_${arch}.tar.gz
base=$repo_url/releases/download/$release
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/ormos-install.XXXXXXXX") || fail 'Could not create a temporary directory.'
printf 'Installing Ormos %s (%s %s)...\n' "$release" "$platform" "$arch"
fetch "$base/checksums.txt" --output "$work_dir/checksums.txt" || fail "Could not download checksums for $release. Check that the release is published."
fetch "$base/$asset" --output "$work_dir/$asset" || fail "Could not download $asset for $release."
expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { count++; digest=$1 } END { if (count == 1) print digest }' "$work_dir/checksums.txt")
printf '%s\n' "$expected" | LC_ALL=C grep -Eq '^[a-fA-F0-9]{64}$' || fail 'Release checksums do not contain one valid entry for this archive.'
if [ "$checksum_tool" = sha256sum ]; then
    actual=$(sha256sum "$work_dir/$asset" | awk '{print $1}')
else
    actual=$(shasum -a 256 "$work_dir/$asset" | awk '{print $1}')
fi
[ "$actual" = "$expected" ] || fail 'Checksum verification failed; the installed binary was not changed.'
tar -xzf "$work_dir/$asset" -C "$work_dir" ormos || fail 'The release archive could not be extracted.'
[ -f "$work_dir/ormos" ] && [ ! -L "$work_dir/ormos" ] || fail 'The archive does not contain a regular Ormos executable.'
chmod 755 "$work_dir/ormos"
reported=$("$work_dir/ormos" --version) || fail 'The downloaded binary cannot run on this machine.'
[ "$reported" = "${release#v}" ] || fail "The binary reports $reported, expected ${release#v}; installation was not changed."

mkdir -p "$install_dir" || fail "Could not create $install_dir; choose a writable directory with --install-dir."
[ ! -d "$install_dir/ormos" ] || fail "$install_dir/ormos is a directory."
# Stage on the destination filesystem so replacement is atomic, including when
# Ormos is running. The running process continues using its original binary.
stage_dir=$(mktemp -d "$install_dir/.ormos-install.XXXXXXXX") || fail "Cannot write to $install_dir."
cp "$work_dir/ormos" "$stage_dir/ormos"
chmod 755 "$stage_dir/ormos"
mv -f "$stage_dir/ormos" "$install_dir/ormos"
printf 'Installed Ormos %s at %s/ormos\n' "$release" "$install_dir"
add_user_bin_to_profile() {
    profile=$1
    marker='# Ormos installer: user executables'
    if [ -f "$profile" ] && grep -F "$marker" "$profile" >/dev/null 2>&1; then return; fi
    if ! cat >> "$profile" <<'PROFILE'

# Ormos installer: user executables
case ":$PATH:" in
    *":$HOME/.local/bin:"*) ;;
    *) export PATH="$HOME/.local/bin:$PATH" ;;
esac
PROFILE
    then
        printf 'Could not update %s; add ~/.local/bin to your PATH manually.\n' "$profile" >&2
    fi
}
case ":$PATH:" in
    *":$install_dir:"*) printf 'Start with: ormos ui\n' ;;
    *)
        # Configure only the standard user directory. Custom directories stay
        # under the caller's control, and paths never become executable text.
        if [ "$install_dir" = "$HOME/.local/bin" ]; then
            case "${SHELL:-/bin/sh}" in
                */zsh)
                    add_user_bin_to_profile "$HOME/.zshrc"
                    add_user_bin_to_profile "$HOME/.zprofile"
                    ;;
                */bash)
                    add_user_bin_to_profile "$HOME/.bashrc"
                    if [ -f "$HOME/.bash_profile" ]; then
                        add_user_bin_to_profile "$HOME/.bash_profile"
                    else
                        add_user_bin_to_profile "$HOME/.profile"
                    fi
                    ;;
                */sh) add_user_bin_to_profile "$HOME/.profile" ;;
            esac
        fi
        printf 'Start now with: "%s/ormos" ui\nOpen a new terminal to pick up PATH changes. Custom install directories must be on your PATH.\n' "$install_dir"
        ;;
esac
