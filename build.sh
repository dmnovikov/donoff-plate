#!/bin/sh
# Static binary for the NanoPi (linux/arm64) into dist/.
set -e
cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o dist/donoff-linux-arm64 ./cmd/donoff
echo dist/donoff-linux-arm64
