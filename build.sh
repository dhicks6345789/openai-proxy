#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

OUT_DIR="dist"
BIN="openai-proxy"
HUGO_SITE="hugo"

# Human-readable file size, portable across Linux/macOS.
human_size() {
  local bytes
  bytes=$(wc -c < "$1" | tr -d ' ')
  if [ "$bytes" -lt 1048576 ]; then
    awk -v b="$bytes" 'BEGIN { printf "%.1f KB", b / 1024 }'
  else
    awk -v b="$bytes" 'BEGIN { printf "%.1f MB", b / 1048576 }'
  fi
}

build_host() {
  echo "==> Building Go executable (host platform)..."
  go build -o "$BIN" main.go api.go
  echo "==> Built: ./$BIN"
}

build_target() {
  local os="$1" arch="$2" goarm="$3" out="$4"
  if [ -n "$goarm" ]; then
    echo " -> $os/$arch (GOARM=$goarm)..."
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$goarm" go build -ldflags="-w -s" -o "$out" main.go api.go
  else
    echo " -> $os/$arch..."
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -ldflags="-w -s" -o "$out" main.go api.go
  fi
}

build_all() {
  mkdir -p "$OUT_DIR"
  echo "==> Compiling for all target platforms..."
  build_target linux amd64 "" "$OUT_DIR/openai-proxy-linux-amd64"
  build_target windows amd64 "" "$OUT_DIR/openai-proxy-windows-amd64.exe"
  build_target darwin amd64 "" "$OUT_DIR/openai-proxy-darwin-amd64"
  build_target darwin arm64 "" "$OUT_DIR/openai-proxy-darwin-arm64"
  build_target linux arm64 "" "$OUT_DIR/openai-proxy-rpi-arm64"
  build_target linux arm 7 "$OUT_DIR/openai-proxy-rpi-armv7"
  echo "==> All builds complete in ./$OUT_DIR/"
}

api_docs() {
  echo "==> Generating OpenAPI spec with Swaggo..."
  local swag_bin
  swag_bin="$(command -v swag || true)"
  if [ -z "$swag_bin" ]; then
    local gobin
    gobin="$(go env GOBIN)"
    [ -z "$gobin" ] && gobin="$(go env GOPATH)/bin"
    if [ -x "$gobin/swag" ]; then
      swag_bin="$gobin/swag"
    else
      echo "swag not found; installing it with Go..."
      go install github.com/swaggo/swag/cmd/swag@latest
      swag_bin="$gobin/swag"
    fi
  fi
  "$swag_bin" init -g api.go -d . -o docs --outputTypes json --useStructName --quiet
  echo "==> Generated: docs/swagger.json"
  echo "==> Generating docs/api.html..."
  go run . -gen-docs docs/api.html
  echo "==> Generated: docs/api.html"
}

write_downloads_data() {
  local out="$HUGO_SITE/data/downloads.toml"
  mkdir -p "$(dirname "$out")"
  : > "$out"

  local entries=(
    "openai-proxy-linux-amd64|Linux (x64)|🐧|64-bit Linux Desktop / Server"
    "openai-proxy-windows-amd64.exe|Windows (x64)|🪟|64-bit Windows Desktop"
    "openai-proxy-darwin-amd64|macOS (Intel x64)|🍎|Intel-based Mac computers"
    "openai-proxy-darwin-arm64|macOS (Apple Silicon)|🍏|Apple M1 / M2 / M3 / M4 Macs"
    "openai-proxy-rpi-arm64|Raspberry Pi (ARM64)|🍓|Raspberry Pi 3/4/5 (64-bit OS)"
    "openai-proxy-rpi-armv7|Raspberry Pi (ARMv7 32-bit)|🍓|Raspberry Pi 2/3/4 (32-bit OS)"
  )

  local entry filename name icon desc size
  for entry in "${entries[@]}"; do
    IFS='|' read -r filename name icon desc <<< "$entry"
    size="Unavailable"
    if [ -f "$OUT_DIR/$filename" ]; then
      size=$(human_size "$OUT_DIR/$filename")
    fi
    cat >> "$out" <<EOF
[[downloads]]
name = "$name"
filename = "$filename"
icon = "$icon"
desc = "$desc"
size = "$size"

EOF
  done
}

index() {
  echo "==> Generating index.html with Hugo..."
  if ! command -v hugo >/dev/null 2>&1; then
    echo "Error: hugo not found. Hugo (a Go-based static site tool) is required to generate index.html." >&2
    exit 1
  fi

  rm -rf "$HUGO_SITE/public"
  mkdir -p "$HUGO_SITE/content"

  {
    echo "---"
    echo 'title: "OpenAI Proxy"'
    echo "---"
    echo
    cat README.md
  } > "$HUGO_SITE/content/_index.md"

  write_downloads_data
  hugo --source "$HUGO_SITE" --destination public
  cp "$HUGO_SITE/public/index.html" ./index.html
  echo "==> Generated: ./index.html"
}

dist() {
  local dest="${DEST_DIR:-${1:-}}"
  if [ -z "$dest" ]; then
    echo "Error: DEST_DIR not set. Usage: DEST_DIR=/path/to/site ./build.sh dist" >&2
    echo "       or: ./build.sh dist /path/to/site" >&2
    exit 1
  fi

  # Accept 'DEST_DIR=/path' as a positional argument too.
  case "$dest" in
    DEST_DIR=*) dest="${dest#DEST_DIR=}" ;;
  esac
  if [ -z "$dest" ]; then
    echo "Error: no destination path given." >&2
    exit 1
  fi

  # Expand a leading tilde to the user's home directory.
  case "$dest" in
    "~"/*) dest="${HOME}/${dest#\~/}" ;;
    "~") dest="$HOME" ;;
  esac

  build_all
  api_docs
  index

  echo "==> Staging distribution files to $dest..."
  mkdir -p "$dest/docs"
  cp index.html "$dest/"
  cp -r docs/* "$dest/docs/"
  cp dist/* "$dest/"
  echo "==> Distribution complete: $dest"
}

run_desktop() {
  build_host
  ./"$BIN" -mode=desktop -port=8080
}

run_server() {
  build_host
  ./"$BIN" -mode=server -port=8080
}

test_all() {
  echo "==> Running tests..."
  go test ./...
}

clean() {
  echo "==> Cleaning build artifacts..."
  rm -f "$BIN"
  rm -rf "$OUT_DIR"
  rm -rf "$HUGO_SITE/public"
  rm -f index.html
}

usage() {
  echo "Usage: $0 [command]"
  echo
  echo "Commands:"
  echo "  build         Build the Go executable for the current platform"
  echo "  build-all     Build executables for all supported platforms into ./dist/"
  echo "  api-docs      Generate docs/api.html from the OpenAPI specification"
  echo "  index         Generate index.html from README.md with Hugo"
  echo "  dist          Stage dist/, docs/ and index.html into DEST_DIR"
  echo "                (DEST_DIR=/path or ./build.sh dist /path/to/site)"
  echo "  run-desktop   Build and run in desktop mode"
  echo "  run-server    Build and run in server mode"
  echo "  test          Run the Go test suite"
  echo "  clean         Remove build artifacts"
}

case "${1:-build}" in
  build) build_host ;;
  build-all) build_all ;;
  api-docs) api_docs ;;
  index) index ;;
  dist) dist "${2:-}" ;;
  run-desktop) run_desktop ;;
  run-server) run_server ;;
  test) test_all ;;
  clean) clean ;;
  help|-h|--help) usage ;;
  *) echo "Unknown command: $1" >&2; usage >&2; exit 1 ;;
esac
