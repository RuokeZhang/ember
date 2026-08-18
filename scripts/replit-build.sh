#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${repo_root}"

npm --prefix web ci --ignore-scripts --no-audit --no-fund
npm --prefix web run build
mkdir -p bin
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/ember-control-api ./controlapi/cmd/control-api
