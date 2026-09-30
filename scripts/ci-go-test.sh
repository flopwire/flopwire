#!/bin/sh
# Runs `go test -json` with the given arguments, then prints the pass, fail
# and skip counts, every skipped test with its reason, and the output of
# failed tests. Keeps the raw events in $RUNNER_TEMP/go-test.json.
set -u
out="${RUNNER_TEMP:-/tmp}/go-test.json"
go test -json "$@" >"$out"
status=$?
echo "== result counts (tests and subtests)"
jq -r 'select(.Test and (.Action=="pass" or .Action=="fail" or .Action=="skip")) | .Action' "$out" | sort | uniq -c
echo "== skipped tests"
jq -rs 'map(select(.Test and .Action=="output" and (.Output|test("^\\s*--- SKIP|_test.go:[0-9]+:")))) as $o
  | map(select(.Test and .Action=="skip")) | .[]
  | . as $s | "\($s.Package|sub("^github.com/flopwire/flopwire/";"")) \($s.Test): "
    + ([$o[] | select(.Package==$s.Package and .Test==$s.Test and (.Output|test("--- SKIP")|not)) | .Output | gsub("^\\s+|\\s+$";"")] | first // "")' "$out"
echo "== slowest packages"
jq -r 'select(.Action=="pass" or .Action=="fail") | select(.Test==null) | "\(.Elapsed)\t\(.Package|sub("^github.com/flopwire/flopwire/";""))"' "$out" | sort -rn | head -10
if [ "$status" -ne 0 ]; then
  echo "== failures"
  jq -rs '(map(select(.Action=="fail") | "\(.Package) \(.Test // "")")) as $f
    | .[] | select(.Action=="output" and ("\(.Package) \(.Test // "")" as $k | $f | index($k))) | .Output' "$out"
  jq -r 'select(.Action=="build-fail" or (.Action=="output" and .ImportPath)) | .Output // empty' "$out"
fi
exit "$status"
