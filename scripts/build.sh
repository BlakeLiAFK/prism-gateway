#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
./scripts/doctor.sh
mkdir -p bin
GOPROXY=off CGO_ENABLED=1 go build -trimpath -o bin/prism-gateway ./cmd/gateway
printf '\nBuilt: bin/prism-gateway\nStart: ./bin/prism-gateway\nDemo: ./bin/prism-gateway --demo\n'
