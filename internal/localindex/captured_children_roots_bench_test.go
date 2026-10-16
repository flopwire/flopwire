package localindex

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkCapturedClaudeChildrenRetainedRoots measures the production query
// against ordinary retained scheduler sources. The result is empty in both
// cases; only the number and length of configured roots changes.
func BenchmarkCapturedClaudeChildrenRetainedRoots(b *testing.B) {
	const sourceCount = 2048
	for _, tc := range []struct {
		name  string
		roots int
		long  bool
	}{
		{"one_short_root", 1, false},
		{"32_long_roots", 32, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			s, parents := retainedRootsFixture(b, sourceCount, tc.roots, tc.long)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.CapturedClaudeChildrenForParents(ctx, parents)
				if err != nil || len(got) != 0 {
					b.Fatalf("relations=%v error=%v; want empty relations", got, err)
				}
			}
		})
	}
}

// TestCapturedClaudeChildrenRetainedRootsAllocationBudget catches allocations
// proportional to retained rows times configured roots. The allowance leaves
// room for SQLite scanning and per-parent setup, without relying on exact counts.
func TestCapturedClaudeChildrenRetainedRootsAllocationBudget(t *testing.T) {
	const sourceCount = 256
	s, parents := retainedRootsFixture(t, sourceCount, 32, true)
	allocations := testing.AllocsPerRun(5, func() {
		got, err := s.CapturedClaudeChildrenForParents(ctx, parents)
		if err != nil || len(got) != 0 {
			t.Fatalf("relations=%v error=%v; want empty relations", got, err)
		}
	})
	// Ordinary rows cost approximately four allocations each. The former
	// root+separator expression added another 32 allocations for every row.
	const budget = 4096
	if allocations > budget {
		t.Fatalf("retained-root lookup allocated %.0f times; budget=%d for %d rows and 32 roots", allocations, budget, sourceCount)
	}
}

func retainedRootsFixture(t testing.TB, sourceCount, rootCount int, longRoots bool) (*Store, map[string][]string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"), Options{DeviceID: "synthetic-device", DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	parents := make(map[string][]string, rootCount)
	for i := 0; i < rootCount; i++ {
		root := "/synthetic/projects"
		if longRoots {
			root = fmt.Sprintf("/synthetic/Library/Application Support/Claude/local-agent-mode-sessions/%032x/%032x/cowork/projects/", i+1, i+1)
			root += strings.Repeat("r", 160-len(root))
		}
		parents[fmt.Sprintf("12345678-abcd-4321-9876-%012x", i+1)] = []string{root}
	}
	// Keep the production scheduler schema, including its composite
	// generation primary key, so the query plan reflects real stores.
	if err := s.write(ctx, func(w *writeTx) error {
		for _, stmt := range []string{
			`CREATE TABLE devsync_sources (
 id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, spec TEXT NOT NULL,
 generation INTEGER NOT NULL, protected_origin TEXT NOT NULL DEFAULT '',
 protected_root TEXT NOT NULL DEFAULT '', watermark TEXT)`,
			`CREATE TABLE devsync_gens (
 source_id INTEGER NOT NULL, generation INTEGER NOT NULL, file_id TEXT NOT NULL,
 previous TEXT, parent TEXT, size INTEGER NOT NULL, change_time INTEGER NOT NULL,
 captured_at INTEGER NOT NULL, entries INTEGER NOT NULL, tail_offset INTEGER NOT NULL,
 tail_size INTEGER NOT NULL, tail_hash BLOB, acked INTEGER NOT NULL,
 tail_acked INTEGER NOT NULL, closed INTEGER NOT NULL, lost INTEGER NOT NULL,
 srv_tail_off INTEGER NOT NULL, srv_tail_size INTEGER NOT NULL,
 capture_proof TEXT, redactions TEXT, PRIMARY KEY(source_id,generation))`,
		} {
			if _, err := w.exec(stmt); err != nil {
				return err
			}
		}
		spec := map[string]string{"Agent": "claude", "StorageKind": "jsonl_append", "Parser": "claude@1", "SessionKey": "", "SyntheticPadding": ""}
		encoded, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		spec["SyntheticPadding"] = strings.Repeat("x", 400-len(encoded))
		encoded, err = json.Marshal(spec)
		if err != nil {
			return err
		}
		for i := 1; i <= sourceCount; i++ {
			path := fmt.Sprintf("/synthetic/ordinary-projects/project/session-%06d.jsonl", i)
			if _, err := w.exec(`INSERT INTO devsync_sources(id,path,spec,generation) VALUES(?,?,?,1)`, i, path, string(encoded)); err != nil {
				return err
			}
			if _, err := w.exec(`INSERT INTO devsync_gens(source_id,generation,file_id,size,change_time,captured_at,entries,tail_offset,tail_size,acked,tail_acked,closed,lost,srv_tail_off,srv_tail_size) VALUES(?,1,'',400,0,0,0,0,0,0,0,0,0,0,0)`, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	return s, parents
}
