#!/usr/bin/env bash
#
# Browsh installer for Debian/Ubuntu servers (tested on Ubuntu Server 26.04).
#
# One-liner:
#   curl -fsSL https://raw.githubusercontent.com/jfspencer/browsh/master/install.sh | bash
#
# What it does:
#   1. Installs the few system packages Browsh needs (curl, procps, etc).
#   2. Installs Firefox if it's missing. By default this uses Mozilla's own APT
#      repository rather than Ubuntu's snap, because the snap sandbox makes
#      headless automation awkward. Browsh does work with the snap too.
#   3. Downloads a prebuilt Browsh binary from this repository's GitHub releases,
#      or builds one from source if there isn't a release for your architecture.
#   4. Installs it to /usr/local/bin/browsh.
#
# Tunables (environment variables):
#   BROWSH_REPO=owner/name        GitHub repo to install from  (default: jfspencer/browsh)
#   BROWSH_REF=master             Git ref to build if no release binary exists
#   BROWSH_INSTALL_DIR=/usr/local/bin
#   BROWSH_FIREFOX=auto|apt|snap|skip
#                                 How to install Firefox when it's missing (default: auto = apt)
#   BROWSH_FROM_SOURCE=1          Always build from source, never download a binary
#   BROWSH_SOURCE_DIR=/path       Build from a local checkout instead of cloning (for testing)
#   BROWSH_XPI_VERSION=1.8.3      Version of the signed webextension to embed when building
#
set -euo pipefail

BROWSH_REPO=${BROWSH_REPO:-jfspencer/browsh}
BROWSH_REF=${BROWSH_REF:-master}
BROWSH_INSTALL_DIR=${BROWSH_INSTALL_DIR:-/usr/local/bin}
BROWSH_FIREFOX=${BROWSH_FIREFOX:-auto}
BROWSH_FROM_SOURCE=${BROWSH_FROM_SOURCE:-0}
BROWSH_SOURCE_DIR=${BROWSH_SOURCE_DIR:-}
BROWSH_XPI_VERSION=${BROWSH_XPI_VERSION:-1.8.3}
# Used only if we have to fetch a Go toolchain before the source tree (and its
# go.mod) is available to tell us which version the build actually wants.
GO_FALLBACK_VERSION=${GO_FALLBACK_VERSION:-1.24.4}
UPSTREAM_REPO="browsh-org/browsh"

WORK_DIR=$(mktemp -d -t browsh-install.XXXXXX)
# Go writes its module cache read-only (0555 dirs, 0444 files), and you can't
# unlink entries out of a read-only directory. Without the chmod, a source build
# leaves the temp dir behind and buries the successful install under hundreds of
# "rm: Permission denied" lines.
cleanup() {
	[ -n "${WORK_DIR:-}" ] && [ -d "$WORK_DIR" ] || return 0
	chmod -R u+w "$WORK_DIR" 2>/dev/null || true
	rm -rf "$WORK_DIR"
}
trap cleanup EXIT

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWarning:\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Environment detection
# ---------------------------------------------------------------------------
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
	if command -v sudo >/dev/null 2>&1; then
		SUDO="sudo"
	else
		die "Run this script as root, or install sudo."
	fi
fi

OS_ID=""
OS_LIKE=""
if [ -r /etc/os-release ]; then
	# shellcheck disable=1091
	. /etc/os-release
	OS_ID=${ID:-}
	OS_LIKE=${ID_LIKE:-}
fi
IS_DEBIAN_LIKE=0
case " $OS_ID $OS_LIKE " in
*" debian "* | *" ubuntu "*) IS_DEBIAN_LIKE=1 ;;
esac

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7l) ARCH=armv7 ;;
armv6l) ARCH=armv6 ;;
i386 | i686) ARCH=386 ;;
*) die "Unsupported architecture: $(uname -m)" ;;
esac

log "Installing Browsh from $BROWSH_REPO on ${PRETTY_NAME:-$(uname -s)} ($ARCH)"

# ---------------------------------------------------------------------------
# System packages
# ---------------------------------------------------------------------------
apt_install() {
	$SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends "$@"
}

if [ "$IS_DEBIAN_LIKE" -eq 1 ]; then
	log "Installing base packages"
	$SUDO apt-get update -q
	# procps provides `ps`, which Browsh uses to check for stray Firefox processes.
	apt_install ca-certificates curl gnupg procps
else
	warn "Not a Debian/Ubuntu system. Firefox and build tools won't be installed automatically."
	command -v curl >/dev/null 2>&1 || die "curl is required"
fi

# ---------------------------------------------------------------------------
# Firefox
# ---------------------------------------------------------------------------
firefox_is_usable() {
	# Ubuntu ships /usr/bin/firefox as a stub that only nags you to install the
	# snap, so check that it actually runs rather than that it merely exists.
	command -v firefox >/dev/null 2>&1 && firefox --version >/dev/null 2>&1
}

install_firefox_apt() {
	log "Installing Firefox from Mozilla's APT repository"
	$SUDO install -d -m 0755 /etc/apt/keyrings
	curl -fsSL https://packages.mozilla.org/apt/repo-signing-key.gpg |
		$SUDO tee /etc/apt/keyrings/packages.mozilla.org.asc >/dev/null
	echo "deb [signed-by=/etc/apt/keyrings/packages.mozilla.org.asc] https://packages.mozilla.org/apt mozilla main" |
		$SUDO tee /etc/apt/sources.list.d/mozilla.list >/dev/null
	# Without this pin Ubuntu's transitional `firefox` package (which just installs
	# the snap) wins over Mozilla's real .deb.
	printf 'Package: *\nPin: origin packages.mozilla.org\nPin-Priority: 1000\n' |
		$SUDO tee /etc/apt/preferences.d/mozilla >/dev/null
	$SUDO apt-get update -q
	apt_install firefox
}

install_firefox_snap() {
	log "Installing Firefox snap"
	command -v snap >/dev/null 2>&1 || apt_install snapd
	$SUDO snap install firefox
}

if firefox_is_usable; then
	log "Firefox already installed: $(firefox --version 2>/dev/null | tail -n1)"
elif [ "$BROWSH_FIREFOX" = "skip" ]; then
	warn "Skipping Firefox installation as requested. Browsh needs Firefox 57+ to run."
elif [ "$IS_DEBIAN_LIKE" -ne 1 ]; then
	warn "Firefox not found. Please install Firefox 57 or newer manually."
else
	case "$BROWSH_FIREFOX" in
	auto | apt) install_firefox_apt ;;
	snap) install_firefox_snap ;;
	*) die "Unknown BROWSH_FIREFOX value: $BROWSH_FIREFOX (use auto, apt, snap or skip)" ;;
	esac
	firefox_is_usable || die "Firefox was installed but 'firefox --version' doesn't work."
	log "Firefox installed: $(firefox --version 2>/dev/null | tail -n1)"
fi

# ---------------------------------------------------------------------------
# Browsh binary: prebuilt release, or build from source
# ---------------------------------------------------------------------------
BINARY="$WORK_DIR/browsh"

download_release_binary() {
	local url="https://github.com/$BROWSH_REPO/releases/latest/download/browsh_linux_$ARCH"
	log "Looking for a prebuilt binary at $url"
	if curl -fsSL --retry 3 -o "$BINARY" "$url" && [ -s "$BINARY" ]; then
		chmod +x "$BINARY"
		return 0
	fi
	rm -f "$BINARY"
	return 1
}

go_version_ok() {
	# Go >= 1.21 auto-downloads a newer toolchain if go.mod asks for one, so
	# anything reasonably recent will do.
	command -v go >/dev/null 2>&1 || return 1
	local v
	v=$(go version | sed -E 's/.*go([0-9]+)\.([0-9]+).*/\1 \2/')
	local major=${v% *} minor=${v#* }
	[ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge 21 ]; }
}

ensure_go() {
	if go_version_ok; then
		return
	fi
	if [ "$IS_DEBIAN_LIKE" -eq 1 ]; then
		log "Installing Go from apt"
		apt_install golang-go
		hash -r
		go_version_ok && return
	fi
	# ensure_go is also reachable before SRC_DIR is assigned, and `set -u` turns
	# that into a crash rather than a fallback. Only read go.mod if it's there.
	local want=""
	if [ -r "${SRC_DIR:-}/interfacer/go.mod" ]; then
		want=$(sed -nE 's/^go ([0-9.]+).*/\1/p' "${SRC_DIR}/interfacer/go.mod")
	fi
	want=${want:-$GO_FALLBACK_VERSION}
	# Go publishes one 32-bit ARM build, named armv6l; there is no armv7/armv6.
	local go_arch=$ARCH
	case "$go_arch" in
	armv7 | armv6) go_arch=armv6l ;;
	esac
	local url="https://go.dev/dl/go${want}.linux-${go_arch}.tar.gz"
	log "Downloading Go $want from $url"
	curl -fsSL --retry 3 -o "$WORK_DIR/go.tar.gz" "$url" || die "Couldn't download Go. Install Go 1.21+ and re-run."
	mkdir -p "$WORK_DIR/goroot"
	tar -C "$WORK_DIR/goroot" --strip-components=1 -xzf "$WORK_DIR/go.tar.gz"
	export PATH="$WORK_DIR/goroot/bin:$PATH"
	go_version_ok || die "Downloaded Go toolchain doesn't work"
}

build_from_source() {
	if [ -n "$BROWSH_SOURCE_DIR" ]; then
		log "Building from local checkout $BROWSH_SOURCE_DIR"
		SRC_DIR="$WORK_DIR/src"
		mkdir -p "$SRC_DIR"
		tar -C "$BROWSH_SOURCE_DIR" --exclude=node_modules --exclude=.git -cf - . | tar -C "$SRC_DIR" -xf -
	else
		[ "$IS_DEBIAN_LIKE" -eq 1 ] && apt_install git
		command -v git >/dev/null 2>&1 || die "git is required to build from source"
		SRC_DIR="$WORK_DIR/src"
		log "Cloning https://github.com/$BROWSH_REPO ($BROWSH_REF)"
		git clone --quiet --depth 1 --branch "$BROWSH_REF" "https://github.com/$BROWSH_REPO.git" "$SRC_DIR"
	fi

	ensure_go
	log "Using $(go version)"

	local xpi="$SRC_DIR/interfacer/src/browsh/browsh.xpi"
	if [ ! -s "$xpi" ] || [ "$(wc -c <"$xpi")" -lt 500 ]; then
		local xpi_url="https://github.com/$UPSTREAM_REPO/releases/download/v$BROWSH_XPI_VERSION/browsh-$BROWSH_XPI_VERSION.xpi"
		log "Downloading signed webextension v$BROWSH_XPI_VERSION"
		curl -fsSL --retry 3 -o "$xpi" "$xpi_url" || die "Couldn't download the webextension from $xpi_url"
		[ "$(wc -c <"$xpi")" -ge 500 ] || die "Downloaded webextension looks too small, aborting"
	fi

	log "Compiling Browsh (this can take a minute or two on first run)"
	(
		cd "$SRC_DIR/interfacer"
		# Keep the build self-contained so nothing is left behind in $HOME.
		export GOPATH="$WORK_DIR/gopath" GOCACHE="$WORK_DIR/gocache" GOFLAGS=-mod=mod GOTOOLCHAIN=auto
		export CGO_ENABLED=0
		go build -trimpath -ldflags "-s -w" -o "$BINARY" ./cmd/browsh
	)
}

if [ "$BROWSH_FROM_SOURCE" = "1" ] || [ -n "$BROWSH_SOURCE_DIR" ]; then
	build_from_source
elif ! download_release_binary; then
	log "No prebuilt binary available, building from source instead"
	build_from_source
fi

# ---------------------------------------------------------------------------
# Install and verify
# ---------------------------------------------------------------------------
log "Installing to $BROWSH_INSTALL_DIR/browsh"
$SUDO install -d -m 0755 "$BROWSH_INSTALL_DIR"
$SUDO install -m 0755 "$BINARY" "$BROWSH_INSTALL_DIR/browsh"

INSTALLED_VERSION=$("$BROWSH_INSTALL_DIR/browsh" --version 2>&1 | tail -n1)
log "Installed Browsh $INSTALLED_VERSION"

cat <<EOF

Browsh is ready. Start it with:

    browsh

Useful things to know:
  * Config lives in ~/.config/browsh/config.toml (created on first run).
  * If something goes wrong, run 'browsh --debug' and look at ./debug.log.
  * Firefox runs headless, no display server is needed.
EOF
