#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
tools_dir=$(mktemp -d)
trap 'rm -rf "$tools_dir"' EXIT
GOBIN="$tools_dir" go install github.com/bufbuild/buf/cmd/buf@v1.73.0
GOBIN="$tools_dir" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$tools_dir" go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.0
PATH="$tools_dir:$PATH" buf lint
PATH="$tools_dir:$PATH" buf generate
