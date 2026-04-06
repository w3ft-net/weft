#!/bin/bash
# Build the weft CLI and install it on PATH.
# Usage: ./scripts/build-cli.sh
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION=$(grep 'Version.*=' cmd/weft/version.go | sed 's/.*"\(.*\)".*/\1/')
echo "Building weft CLI v${VERSION}..."

DEST="${GOBIN:-$(go env GOPATH)/bin}/weft"
go build -o "$DEST" ./cmd/weft/

echo "Installed weft v${VERSION} to $DEST"
