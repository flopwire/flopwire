#!/usr/bin/env bash
# regen-oracle.sh: rebuild SQLite fixtures from testdata/oracle/seeds and
# regenerate testdata/oracle/expected/*.json with tools/fad-dump (FAD 0.3.1).
#
# Adapted from gbasin/agentboard scripts/regen-fad-fixtures.ts (MIT) at
# commit 4fb640dd9b539413d2ba2fdcf5385a7b51478797.
#
# Usage: scripts/regen-oracle.sh [fad connector slug ...]   (default: claude codex devin)
# Requires: cargo, sqlite3.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
oracle="$root/testdata/oracle"
home="$oracle/home"
agents=("$@")
[ ${#agents[@]} -eq 0 ] && agents=(claude codex devin)

# Each seed names its target under home on its first line: "-- target: <path>".
for seed in "$oracle"/seeds/*.sql; do
  [ -e "$seed" ] || continue
  target="$(sed -n '1s/^-- target: //p' "$seed")"
  [ -n "$target" ] || { echo "regen-oracle: $seed lacks a '-- target:' first line" >&2; exit 1; }
  db="$home/$target"
  mkdir -p "$(dirname "$db")"
  rm -f "$db" "$db-wal" "$db-shm"
  sqlite3 "$db" < "$seed"
  echo "built ${db#"$root"/}"
done

cargo build --release --locked --quiet --manifest-path "$root/tools/fad-dump/Cargo.toml"

agent_args=()
for a in "${agents[@]}"; do agent_args+=(--agent "$a"); done

# Scrubbed environment: connectors resolve roots from HOME only, never from
# the developer's CLAUDE_CONFIG_DIR, CODEX_HOME, XDG_* or real home.
data_dir="$(mktemp -d)"
trap 'rm -rf "$data_dir"' EXIT
env -i HOME="$home" PATH="$PATH" \
  "$root/tools/fad-dump/target/release/fad-dump" \
  --home "$home" --out-dir "$oracle/expected" --data-dir "$data_dir" "${agent_args[@]}"
