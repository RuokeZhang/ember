#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "${repo_root}"

exec ./bin/ember-control-api \
  --listen-address=0.0.0.0:8080 \
  --secure-cookies=true \
  --web-root=web/dist
