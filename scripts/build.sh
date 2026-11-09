#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin assets/payload
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}
  arch=${target#*/}
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -tags helper -trimpath -ldflags='-s -w' -o bin/helper ./cmd/rsync_tool
  gzip -c bin/helper > "assets/payload/$os-$arch.gz"
done
rm bin/helper
go build -trimpath -ldflags='-s -w' -o bin/rsync_tool ./cmd/rsync_tool
