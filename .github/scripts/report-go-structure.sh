#!/usr/bin/env bash
set -euo pipefail

golangci-lint run --config .golangci.structure.yml ./cmd/... ./internal/...
find cmd internal -type f -name '*.go' ! -name '*_test.go' ! -name '*.pb.go' \
  -exec awk 'FNR == 401 { print FILENAME ":401: Go source file exceeds 400 lines" }' {} +
