#!/bin/sh
# Installs even-cc on macOS or Linux from the latest GitHub release:
#
#   curl -fsSL https://raw.githubusercontent.com/sorbet-ai/even-cc/main/install.sh | sh
#
# Environment: EVEN_CC_VERSION=v1.2.3 (default: latest),
# EVEN_CC_INSTALL_DIR (default: ~/.local/bin).
set -eu

REPO="sorbet-ai/even-cc"
VERSION="${EVEN_CC_VERSION:-latest}"
DIR="${EVEN_CC_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
fail() { printf 'even-cc install: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) fail "unsupported system $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac

asset="even-cc_${os}_${arch}.tar.gz"
if [ -n "${EVEN_CC_BASE_URL:-}" ]; then # a mirror, or a local test
  base="$EVEN_CC_BASE_URL"
elif [ "$VERSION" = latest ]; then
  base="https://github.com/$REPO/releases/latest/download"
else
  base="https://github.com/$REPO/releases/download/$VERSION"
fi

if command -v curl >/dev/null 2>&1; then
  get() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  get() { wget -qO "$2" "$1"; }
else
  fail "needs curl or wget"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
say "Downloading $asset ($VERSION)..."
get "$base/$asset" "$tmp/$asset" || fail "download failed: $base/$asset"
if get "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
  want="$(grep -E "[ *]$asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
  if command -v sha256sum >/dev/null 2>&1; then got="$(sha256sum "$tmp/$asset" | cut -d' ' -f1)"
  else got="$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)"; fi
  [ -n "$want" ] && [ "$want" = "$got" ] || fail "checksum mismatch for $asset"
fi
tar -xzf "$tmp/$asset" -C "$tmp"
mkdir -p "$DIR"
# replace atomically: a running even-cc keeps its old file
cp "$tmp/even-cc" "$DIR/even-cc.new" && chmod +x "$DIR/even-cc.new" && mv -f "$DIR/even-cc.new" "$DIR/even-cc"
say "Installed $("$DIR/even-cc" version) to $DIR/even-cc"

case ":$PATH:" in
  *":$DIR:"*) ;;
  *) say "" ; say "Add $DIR to your PATH, e.g. in ~/.zshrc or ~/.bashrc:" ; say "  export PATH=\"$DIR:\$PATH\"" ;;
esac
command -v claude >/dev/null 2>&1 || say "Note: Claude Code (claude) isn't on your PATH; even-cc needs it: https://claude.com/claude-code"

say ""
say "Next: run 'even-cc start' and scan the QR code with Faceclaw"
say "(Claude Code > Settings > Machines > Add machine). Stop it with 'even-cc stop'."
