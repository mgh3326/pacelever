#!/bin/sh
# Final verification. Development should use focused go test -run selections.
set -eu
export GOMAXPROCS=${GOMAXPROCS:-2}
export GOFLAGS=${GOFLAGS:--p=2}
go vet ./...
go test -race -p 1 ./...
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -trimpath \
    -ldflags='-s -w' -o "dist/pacelever-${target%/*}-${target#*/}" ./cmd/pacelever
done
