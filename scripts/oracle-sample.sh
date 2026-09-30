#!/usr/bin/env bash
# oracle-sample.sh: FAD 0.3.1 parity on a sample of this device's real
# transcripts (spec §11.2, acceptance item c).
#
# Copies about 50 Claude sessions, 50 Codex rollouts (over 4KB, so
# title-only stubs are left out; evenly spaced in sorted order) and Devin's sessions.db into <scratch>/oracle/home,
# runs tools/fad-dump on that home, then runs each parser's
# TestOracleCorpusSample, which writes <scratch>/oracle/report-<agent>.json.
# The harness directories are only read. Reports quote real transcript
# text: keep them out of the repository.
#
# With AGENTSVIEW_SRC=<agentsview clone>, it also builds tools/avdump
# inside that clone (Go 1.27 via GOTOOLCHAIN, which agentsview needs) and
# dumps agentsview's parse of the same home, the second oracle.
#
# Usage: [AGENTSVIEW_SRC=dir] scripts/oracle-sample.sh <scratch dir> [per-agent count, default 50]
# Requires: cargo (first run builds fad-dump), go, sqlite3.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scratch="${1:?usage: oracle-sample.sh <scratch dir> [count]}"
count="${2:-50}"
out="$scratch/oracle"
home="$out/home"
rm -rf "$out"
mkdir -p "$home/.claude/projects" "$home/.codex/sessions" "$home/.local/share/devin/cli"

# every_nth LIST COUNT: COUNT lines evenly spaced through LIST.
every_nth() {
  awk -v n="$2" '{ a[NR] = $0 } END { if (NR == 0) exit; step = NR / n; if (step < 1) step = 1;
    for (i = 1; i <= NR && c < n; i += step) { print a[int(i)]; c++ } }' "$1"
}

claude_src="${CLAUDE_CONFIG_DIR:-$HOME/.claude}/projects"
codex_src="${CODEX_HOME:-$HOME/.codex}/sessions"
devin_src="$HOME/.local/share/devin/cli/sessions.db"

find "$claude_src" -mindepth 2 -maxdepth 2 -name '*.jsonl' -size +4k | LC_ALL=C sort > "$out/claude.all"
every_nth "$out/claude.all" "$count" > "$out/claude.sample"
while IFS= read -r f; do
  rel="${f#"$claude_src"/}"
  mkdir -p "$home/.claude/projects/${rel%/*}"
  cp "$f" "$home/.claude/projects/$rel"
  # Persisted tool outputs the parser reads beside the transcript.
  if [ -d "${f%.jsonl}/tool-results" ]; then
    mkdir -p "$home/.claude/projects/${rel%.jsonl}"
    cp -R "${f%.jsonl}/tool-results" "$home/.claude/projects/${rel%.jsonl}/"
  fi
done < "$out/claude.sample"

find "$codex_src" -name 'rollout-*.jsonl' -size +4k | LC_ALL=C sort > "$out/codex.all"
every_nth "$out/codex.all" "$count" > "$out/codex.sample"
while IFS= read -r f; do
  rel="${f#"$codex_src"/}"
  mkdir -p "$home/.codex/sessions/${rel%/*}"
  cp "$f" "$home/.codex/sessions/$rel"
done < "$out/codex.sample"

if [ -f "$devin_src" ]; then
  cp "$devin_src" "$home/.local/share/devin/cli/sessions.db"
  [ -f "$devin_src-wal" ] && cp "$devin_src-wal" "$home/.local/share/devin/cli/sessions.db-wal"
  # FAD 0.3.1 (frankensqlite) finds no sessions in a store that has a WAL
  # beside it; fold the WAL into the copy so both parsers read one file.
  sqlite3 "$home/.local/share/devin/cli/sessions.db" 'PRAGMA wal_checkpoint(TRUNCATE);' > /dev/null
fi
echo "oracle-sample: $(wc -l < "$out/claude.sample") claude, $(wc -l < "$out/codex.sample") codex files, devin: $([ -f "$devin_src" ] && echo yes || echo no)"

fad="$root/tools/fad-dump/target/release/fad-dump"
[ -x "$fad" ] || cargo build --release --locked --quiet --manifest-path "$root/tools/fad-dump/Cargo.toml"
data_dir="$(mktemp -d)"
trap 'rm -rf "$data_dir"' EXIT
env -i HOME="$home" PATH="$PATH" "$fad" --home "$home" --out-dir "$out/expected" --data-dir "$data_dir" \
  --agent claude --agent codex --agent devin
echo "oracle-sample: fad-dump wrote $(ls "$out/expected" | wc -l | tr -d ' ') expected files"

if [ -n "${AGENTSVIEW_SRC:-}" ]; then
  mkdir -p "$AGENTSVIEW_SRC/cmd/avdump" "$out/agentsview"
  sed '1,2d' "$root/tools/avdump/main.go" > "$AGENTSVIEW_SRC/cmd/avdump/main.go"
  # A clone lacks the generated pricing snapshot it embeds; parsing never
  # prices, so a one-model stub satisfies its validation.
  snap="$AGENTSVIEW_SRC/internal/pricing/snapshot/litellm_snapshot.json.gz"
  [ -f "$snap" ] || printf '{"version":"stub","source_ref":"%040d","models":[{"ModelPattern":"stub"}]}' 0 | gzip > "$snap"
  (cd "$AGENTSVIEW_SRC" && GOTOOLCHAIN=go1.27.1 go build -o "$out/avdump" ./cmd/avdump)
  HOME="$home" "$out/avdump" -agent claude -root "$home/.claude/projects" > "$out/agentsview/claude.jsonl"
  HOME="$home" "$out/avdump" -agent codex -root "$home/.codex/sessions" > "$out/agentsview/codex.jsonl"
  HOME="$home" "$out/avdump" -agent devin -root "$home/.local/share/devin" > "$out/agentsview/devin.jsonl"
  echo "oracle-sample: agentsview parsed $(cat "$out"/agentsview/*.jsonl | wc -l | tr -d ' ') sessions"
fi

cd "$root"
FLOPWIRE_ORACLE_SAMPLE="$out" go test -count=1 -run TestOracleCorpusSample -v \
  ./internal/transcript/claude ./internal/transcript/codex ./internal/transcript/devin | grep -E 'oracle_corpus_test|^(ok|FAIL|---)'
