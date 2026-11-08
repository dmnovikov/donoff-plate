#!/bin/sh
# Static binaries of donoff-panel into dist/: for this machine and for linux/arm64.
set -e
cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/donoff-panel ./cmd/donoff-panel
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/donoff-panel-linux-arm64 ./cmd/donoff-panel
ls dist/
