#!/bin/sh
# One-click installer for agent (https://github.com/holihur/agent)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/holihur/agent/main/install.sh | sh
#
# Environment overrides:
#   AGENT_VERSION     install a specific tag, e.g. v0.1.0 (default: latest release)
#   AGENT_INSTALL_DIR install directory (default: /usr/local/bin if writable, else ~/.local/bin)
#   AGENT_REPO        GitHub repo as OWNER/NAME (default: holihur/agent)
#   AGENT_GITHUB_BASE GitHub base URL (default: https://github.com), useful for proxies/tests
set -eu

REPO="${AGENT_REPO:-holihur/agent}"
GITHUB="${AGENT_GITHUB_BASE:-https://github.com}"
VERSION="${AGENT_VERSION:-}"
INSTALL_DIR="${AGENT_INSTALL_DIR:-}"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

command -v tar >/dev/null 2>&1 || die "tar is required"
if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
  die "curl or wget is required"
fi

fetch() { # fetch <url> <outfile|-> : download url to file, or stdout if -
  url=$1
  out=$2
  if command -v curl >/dev/null 2>&1; then
    if [ "$out" = "-" ]; then
      curl -fsSL "$url"
    else
      curl -fsSL -o "$out" "$url"
    fi
  else
    if [ "$out" = "-" ]; then
      wget -qO- "$url"
    else
      wget -qO "$out" "$url"
    fi
  fi
}

resolve_latest() { # resolve tag from the /releases/latest redirect, no API needed
  url=""
  if command -v curl >/dev/null 2>&1; then
    url=$(curl -fsSI -o /dev/null -w '%{redirect_url}' "$GITHUB/$REPO/releases/latest") || return 1
  else
    url=$(wget -qS --max-redirect=0 -O /dev/null "$GITHUB/$REPO/releases/latest" 2>&1 |
      sed -n 's/.*[Ll]ocation: //p' | head -n 1) || return 1
  fi
  [ -n "$url" ] || return 1
  printf '%s' "${url##*/}"
}

# --- detect platform (names follow .goreleaser.yml archives) -----------------
OS=$(uname -s)
case "$OS" in
  Darwin) OS_NAME=Darwin ;;
  Linux) OS_NAME=Linux ;;
  *) die "unsupported OS: $OS (only macOS and Linux are supported)" ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64) ARCH_NAME=x86_64 ;;
  arm64 | aarch64) ARCH_NAME=arm64 ;;
  i386 | i686) ARCH_NAME=i386 ;;
  armv6* | armv7*) ARCH_NAME=armv7 ;;
  *) die "unsupported architecture: $ARCH" ;;
esac

ARCHIVE="agent_${OS_NAME}_${ARCH_NAME}.tar.gz"

# --- resolve version ----------------------------------------------------------
if [ -z "$VERSION" ]; then
  VERSION=$(resolve_latest) || die "cannot resolve the latest release; set AGENT_VERSION to a tag (e.g. v0.1.0)"
fi
case "$VERSION" in
  v[0-9]* | [0-9]*) ;;
  *) die "no published release found at $GITHUB/$REPO (got \"$VERSION\"); publish one via GoReleaser first" ;;
esac

# --- download and verify ------------------------------------------------------
TMPDIR_INSTALL=$(mktemp -d)
trap 'rm -rf "$TMPDIR_INSTALL"' EXIT INT TERM

BASE="$GITHUB/$REPO/releases/download/$VERSION"
log "==> downloading $REPO $VERSION for ${OS_NAME}/${ARCH_NAME}"
fetch "$BASE/$ARCHIVE" "$TMPDIR_INSTALL/$ARCHIVE" ||
  die "download failed: $BASE/$ARCHIVE (no asset for ${OS_NAME}/${ARCH_NAME}?)"

log "==> verifying checksum"
fetch "$BASE/checksums.txt" "$TMPDIR_INSTALL/checksums.txt" ||
  die "download failed: $BASE/checksums.txt"
EXPECTED=$(grep "  $ARCHIVE\$" "$TMPDIR_INSTALL/checksums.txt" | awk '{print $1}') ||
  die "checksums.txt has no entry for $ARCHIVE"
[ -n "$EXPECTED" ] || die "checksums.txt has no entry for $ARCHIVE"
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMPDIR_INSTALL/$ARCHIVE" | awk '{print $1}')
else
  ACTUAL=$(shasum -a 256 "$TMPDIR_INSTALL/$ARCHIVE" | awk '{print $1}')
fi
[ "$ACTUAL" = "$EXPECTED" ] || die "checksum mismatch: want $EXPECTED, got $ACTUAL"

# --- extract and install ------------------------------------------------------
tar -xzf "$TMPDIR_INSTALL/$ARCHIVE" -C "$TMPDIR_INSTALL"
[ -f "$TMPDIR_INSTALL/agent" ] || die "archive does not contain the agent binary"

if [ -z "$INSTALL_DIR" ]; then
  if [ -w /usr/local/bin ]; then
    INSTALL_DIR=/usr/local/bin
  elif [ -w /usr/local ]; then
    mkdir -p /usr/local/bin && INSTALL_DIR=/usr/local/bin
  else
    INSTALL_DIR="$HOME/.local/bin"
  fi
fi
mkdir -p "$INSTALL_DIR"
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$TMPDIR_INSTALL/agent" "$INSTALL_DIR/agent"
else
  cp "$TMPDIR_INSTALL/agent" "$INSTALL_DIR/agent" && chmod 0755 "$INSTALL_DIR/agent"
fi

log "==> installed $VERSION to $INSTALL_DIR/agent"
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    log "==> note: $INSTALL_DIR is not in PATH; add it with:"
    log "    export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac
log "==> run 'agent -h' to get started"
