#!/bin/sh
# Logbook installer for macOS and Linux. No sudo needed: everything goes in
# your home directory.
#
#   curl -fsSL https://raw.githubusercontent.com/apneduniya/logbook/main/scripts/install.sh | LOGBOOK_API_KEY=<key> sh
#
# Any arguments are passed to `logbook install`, for example:
#
#   curl -fsSL .../install.sh | sh -s -- --key <key> --harness none
#
# Passing the key in LOGBOOK_API_KEY keeps it out of the process list.
# LOGBOOK_VERSION picks a release tag (default: the latest release).

set -eu

REPO="apneduniya/logbook"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'logbook installer: %s\n' "$*" >&2
	exit 1
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	*) fail "unsupported OS $(uname -s); on Windows use install.ps1" ;;
	esac
}

detect_arch() {
	arch=$(uname -m)
	# An Intel shell under Rosetta on Apple silicon: install the native build.
	if [ "$1" = darwin ] && [ "$arch" = x86_64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
	case "$arch" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) fail "unsupported CPU $arch (Logbook ships amd64 and arm64)" ;;
	esac
}

download() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 300 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q --https-only --tries=3 --timeout=60 -O "$2" "$1"
	else
		fail "neither curl nor wget is installed"
	fi
}

sha256() { # file
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 -r "$1" | awk '{print $1}'
	else
		fail "no SHA-256 tool found (sha256sum, shasum or openssl)"
	fi
}

main() {
	if [ "$(id -u)" = 0 ]; then
		fail "don't run this as root or with sudo: Logbook installs for your own user"
	fi
	os=$(detect_os)
	arch=$(detect_arch "$os")
	asset="logbook_${os}_${arch}"

	version="${LOGBOOK_VERSION:-latest}"
	if [ "$version" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$version"
	fi

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t logbook)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	say "Downloading Logbook ($version, $os/$arch)..."
	download "$base/$asset" "$tmp/$asset"
	download "$base/checksums.txt" "$tmp/checksums.txt"

	expected=$(awk -v n="$asset" '$2 == n || $2 == "*" n { print $1 }' "$tmp/checksums.txt")
	[ -n "$expected" ] || fail "$asset is missing from checksums.txt"
	actual=$(sha256 "$tmp/$asset")
	if [ "$(printf '%s' "$expected" | tr 'A-F' 'a-f')" != "$actual" ]; then
		fail "checksum mismatch for $asset (expected $expected, got $actual); not installing"
	fi
	say "Checksum verified."

	chmod 0755 "$tmp/$asset"
	"$tmp/$asset" install "$@"
}

main "$@"
