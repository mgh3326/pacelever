#!/bin/sh
# Final verification. Development should use focused go test -run selections.
set -eu
export GOMAXPROCS=${GOMAXPROCS:-2}
export GOFLAGS=${GOFLAGS:--p=2}
test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go test -race -p 1 ./...
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  CGO_ENABLED=0 GOOS=${target%/*} GOARCH=${target#*/} go build -trimpath \
    -ldflags='-s -w' -o "dist/pacelever-${target%/*}-${target#*/}" ./cmd/pacelever
done
