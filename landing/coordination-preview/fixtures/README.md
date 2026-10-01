# Retrieval example capture

`pagination.jsonl` is a synthetic Claude Code transcript. `grep.txt` and `read.txt` are unedited output from the retrieval CLI built at `abda135`. The homepage uses these outputs verbatim, with HTML escaping.

To reproduce from the repository root:

```sh
go build -o /tmp/flopwire-example-cli ./cmd/flopwire
task_capture_dir=$(mktemp -d)
mkdir -p "$task_capture_dir/projects/acme-app" "$task_capture_dir/empty"
cp landing/coordination-preview/fixtures/pagination.jsonl \
  "$task_capture_dir/projects/acme-app/0b7e2c1a-0000-4000-8000-000000000001.jsonl"
export FLOPWIRE_CONFIG="$task_capture_dir/config.json"
export FLOPWIRE_INDEX="$task_capture_dir/index.db"
/tmp/flopwire-example-cli agent run --once --no-sync \
  --claude-projects "$task_capture_dir/projects" \
  --codex-home "$task_capture_dir/empty" --devin-db -
/tmp/flopwire-example-cli grep -F next_cursor --include-self
/tmp/flopwire-example-cli read 0b7e2c1a/3026944:1 -B 1
```

Run in a separate shell to keep the fixture configuration isolated. No server is used. The messaging example is documented separately in `landing/agent-tools.md`.
