package localindex

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func assertDefaultAdoptionLayout(t *testing.T, s *Store, tri Detail) string {
	t.Helper()
	want := Details{Tok: DetailFull, Tri: tri, TriParts: 2}
	if got := s.Details(); got != want {
		t.Fatalf("details=%+v, want %+v", got, want)
	}
	var stored Details
	var identity string
	if err := s.DB().QueryRow(`SELECT (SELECT value FROM meta WHERE key='tok_detail'),(SELECT value FROM meta WHERE key='tri_detail'),(SELECT CAST(value AS INTEGER) FROM meta WHERE key='tri_parts'),(SELECT value FROM meta WHERE key='index_id')`).Scan(&stored.Tok, &stored.Tri, &stored.TriParts, &identity); err != nil {
		t.Fatal(err)
	}
	if stored != want || identity == "" {
		t.Fatalf("stored details=%+v identity=%q", stored, identity)
	}
	for _, shard := range []struct {
		schema, table string
		detail        Detail
	}{{"tok", "fts_tok", DetailFull}, {"tri0", "fts_tri", tri}, {"tri1", "fts_tri", tri}} {
		var definition string
		if err := s.DB().QueryRow(fmt.Sprintf(`SELECT sql FROM %s.sqlite_master WHERE name=?`, shard.schema), shard.table).Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(definition, "detail="+string(shard.detail)) {
			t.Fatalf("%s actual schema: %s", shard.schema, definition)
		}
	}
	return identity
}

func TestTrigramColumnDefaultFresh(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertDefaultAdoptionLayout(t, s, DetailColumn)
}

func TestTrigramColumnDefaultPreservesExistingFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{TokDetail: DetailFull, TriDetail: DetailFull})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	identity := assertDefaultAdoptionLayout(t, s, DetailFull)
	src := source(t, s, transcript.AgentClaude, "/synthetic/retained.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "retained"}, msg("retained", "other", 0, transcript.KindUser, "unrelated retained row"), msg("retained", "message", 1, transcript.KindUser, "persistent sentinel marker"))
	var candidates int
	if err := s.DB().QueryRow(`SELECT count(*) FROM tri0.fts_tri WHERE fts_tri MATCH '"per"'`).Scan(&candidates); err != nil || candidates != 1 {
		t.Fatalf("fixture must populate the shard removed below: %d, %v", candidates, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		if missing {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Remove(path + "-tri0" + suffix); err != nil && (suffix == "" || !os.IsNotExist(err)) {
					t.Fatal(err)
				}
			}
		}
		s, err = Open(path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := assertDefaultAdoptionLayout(t, s, DetailFull); got != identity {
			t.Fatalf("missing=%t: ordinary reopen changed index identity", missing)
		}
		if got := countRows(t, s, "messages"); got != 2 {
			t.Fatalf("missing=%t: retained rows=%d", missing, got)
		}
		hits, err := s.Find(ctx, "persistent sentinel", FindOptions{})
		if err != nil || len(hits) != 1 {
			t.Fatalf("missing=%t: retained text search=%d, %v", missing, len(hits), err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTrigramColumnDefaultExplicitRebuildPreservesAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{TokDetail: DetailFull, TriDetail: DetailFull})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	oldIdentity := assertDefaultAdoptionLayout(t, s, DetailFull)
	index := func(s *Store) {
		src := source(t, s, transcript.AgentClaude, "/synthetic/rebuild.jsonl")
		sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "rebuild"},
			msg("rebuild", "secret", 0, transcript.KindUser, "hidden REBUILDSECRET text"),
			msg("rebuild", "visible", 1, transcript.KindAssistant, "visible rebuild survivor"))
	}
	index(s)
	if n, err := s.RedactMessage(ctx, LocalRedaction{Session: "rebuild", Ordinal: 0}); err != nil || n != 1 {
		t.Fatalf("redact=%d, %v", n, err)
	}
	placement := Placement{Agent: transcript.AgentClaude, SessionID: "unavailable-worktree", How: PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: "/synthetic/gone/src", Worktree: "/synthetic/gone", Main: "/synthetic/main", Remote: "github.com/example/repo"}}
	if err := s.SavePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(w *writeTx) error {
		if _, err := w.exec(`CREATE TABLE devsync_adoption_state (queue_key TEXT PRIMARY KEY,acked INTEGER)`); err != nil {
			return err
		}
		_, err := w.exec(`INSERT INTO devsync_adoption_state VALUES('original-queue',41)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	sidecars := map[string][]byte{}
	for _, suffix := range []string{".redactions.jsonl", ".redactions.key"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		sidecars[suffix] = data
	}
	s, err = Open(path, Options{RebuildIndex: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := assertDefaultAdoptionLayout(t, s, DetailColumn); got == oldIdentity {
		t.Fatal("explicit rebuild retained old index identity")
	}
	if n := countRows(t, s, "messages"); n != 0 {
		t.Fatalf("rebuild retained %d message rows", n)
	}
	var queue string
	var acked int
	if err := s.DB().QueryRow(`SELECT queue_key,acked FROM devsync_adoption_state`).Scan(&queue, &acked); err != nil || queue != "original-queue" || acked != 41 {
		t.Fatalf("queue=%q acked=%d, %v", queue, acked, err)
	}
	got, err := s.Placements(ctx)
	if err != nil || len(got) != 1 || got[0] != placement {
		t.Fatalf("placements=%+v, %v", got, err)
	}
	for suffix, want := range sidecars {
		data, err := os.ReadFile(path + suffix)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("rebuild changed redaction sidecar %s: %v", suffix, err)
		}
	}
	index(s)
	noLeak(t, s, "REBUILDSECRET", "column default after rebuild")
	hits, err := s.Find(ctx, "visible rebuild", FindOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("visible reindexed content=%d, %v", len(hits), err)
	}
}

// The direct-shard oracle must see both the token and column-detail trigram
// entries before redaction, rather than silently checking only stored text.
func TestTrigramColumnDefaultDirectShardRedactionOracle(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertDefaultAdoptionLayout(t, s, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/synthetic/leak.jsonl")
	sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "leak"}, msg("leak", "marker", 0, transcript.KindUser, "ADOPTIONLEAK"))
	if n := ftsHits(t, s, "ADOPTIONLEAK"); n != 2 {
		t.Fatalf("direct oracle must see one token and one trigram entry, got %d", n)
	}
	if n, err := s.RedactMessage(ctx, LocalRedaction{Session: "leak", Ordinal: 0}); err != nil || n != 1 {
		t.Fatalf("redact=%d, %v", n, err)
	}
	noLeak(t, s, "ADOPTIONLEAK", "column-detail direct-shard cleanup")
}

// Existing schema/mode rebuild triggers adopt the new default without adding
// an automatic format conversion to ordinary opens or missing-shard repairs.
func TestTrigramColumnDefaultExistingRebuildTriggers(t *testing.T) {
	for _, syncOnly := range []bool{false, true} {
		name := "older-schema"
		if syncOnly {
			name = "sync-only-to-full"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.db")
			s, err := Open(path, Options{TokDetail: DetailFull, TriDetail: DetailFull, SyncOnly: syncOnly})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if s != nil {
					s.Close()
				}
			}()
			var oldIdentity string
			if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key='index_id'`).Scan(&oldIdentity); err != nil {
				t.Fatal(err)
			}
			src := source(t, s, transcript.AgentClaude, "/synthetic/required-rebuild.jsonl")
			sinkMsgs(t, s, src.ID, 1, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "required"}, msg("required", "old", 0, transcript.KindUser, "old retained content"))
			placement := Placement{Agent: transcript.AgentClaude, SessionID: "preserved", How: PlacedByCwd, Placement: pathpolicy.Placement{Cwd: "/synthetic/preserved"}}
			if err := s.SavePlacement(ctx, placement); err != nil {
				t.Fatal(err)
			}
			if !syncOnly {
				// This is the generic schema-rebuild path already exercised by the
				// placement migration tests, not a newly introduced format trigger.
				if err := s.write(ctx, func(w *writeTx) error { _, err := w.exec(`PRAGMA user_version=5`); return err }); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if identity := assertDefaultAdoptionLayout(t, s, DetailColumn); identity == oldIdentity {
				t.Fatal("required rebuild retained old identity")
			}
			if n := countRows(t, s, "messages"); n != 0 {
				t.Fatalf("required rebuild kept %d old messages", n)
			}
			var mode string
			if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key='mode'`).Scan(&mode); err != nil || mode != ModeFull {
				t.Fatalf("mode=%q, %v", mode, err)
			}
			got, err := s.Placements(ctx)
			if err != nil || len(got) != 1 || got[0] != placement {
				t.Fatalf("placement=%+v, %v", got, err)
			}
		})
	}
}
