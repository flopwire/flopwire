package devicesync

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func fixture(t *testing.T, name string, depth int) []byte {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "testdata", "redaction", name))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(redacttest.Fill(string(raw), depth))
}

// requireRedacted checks what the server holds for a source: the same
// length as the raw bytes, no planted secret, every line still JSON (and
// every nested JSON string still JSON), and a redaction record.
func (e *env) requireRedacted(path, fileID string, raw []byte, jsonl bool) {
	e.t.Helper()
	got, err := e.srv.Reconstruct(path, fileID, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(got) != len(raw) {
		e.t.Fatalf("%s: server holds %d bytes, raw is %d", filepath.Base(path), len(got), len(raw))
	}
	for name, needle := range redacttest.Needles() {
		if bytes.Contains(raw, []byte(needle)) && bytes.Contains(got, []byte(needle)) {
			e.t.Errorf("%s: planted %s reached the server", filepath.Base(path), name)
		}
	}
	if jsonl {
		for i, line := range bytes.Split(bytes.TrimSuffix(got, []byte("\n")), []byte("\n")) {
			var v map[string]any
			if err := json.Unmarshal(line, &v); err != nil {
				e.t.Fatalf("%s line %d no longer parses: %v", filepath.Base(path), i+1, err)
			}
			for _, k := range []string{"chat_message", "tool_call_json"} {
				if s, ok := v[k].(string); ok && json.Unmarshal([]byte(s), new(any)) != nil {
					e.t.Fatalf("%s line %d: nested %s no longer parses", filepath.Base(path), i+1, k)
				}
			}
		}
	}
	rec := e.srv.Redaction(path, fileID)
	if rec == nil || rec.Rules != redact.RulesVersion || len(rec.Counts) == 0 {
		e.t.Fatalf("%s: redaction record %+v", filepath.Base(path), rec)
	}
}

// Planted secrets in every harness format never reach the server, whether
// the upload reads the bytes at capture or later from the file (the
// server was down at capture).
func TestPlantedSecretsNeverUploaded(t *testing.T) {
	for _, down := range []bool{false, true} {
		e := newEnv(t, Config{}, 1<<20)
		ctx := context.Background()
		cases := []struct {
			file  string
			depth int
			spec  SourceSpec
		}{
			{"claude.jsonl", 1, SourceSpec{Path: e.path("claude.jsonl"), Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}},
			{"codex.jsonl", 1, SourceSpec{Path: e.path("codex.jsonl"), Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"}},
			{"tool-result.txt", 0, SourceSpec{Path: e.path("s/tool-results/out.txt"), Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: e.path("claude.jsonl")}},
		}
		e.srv.SetDown(down)
		for _, c := range cases {
			os.MkdirAll(filepath.Dir(c.spec.Path), 0o700)
			if err := os.WriteFile(c.spec.Path, fixture(t, c.file, c.depth), 0o600); err != nil {
				t.Fatal(err)
			}
			e.sy.Sync(ctx, c.spec)
		}
		devin := SourceSpec{Path: e.path("sessions.db") + "#devin-redact-001", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin@1", Export: true}
		export := fixture(t, "devin-export.jsonl", 1)
		e.sy.SyncExport(ctx, devin, export)
		e.srv.SetDown(false)
		for _, c := range cases {
			e.sync(c.spec)
			raw, _ := os.ReadFile(c.spec.Path)
			e.requireRedacted(c.spec.Path, fileIDOf(t, c.spec.Path), raw, c.file != "tool-result.txt")
		}
		if err := e.sy.SyncExport(ctx, devin, export); err != nil {
			t.Fatal(err)
		}
		e.requireRedacted(devin.Path, "", export, true)
	}
}

// A line still being written is not uploaded until its newline arrives:
// half a token must not reach the server unredacted. Once the file goes
// idle, only captured complete records are sealed. The partial line waits.
func TestPartialLineWithheldUntilComplete(t *testing.T) {
	now := time.Now()
	e := newEnv(t, Config{Now: func() time.Time { return now }, SealAfter: time.Minute}, 1<<20)
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	tok := redacttest.Needles()["GITHUB_PAT"]
	first := `{"type":"x","text":"ok"}` + "\n"
	half := `{"type":"x","text":"token ` + tok[:20]
	appendFile(t, sp.Path, []byte(first+half))
	e.sync(sp)
	id := fileIDOf(t, sp.Path)
	e.requireServerHas(sp.Path, id, 0, []byte(first))
	if !e.sy.provisional(context.Background(), sp.Path) {
		t.Fatal("a withheld partial line must keep the seal check scheduled")
	}

	appendFile(t, sp.Path, []byte(tok[20:]+` end"}`+"\n"))
	e.sync(sp)
	got, _ := e.srv.Reconstruct(sp.Path, id, 0)
	raw, _ := os.ReadFile(sp.Path)
	if len(got) != len(raw) || bytes.Contains(got, []byte(tok[:12])) || !bytes.Contains(got, []byte("[REDACTED:github-token:")) {
		t.Fatalf("completed line: %q", got)
	}

	// Idle seals the complete prefix while retaining the partial record locally.
	appendFile(t, sp.Path, []byte(`{"type":"x","text":"tail`))
	e.sync(sp)
	if got, _ := e.srv.Reconstruct(sp.Path, id, 0); len(got) == len(raw)+len(`{"type":"x","text":"tail`) {
		t.Fatal("partial line uploaded before idle")
	}
	now = now.Add(time.Hour)
	e.sync(sp)
	raw, _ = os.ReadFile(sp.Path)
	e.requireServerHas(sp.Path, id, 0, redactAll(t, raw[:len(raw)-len(`{"type":"x","text":"tail`)], redact.Lines))
	if e.sy.provisional(context.Background(), sp.Path) {
		t.Fatal("an unfinished record kept the idle seal timer armed")
	}
}

func redactAll(t *testing.T, raw []byte, m redact.Mode) []byte {
	t.Helper()
	out := make([]byte, len(raw))
	if n, err := redact.NewReaderAt(bytes.NewReader(raw), m).ReadAt(out, 0); n != len(raw) {
		t.Fatalf("redact: %d of %d: %v", n, len(raw), err)
	}
	return out
}

// The agent's status sums what each source's current generation masked.
func TestRedactionTotals(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("a.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, fixture(t, "codex.jsonl", 1))
	e.sync(sp)
	counts, srcs, err := e.store.RedactionTotals(context.Background())
	if err != nil || srcs != 1 || counts["aws-access-key"] != 1 || counts["private-key"] != 1 {
		t.Fatalf("totals %v %d %v", counts, srcs, err)
	}
	// Appending counts only the new bytes; a rewrite starts over.
	appendFile(t, sp.Path, []byte(`{"text":"`+redacttest.Needles()["GITHUB_PAT"]+`"}`+"\n"))
	e.sync(sp)
	if counts, _, _ = e.store.RedactionTotals(context.Background()); counts["github-token"] != 1 || counts["aws-access-key"] != 1 {
		t.Fatalf("after append %v", counts)
	}
	os.WriteFile(sp.Path, []byte(`{"text":"`+redacttest.Needles()["GITHUB_PAT"]+`"}`+"\n"), 0o600)
	e.sync(sp)
	if counts, _, _ = e.store.RedactionTotals(context.Background()); counts["github-token"] != 1 || counts["aws-access-key"] != 0 {
		t.Fatalf("after rewrite %v", counts)
	}
}
