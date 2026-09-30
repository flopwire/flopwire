package ingest

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

type plantedSource struct {
	file  string
	depth int
	spec  devicesync.SourceSpec
}

func plantedSources(home string) []plantedSource {
	return []plantedSource{
		{"claude.jsonl", 1, devicesync.SourceSpec{Path: filepath.Join(home, ".claude/projects/-workspace-redaction/20000000-0000-4000-8000-000000000010.jsonl"),
			Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, SessionKey: "20000000-0000-4000-8000-000000000010", Parser: claude.ParserName}},
		{"codex.jsonl", 1, devicesync.SourceSpec{Path: filepath.Join(home, ".codex/sessions/2026/09/30/rollout-2026-09-30T12-00-00-20000000-0000-4000-8000-0000000000c1.jsonl"),
			Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}},
		{"devin-export.jsonl", 1, devicesync.SourceSpec{Path: devin.ExportPath(filepath.Join(home, ".local/share/devin/cli/sessions.db"), "devin-redact-001"),
			Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat, Export: true}},
	}
}

func plantedFixture(t *testing.T, name string, depth int) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "redaction", name))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(redacttest.Fill(string(raw), depth))
}

// requireNoPlantedRows checks that no message row of any agent holds a
// planted secret, and that every agent produced rows.
func (e *env) requireNoPlantedRows() {
	e.t.Helper()
	for _, agent := range []string{"claude", "codex", "devin"} {
		if n := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent=$1`, agent); n == 0 {
			e.t.Fatalf("no %s rows parsed", agent)
		}
	}
	rows, err := e.pool.Query(e.ctx, `SELECT text, COALESCE(enrichment::text,'') FROM messages`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var text, enr string
		if err := rows.Scan(&text, &enr); err != nil {
			e.t.Fatal(err)
		}
		for name, needle := range redacttest.Needles() {
			if strings.Contains(text, needle) || strings.Contains(enr, needle) {
				e.t.Errorf("planted %s is in a message row", name)
			}
		}
	}
}

// An agent that does not redact (or the pre-redaction archive) uploads
// raw bytes: the server's own pass masks them before they become rows,
// counts what it caught, and the admin status reports the source as
// unredacted.
func TestServerRedactsUnredactedUpload(t *testing.T) {
	e := newEnv(t)
	for _, ps := range plantedSources("/home/dev") {
		data := plantedFixture(t, ps.file, ps.depth)
		h := syncproto.FlushHeader{Version: syncproto.Version, Generation: 0, CapturedAt: time.Now().UTC(),
			Source:  syncproto.Source{Path: ps.spec.Path, FileID: "1:1", Agent: string(ps.spec.Agent), StorageKind: string(ps.spec.StorageKind), Parser: ps.spec.Parser},
			Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10},
			Entries: []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Sum(data), Offset: 0, Size: int64(len(data))}}}
		body, z := syncproto.EncodeBody(data)
		h.Bodies = []syncproto.Body{body}
		if _, err := e.client.Flush(e.ctx, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(z)}); err != nil {
			t.Fatalf("flush %s: %v", ps.file, err)
		}
	}
	e.drain()
	e.requireNoPlantedRows()
	st, err := e.queue.Redactions(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SourcesUnredacted != 3 || st.Server["github-token"] == 0 || st.Server["private-key"] == 0 || len(st.Device) != 0 {
		t.Fatalf("status %+v", st)
	}
	// A re-parse replaces the server count rather than adding to it.
	before := st.Server["private-key"]
	e.exec(`UPDATE source_parse_state SET requested_seq=requested_seq+1, reparse=true`)
	e.drain()
	if st, _ = e.queue.Redactions(e.ctx); st.Server["private-key"] != before {
		t.Fatalf("server count after re-parse: %d, was %d", st.Server["private-key"], before)
	}
}

// A redacting device: the archive holds only redacted bytes, the server's
// pass finds nothing more, and the device's counts reach the admin status.
func TestDeviceRedactsBeforeUpload(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	sy := e.syncer(devicesync.Config{})
	ctx := context.Background()
	for _, ps := range plantedSources(home) {
		data := plantedFixture(t, ps.file, ps.depth)
		if ps.spec.Export {
			if err := sy.SyncExport(ctx, ps.spec, data); err != nil {
				t.Fatal(err)
			}
			continue
		}
		os.MkdirAll(filepath.Dir(ps.spec.Path), 0o700)
		if err := os.WriteFile(ps.spec.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := sy.Sync(ctx, ps.spec); err != nil {
			t.Fatal(err)
		}
	}
	e.drain()
	e.requireNoPlantedRows()
	// Nothing planted is in object storage or a provisional tail.
	rows, err := e.pool.Query(e.ctx, `SELECT s.id::text FROM sources s`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		g, err := LoadGeneration(e.ctx, e.pool, id, -1)
		if err != nil {
			t.Fatal(err)
		}
		r := NewReader(e.ctx, e.objects, g)
		all := make([]byte, r.Size())
		r.ReadAt(all, 0)
		for name, needle := range redacttest.Needles() {
			if bytes.Contains(all, []byte(needle)) {
				t.Errorf("planted %s is in the archive", name)
			}
		}
	}
	st, err := e.queue.Redactions(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SourcesUnredacted != 0 || st.SourcesRedacted != 3 || st.Device["github-token"] == 0 || st.Rules[redact.RulesVersion] != 3 {
		t.Fatalf("status %+v", st)
	}
	for rule, n := range st.Server {
		if n != 0 {
			t.Errorf("server pass masked %d %s in device-redacted bytes", n, rule)
		}
	}
}
