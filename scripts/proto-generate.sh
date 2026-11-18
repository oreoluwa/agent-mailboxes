#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN_DIR="${GOBIN:-$(go env GOPATH)/bin}"
PROTO_FILE="${ROOT_DIR}/proto/agentemails/emaild/v1/emaild.proto"

export PATH="${GO_BIN_DIR}:${PATH}"


protoc \
  -I "${ROOT_DIR}/proto" \
  --plugin=protoc-gen-go="${GO_BIN_DIR}/protoc-gen-go" \
  --plugin=protoc-gen-connect-go="${GO_BIN_DIR}/protoc-gen-connect-go" \
  --go_out="${ROOT_DIR}/proto" \
  --go_opt=paths=source_relative \
  --connect-go_out="${ROOT_DIR}/proto" \
  --connect-go_opt=paths=source_relative \
  "${PROTO_FILE}"
