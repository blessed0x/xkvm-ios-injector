#!/usr/bin/env bash
# xkvm one-shot installer — macOS and Linux (Ubuntu, Arch, anything with
# bash + curl/wget). Downloads the latest release binary from GitHub, or
# falls back to `go install` when no release exists yet (pre-release days).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/blessed0x/xkvm-ios-injector/main/scripts/install.sh | bash
#
# Installs to ~/.local/bin by default (respect XKVM_PREFIX to override).
set -euo pipefail
# --- intro (moon scene, terminal-safe) -------------------------------------
# Full moon art when stdout is a real terminal; one deterministic plain line
# when piped into a file, CI, or NO_COLOR is set — so this never breaks
# `curl | bash` automation.
intro() {
  if [ -t 1 ]; then
    BOLD=""; MG=""; CY=""; YL=""; DM=""; RS=""
    if [ -z "${NO_COLOR:-}" ]; then
      BOLD=$'\033[1m'; MG=$'\033[35m'; CY=$'\033[36m'; YL=$'\033[33m'; DM=$'\033[90m'; RS=$'\033[0m'
    fi
    printf '%s\n' \
      "${DM}               ·                          ✧${RS}" \
      "${YL}     ✧                       .${RS}" \
      "${CY}                 ▄▄▄▄▓▓▄▄▄▄${RS}" \
      "${CY}       .       ▄▓▓▓██░░░░░░░▀▄${RS}" \
      "${CY}             ▄▓▓██░░░░░  ✧  ░░▀▄${RS}" \
      "${CY}            ▄▓██░░░░░  ✵      ░░▀▄${RS}" \
      "${CY}  ✧        ▄▓█░░░░░░            ░▐█▄${RS}" \
      "${CY}           ▐█▌░░░░░░            ░░██${RS}" \
      "${CY}            ▀█▄░░░░░          ░░▄█▀${RS}" \
      "${CY}             ▀██▄░░░░░      ░░▄█▀${RS}" \
      "${CY}      .        ▀▀████▓▄▄▄▄▄██▀▀${RS}" \
      "${DM}                 ✧            ✦${RS}" \
      "${DM}        ·                        ✧${RS}" \
      "${BOLD}${MG}   x k v m${RS}" \
      "${CY}   the friendly way to tweak your iOS apps${RS}" \
      "${DM}   installing on $(uname -s) via ${PREFIX:-~/.local/bin}${RS}" \
      ""
  else
    printf '%s\n' "xkvm — the friendly way to tweak your iOS apps"
  fi
}


REPO="blessed0x/xkvm-ios-injector"
VERSION="${XKVM_VERSION:-latest}"

# --- pick an install prefix -------------------------------------------------
if [ -n "${XKVM_PREFIX:-}" ]; then
  PREFIX="$XKVM_PREFIX"
elif [ "$(uname -s)" = "Darwin" ] && [ -d "/opt/homebrew/bin" ] && [ -w "/opt/homebrew/bin" ]; then
  PREFIX="/opt/homebrew/bin"
else
  PREFIX="$HOME/.local/bin"
fi
mkdir -p "$PREFIX"

# --- detect os/arch in goreleaser's naming ---------------------------------
case "$(uname -s)" in
  Darwin) OS="darwin" ;;
  Linux)  OS="linux" ;;
  *)      echo "xkvm: unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) ARCH="arm64" ;;
  x86_64|amd64)  ARCH="amd64" ;;
  *) echo "xkvm: unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

BIN="$PREFIX/xkvm"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

intro
echo "xkvm: installing to $PREFIX"

# --- figure out the release to grab ----------------------------------------
API="https://api.github.com/repos/$REPO/releases/latest"
REL_JSON="$(curl -fsSL "$API" 2>/dev/null || true)"
if [ -z "$REL_JSON" ] || ! echo "$REL_JSON" | grep -q '"tag_name"'; then
  echo "xkvm: no release found yet — falling back to 'go install' (needs Go 1.26+)."
  if ! command -v go >/dev/null 2>&1; then
    echo "xkvm: 'go' not found. Install Go (https://go.dev/dl/) then re-run, or wait for a release." >&2
    exit 1
  fi
  go install "github.com/blessed0x/xkvm-ios-injector/cmd/xkvm@latest"
  echo "xkvm: installed via 'go install'."
  exit 0
fi

TAG="$(echo "$REL_JSON" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
VER="${TAG#v}"
ASSET="xkvm_${VER}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"

echo "xkvm: fetching $TAG ($ASSET)"
if ! curl -fsSL -o "$TMP/xkvm.tar.gz" "$URL"; then
  echo "xkvm: release $TAG has no $ASSET (maybe the release was made before this asset existed)." >&2
  echo "xkvm: try again with a newer release, or install from source: go install github.com/blessed0x/xkvm-ios-injector/cmd/xkvm@latest" >&2
  exit 1
fi

tar -xzf "$TMP/xkvm.tar.gz" -C "$TMP"
BIN_SRC="$(find "$TMP" -type f -name xkvm | head -1)"
if [ -z "$BIN_SRC" ]; then
  echo "xkvm: archive did not contain the xkvm binary" >&2
  exit 1
fi

install -m 0755 "$BIN_SRC" "$BIN"
echo "xkvm: installed $( "$BIN" --version 2>/dev/null || echo "xkvm" ) at $BIN"

# --- PATH hint -------------------------------------------------------------
case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) echo "xkvm: add $PREFIX to your PATH (e.g. echo 'export PATH=\"$PREFIX:\$PATH\"' >> ~/.bashrc)" ;;
esac

echo "xkvm: done. Run 'xkvm tui' to get started."