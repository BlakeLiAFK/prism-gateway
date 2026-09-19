#!/bin/sh
set -eu
: "${PRISM_API_KEY:?Set PRISM_API_KEY to a client key created in the WebUI}"
BASE="${PRISM_URL:-http://127.0.0.1:8080}"
# These demos require the Enable Sandbox action in the WebUI overview.
printf '\n--- Chat Completions ---\n'
curl --fail-with-body -sS "$BASE/openai/v1/chat/completions" \
  -H "Authorization: Bearer $PRISM_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"demo-chat","messages":[{"role":"user","content":"Hello"}]}'
printf '\n--- Responses ---\n'
curl --fail-with-body -sS "$BASE/openai/v1/responses" \
  -H "Authorization: Bearer $PRISM_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"demo-responses","input":"Hello"}'
printf '\n--- Anthropic SSE ---\n'
curl --fail-with-body -sS -N "$BASE/anthropic/v1/messages" \
  -H "x-api-key: $PRISM_API_KEY" -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' -H 'X-Prism-Session: example-conversation-1' \
  -d '{"model":"demo-messages","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"Hello"}]}'
printf '\n--- Estimated token count (inspect response header) ---\n'
curl --fail-with-body -sS -i "$BASE/anthropic/v1/messages/count_tokens" \
  -H "x-api-key: $PRISM_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"demo-messages","messages":[{"role":"user","content":"Hello"}]}'
printf '\n'
