#!/usr/bin/env bash
# Tests scripts/perf-baseline.sh in a scratch git repository.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/perf-baseline.sh"
repo="$(mktemp -d)"
trap 'rm -rf -- "$repo"' EXIT
cd "$repo"
git init -q
git config user.email t@example.com
git config user.name t
git config commit.gpgsign false
git config tag.gpgsign false
c() { git commit -q --allow-empty -m "$1"; git rev-parse HEAD; }
pin() { mkdir -p docs/perf; echo "$1" >docs/perf/nightly-baseline; }

fail=0
expect() { # expect NAME WANT_SHA_OR_FAIL [env...]
  local name="$1" want="$2" got
  shift 2
  if got="$(env "$@" "$script" 2>/dev/null)"; then
    got="$(sed -n 's/^sha=//p' <<<"$got")"
  else
    got=FAIL
  fi
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $name: got $got, want $want" >&2
    fail=1
  fi
}

c1="$(c one)"
expect "no pin, no tag" FAIL
c2="$(c two)"
pin "$c1"
expect "pin only" "$c1"
pin "${c1:0:12}"
expect "short pin refused" FAIL
pin "$c1"
git tag v0.1.0 "$c2"
c3="$(c three)"
expect "release newer than pin" "$c2"
pin "$c3"
c4="$(c four)"
expect "pin moved after the release" "$c3"
expect "input sha" "$c1" INPUT_BASELINE="${c1:0:10}"
expect "input tag" "$c2" INPUT_BASELINE=v0.1.0
# shellcheck disable=SC2016 # literal on purpose
expect "input with shell text refused" FAIL 'INPUT_BASELINE=$(id)'
expect "input option refused" FAIL INPUT_BASELINE=--all
git checkout -q --orphan other
c5="$(c five)"
pin "$c4"
expect "pin not an ancestor" FAIL
: "$c5"
exit "$fail"
