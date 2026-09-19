#!/bin/sh
set -eu
: "${PRISM_ADMIN_TOKEN:?Set PRISM_ADMIN_TOKEN to the terminal administrator token}"
BASE="${PRISM_URL:-http://127.0.0.1:8080}"
curl --fail-with-body -sS "$BASE/api.json" \
  -H "Authorization: Bearer $PRISM_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"config.get","params":{}}'
printf '\n'
