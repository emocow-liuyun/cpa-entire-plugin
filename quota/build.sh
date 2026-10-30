#!/usr/bin/env bash
# Build, test, and smoke-check the Entire quota plugin.
# Usage: ./build.sh [output-name]   (default: libentire_quota.so)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

artifact="${1:-libentire_quota.so}"

echo "== go vet =="
go vet ./...

echo "== go test =="
go test -count=1 ./...

echo "== go test -race =="
go test -count=1 -race ./...

echo "== go build -buildmode=c-shared -o ${artifact} . =="
go build -buildmode=c-shared -o "$artifact" .

echo "== native ABI smoke =="
python3 tools/abi_smoke.py "./$artifact"

echo "== done: $(pwd)/${artifact} =="
ls -la "./$artifact"
