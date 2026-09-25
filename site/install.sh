#!/bin/sh
# Agent Tincan installer.
#
#   curl -fsSL https://agenttincan.com/install.sh | sh
#
# Downloads the newest tincan release for this machine from GitHub, checks it
# against the release's checksums.txt, and installs it to
# ${TINCAN_INSTALL_DIR:-$HOME/.local/bin}/tincan. It never uses sudo.
#
# Environment:
#   TINCAN_VERSION      release tag to install (for example v0.5.0); default newest
#   TINCAN_INSTALL_DIR  where to put tincan; default $HOME/.local/bin
#
# TINCAN_RELEASES_API and TINCAN_DOWNLOAD_BASE override the GitHub URLs; they
# exist for the installer's tests.

set -eu

REPO_URL="https://github.com/mvanhorn/agent-tincan"
RELEASES_API="${TINCAN_RELEASES_API:-https://api.github.com/repos/mvanhorn/agent-tincan/releases?per_page=1}"
DOWNLOAD_BASE="${TINCAN_DOWNLOAD_BASE:-$REPO_URL/releases/download}"
QUICKSTART_URL="$REPO_URL/blob/main/docs/quickstart.md"

say() {
	printf '%s\n' "$*"
}

die() {
	printf 'tincan install: %s\n' "$*" >&2
	exit 1
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# fetch URL FILE downloads URL to FILE, failing on any HTTP error.
fetch() {
	if have curl; then
		curl -fsSL --proto '=https,http' -o "$2" "$1"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

unreachable() {
	die "could not download $1
The Agent Tincan repository may not be public yet (GitHub answers HTTP 404
until it is); downloads open when the repository is public. Check your
network, or download manually from $REPO_URL/releases"
}

sha256() {
	if have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	else
		die "shasum or sha256sum is required to verify the download"
	fi
}

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) unsupported "$os" "$arch" ;;
	esac
	case "$arch" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) unsupported "$os" "$arch" ;;
	esac
	# A shell under Rosetta reports x86_64 on Apple silicon; use the native build.
	if [ "$os" = darwin ] && [ "$arch" = amd64 ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
		arch=arm64
	fi
}

unsupported() {
	die "no prebuilt tincan for $1 $2.
Builds exist for macOS (Apple silicon and Intel) and Linux (x86-64 and ARM64).
Build from source instead: clone $REPO_URL and run make build (Go 1.26 or newer)."
}

latest_tag() {
	fetch "$RELEASES_API" "$tmp/releases.json" || unreachable "$RELEASES_API"
	tr ',' '\n' <"$tmp/releases.json" |
		grep '"tag_name"' |
		head -n 1 |
		sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/'
}

main() {
	detect_platform
	asset="tincan_${os}_${arch}"

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t tincan)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	tag="${TINCAN_VERSION:-}"
	if [ -z "$tag" ]; then
		tag=$(latest_tag)
		[ -n "$tag" ] || die "no releases found at $RELEASES_API
Download manually from $REPO_URL/releases"
	fi
	case "$tag" in
	[0-9]*) tag="v$tag" ;;
	esac

	say "Installing tincan $tag ($asset)"
	fetch "$DOWNLOAD_BASE/$tag/$asset" "$tmp/$asset" || unreachable "$DOWNLOAD_BASE/$tag/$asset"
	fetch "$DOWNLOAD_BASE/$tag/checksums.txt" "$tmp/checksums.txt" || unreachable "$DOWNLOAD_BASE/$tag/checksums.txt"

	want=$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
	[ -n "$want" ] || die "checksums.txt for $tag has no entry for $asset"
	got=$(sha256 "$tmp/$asset")
	if [ "$got" != "$want" ]; then
		rm -rf "$tmp"
		die "checksum mismatch for $asset: expected $want, got $got. Aborting; nothing was installed."
	fi

	dir="${TINCAN_INSTALL_DIR:-$HOME/.local/bin}"
	mkdir -p "$dir" || die "cannot create $dir; set TINCAN_INSTALL_DIR to a directory you can write"
	dest="$dir/tincan"
	staged="$dir/.tincan.install.$$"
	cp "$tmp/$asset" "$staged" || die "cannot write to $dir; set TINCAN_INSTALL_DIR to a directory you can write"
	chmod +x "$staged"
	if [ "$os" = darwin ] && have xattr; then
		xattr -d com.apple.quarantine "$staged" >/dev/null 2>&1 || true
	fi
	if ! mv -f "$staged" "$dest"; then
		rm -f "$staged"
		die "cannot replace $dest"
	fi

	say "Installed $dest"
	"$dest" version || die "$dest did not run"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*)
		say ""
		say "$dir is not on your PATH. Add it, for example:"
		say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile   # or ~/.zshrc, ~/.bashrc"
		say "  export PATH=\"$dir:\$PATH\""
		;;
	esac

	say ""
	say "Next steps:"
	say "  Easiest: tell your agent \"Follow https://agenttincan.com/agents.txt\""
	say "  On your always-on machine, start the relay (it is also your admin):"
	say "    tincan relay"
	say "  On each agent's machine, join with the code from tincan invite:"
	say "    tincan join ABCD-EFGH --relay http://tincan-relay"
	say "  Website:     https://agenttincan.com"
	say "  Quick start: $QUICKSTART_URL"
}

main "$@"
