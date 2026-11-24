package ingest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/antigravity"
)

func TestAntigravityFileIngest(t *testing.T) {
	e := newEnv(t)
	const sessionID = "20000000-0000-4000-8000-0000000000a1"
	path := filepath.Join(t.TempDir(), ".gemini", "antigravity-cli", "brain", sessionID, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	spec := devicesync.SourceSpec{Path: path, Agent: transcript.AgentAntigravity, StorageKind: transcript.StorageJSONLAppend, SessionKey: sessionID, Parser: antigravity.ParserName}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	secret := redacttest.Needles()["GITHUB_PAT"]
	if secret == "" {
		t.Fatal("missing GitHub token fixture")
	}
	prompt := func(index int, message string) []byte {
		t.Helper()
		content, err := json.Marshal("<USER_REQUEST>\n" + message + "\n</USER_REQUEST>")
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Appendf(nil, `{"step_index":%d,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-01-01T00:00:00Z","content":%s}`+"\n", index, content)
	}
	sync := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sy.Sync(e.ctx, spec); err != nil {
			t.Fatal(err)
		}
		e.drain()
	}
	texts := func() []string {
		t.Helper()
		rows, err := e.pool.Query(e.ctx, `SELECT m.text FROM messages m JOIN conversations c ON c.id=m.conversation_id
			WHERE NOT m.superseded AND c.agent=$1 AND c.session_id=$2 ORDER BY m.ordinal`, spec.Agent, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				t.Fatal(err)
			}
			out = append(out, text)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := prompt(0, "first Antigravity prompt")
	sync(first)
	if got := texts(); !slices.Equal(got, []string{"first Antigravity prompt"}) {
		t.Fatalf("initial transcript: %q", got)
	}
	sync(append(slices.Clone(first), prompt(1, "second Antigravity prompt "+secret)...))
	if got := texts(); len(got) != 2 || got[0] != "first Antigravity prompt" || !strings.HasPrefix(got[1], "second Antigravity prompt ") || strings.Contains(got[1], secret) {
		t.Fatalf("appended transcript has %d rows or failed redaction", len(got))
	}
	redactions, err := e.queue.Redactions(e.ctx)
	if err != nil || redactions.Device["github-token"] == 0 {
		t.Fatalf("device redactions: %+v, %v", redactions, err)
	}
	sync(prompt(0, "rewritten Antigravity prompt"))
	if got := texts(); !slices.Equal(got, []string{"rewritten Antigravity prompt"}) {
		t.Fatalf("rewritten transcript: %q", got)
	}
	e.exec(`UPDATE source_parse_state SET applied_parser='antigravity@0'`)
	id, err := e.queue.nextRefresh(e.ctx)
	if err != nil || id == "" {
		t.Fatalf("stale Antigravity parser was not queued: %q, %v", id, err)
	}
	if err := e.queue.refreshSource(e.ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := texts(); !slices.Equal(got, []string{"rewritten Antigravity prompt"}) {
		t.Fatalf("refreshed transcript: %q", got)
	}
}
