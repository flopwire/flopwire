#!/bin/sh
# Response contract checks only; no server, database, or containers.
set -eu
filter="$(dirname "$0")/e2e-search.jq"

expect() {
  name=$1 want=$2 body=$3
  if printf '%s\n' "$body" | jq -e -s -f "$filter" >/dev/null 2>&1; then
    got=PASS
  else
    got=FAIL
  fi
  if [ "$got" != "$want" ]; then
    printf 'FAIL %s: got %s, want %s\n' "$name" "$got" "$want" >&2
    exit 1
  fi
}

expect 'omitted zero fields' PASS '{"hits":[]}'
expect 'explicit zero fields' PASS '{"hits":[],"total":0,"total_sessions":0,"offset":0,"next_offset":0,"truncated":false,"reason":"","sessions":[],"session_info":[]}'
expect 'problem response' FAIL '{"status":501,"detail":"no index"}'
expect 'error envelope' FAIL '{"hits":[],"error":"failed"}'
expect 'missing hits' FAIL '{}'
expect 'null hits' FAIL '{"hits":null}'
expect 'wrong hits type' FAIL '{"hits":{}}'
expect 'unexpected synthetic hit' FAIL '{"hits":[{"address":"synthetic/1"}]}'
expect 'unexpected count' FAIL '{"hits":[],"total":1}'
expect 'null count' FAIL '{"hits":[],"total":null}'
expect 'wrong count type' FAIL '{"hits":[],"total":"0"}'
expect 'session count' FAIL '{"hits":[],"total_sessions":1}'
expect 'wrong page' FAIL '{"hits":[],"offset":1}'
expect 'continuation' FAIL '{"hits":[],"next_offset":20}'
expect 'timeout page' FAIL '{"hits":[],"truncated":true,"reason":"timed out"}'
expect 'wrong truncation type' FAIL '{"hits":[],"truncated":"false"}'
expect 'unexpected session' FAIL '{"hits":[],"sessions":[{"id":"synthetic"}]}'
expect 'unexpected session metadata' FAIL '{"hits":[],"session_info":[{"id":"synthetic"}]}'
expect 'reason without truncation' FAIL '{"hits":[],"reason":"timed out"}'
expect 'null page' FAIL 'null'
expect 'malformed JSON' FAIL '{'
expect 'empty body' FAIL ''
expect 'multiple pages' FAIL '{"hits":[]} {"hits":[]}'
printf 'E2E search response checks passed (23 cases)\n'
