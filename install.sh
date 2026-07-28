#!/usr/bin/env bash
set -euo pipefail

AFISLA_VERSION="${AFISLA_VERSION:-v0.4.0}"
DOWNLOAD_HOST="${AFISLA_HOST:-https://github.com/afisla/tunnel/releases/download/$AFISLA_VERSION}"
BIN_NAME="afisla"
INSTALL_DIR="${AFISLA_DIR:-/usr/local/bin}"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
info() { echo -e "${GREEN}[+]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
err()  { echo -e "${RED}[x]${NC} $1"; exit 1; }

usage() {
  cat <<EOF
Usage: curl -fsSL https://afisla.web.id/install.sh | bash

Downloads latest release binary from GitHub.
To build from source instead: --source (requires Go)

Options:
  --dir <PATH>   Install directory (default: $INSTALL_DIR)
  --source       Build from source (requires Go)
  --host <URL>   Download host override (default: GitHub releases)
  --help         Show this

Examples:
  curl -fsSL https://afisla.web.id/install.sh | bash
  curl -fsSL https://afisla.web.id/install.sh | bash -s -- --dir ~/.local/bin
EOF
  exit 0
}

BUILD_SOURCE=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)    INSTALL_DIR="$2"; shift 2 ;;
    --source) BUILD_SOURCE=true; shift ;;
    --host)   DOWNLOAD_HOST="$2"; shift 2 ;;
    --help)   usage ;;
    *)        err "Unknown: $1. Use --help" ;;
  esac
done

detect_arch() {
  local arch; arch=$(uname -m)
  case "$arch" in x86_64|amd64) echo "amd64" ;; aarch64|arm64) echo "arm64" ;; *) err "Unsupported arch: $arch" ;; esac
}
detect_os() {
  local os; os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in linux|darwin) echo "$os" ;; *) err "Unsupported OS: $os" ;; esac
}

install_download() {
  local url="$1" target="$2"
  info "Downloading from $url"
  if command -v curl &>/dev/null; then curl -fsSLo "$target" "$url"
  elif command -v wget &>/dev/null; then wget -qO "$target" "$url"
  else err "Need curl or wget"; fi
  chmod +x "$target"
}

install_source() {
  local tmpdir; tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  if ! command -v go &>/dev/null; then
    warn "Installing Go..."
    local os arch; os=$(detect_os); arch=$(detect_arch)
    curl -fsSLo "$tmpdir/go.tar.gz" "https://go.dev/dl/go1.23.4.${os}-${arch}.tar.gz"
    tar -C "$tmpdir" -xzf "$tmpdir/go.tar.gz"
    PATH="$tmpdir/go/bin:$PATH"
  fi

  if [[ -f "$(dirname "$0")/main.go" ]]; then
    cp -r "$(dirname "$0")" "$tmpdir/repo"
  else
    info "Downloading source from $DOWNLOAD_HOST/afisla-src.tar.gz"
    curl -fsSLo "$tmpdir/src.tar.gz" "$DOWNLOAD_HOST/afisla-src.tar.gz" 2>/dev/null || {
      warn "Source download failed. Use --host or install git and clone manually."
      info "Until then, install via: curl -fsSL https://afisla.web.id/install.sh | bash"
      err "Cannot obtain source."
    }
    mkdir "$tmpdir/repo" && tar -xzf "$tmpdir/src.tar.gz" -C "$tmpdir/repo" 2>/dev/null || err "Extract failed"
  fi

  cd "$tmpdir/repo"
  go build -o "$BIN_NAME" .
  cp "$BIN_NAME" "$target" && chmod +x "$target"
  info "Source build complete"
}

# --- main ---
if [[ "$EUID" -ne 0 ]] && [[ "$INSTALL_DIR" == "/usr/local/bin" ]]; then
  warn "Not root — install may fail. Use --dir ~/.local/bin or run with sudo"
fi

mkdir -p "$INSTALL_DIR"
target="$INSTALL_DIR/$BIN_NAME"

if [[ -f "$target" ]]; then
  warn "$target exists — overwriting"
fi

if $BUILD_SOURCE; then
  install_source
else
  arch=$(detect_arch)
  install_download "$DOWNLOAD_HOST/afisla-linux-$arch" "$target"
fi

info "$BIN_NAME installed!"
"$BIN_NAME" version
echo ""
echo "  HTTP:  $(basename $target) client --port-local 8080 --domain myapp"
echo "         curl https://myapp.afisla.web.id"
echo ""
echo "  SSH:   $(basename $target) client --port-local 22 --domain myserver"
echo "         ssh -o ProxyCommand='nc relay.afisla.web.id RELAY_PORT' user@localhost -p 22"
echo "         (RELAY_PORT is shown after client connects)"
echo ""
echo "  DNS:   HTTP via *.afisla.web.id (Cloudflare proxied)"
echo "         Tunnel via relay.afisla.web.id (direct)"
echo ""
