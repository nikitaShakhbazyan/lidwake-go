#!/bin/bash
#
# Installs the latest lidwake release:
#
#   curl -fsSL https://raw.githubusercontent.com/nikitaShakhbazyan/lidwake-go/main/install.sh | bash
#
# Downloads the universal binary (Apple Silicon + Intel), checks its SHA-256 against the
# release's checksums.txt, then runs `lidwake setup`, which asks for your password once to
# install the root helper. Set LIDWAKE_VERSION=0.1.0 to pin a version.

set -euo pipefail

REPO=nikitaShakhbazyan/lidwake-go

fail() {
	echo "lidwake install: $*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || fail "lidwake runs on macOS only"

version=${LIDWAKE_VERSION:-}
if [ -z "$version" ]; then
	latest=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null) ||
		fail "no release published yet at https://github.com/$REPO/releases — install with Go instead:
  go install github.com/$REPO/cmd/lidwake@latest && \"\$(go env GOPATH)/bin/lidwake\" setup"
	version=$(printf '%s\n' "$latest" | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
fi
[ -n "$version" ] || fail "could not read the latest release version"

archive="lidwake_${version}_darwin_all.tar.gz"
base="https://github.com/$REPO/releases/download/v$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "==> downloading lidwake $version"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "download failed: $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: checksums.txt"

echo "==> verifying checksum"
(cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c - >/dev/null) || fail "checksum mismatch"

tar -xzf "$tmp/$archive" -C "$tmp"
"$tmp/lidwake" setup
