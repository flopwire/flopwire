package ingest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Performance guards for versioned reparse (notes/perf-guards.md, known
// violation #1). They gate rows touched and statements sent, never time.

// perfEnv is an env whose pool counts statements. Like perfguard.NewPool,
// but sequential scans are off as well: the fixtures are a few thousand
// rows, where the planner would scan whole tables that it reaches through
// an index at production size. A table no index serves is still scanned.
func perfEnv(t *testing.T) (*env, *perfguard.Counter) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	counter := perfguard.Attach(cfg)
	cfg.ConnConfig.RuntimeParams["max_parallel_workers_per_gather"] = "0"
	cfg.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return newEnvOn(t, pool), counter
}

// claudeSources uploads one synthetic Claude transcript of records
// messages per session and parses them. It returns the source ids.
func claudeSources(t *testing.T, e *env, sessions, records int) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, sessions)
	for i := range sessions {
		s := perfguard.ClaudeSession{SessionID: fmt.Sprintf("5e550000-0000-4000-8000-%012d", i+1)}
		paths[i] = filepath.Join(dir, s.SessionID+".jsonl")
		// Record uuids are unique per session, as Claude's are.
		lines := bytes.ReplaceAll(s.Lines(0, records), []byte("5e550000-0000-4000-9000-"), fmt.Appendf(nil, "5e55%04x-0000-4000-9000-", i+1))
		if err := os.WriteFile(paths[i], lines, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One device flushes one source at a time.
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	for _, p := range paths {
		sync1(t, sy, devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	}
	e.drain()
	// Production tables have planner statistics (autoanalyze); Measure
	// turns autovacuum off, so they are gathered here.
	e.exec(`ANALYZE`)
	ids := make([]string, len(paths))
	for i, p := range paths {
		if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE path=$1`, p).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.count(`SELECT count(*) FROM messages WHERE NOT superseded`); got != sessions*records {
		t.Fatalf("fixture has %d live messages, want %d", got, sessions*records)
	}
	return ids
}

// majorBump makes every source stale as a new parser major would, with
// the stored rows stamped by the old parser.
func majorBump(e *env) {
	e.exec(`UPDATE source_parse_state SET applied_parser='claude@2.0'`)
	e.exec(`UPDATE messages SET parser='claude@2.0'`)
}

// A full reparse of one conversation is linear in its messages: no digest
// recount per batch.
func TestPerfFullReparseLinear(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Linear, 500, 8, func(_ testing.TB, n int) perfguard.Cost {
		e, counter := perfEnv(t)
		id := claudeSources(t, e, 1, n)[0]
		majorBump(e)
		return perfguard.Measure(t, e.pool, counter, func() {
			if err := e.queue.refreshSource(e.ctx, id); err != nil {
				t.Fatal(err)
			}
		})
	})
}

// A full reparse whose output matches the stored rows writes none of them:
// it reads them, keeps them live and checkpoints.
func TestPerfUnchangedReparseKeepsRows(t *testing.T) {
	const n = 4000
	e, counter := perfEnv(t)
	id := claudeSources(t, e, 1, n)[0]
	e.exec(`UPDATE source_parse_state SET applied_parser='claude@2.0'`)
	cost := perfguard.Measure(t, e.pool, counter, func() {
		if err := e.queue.refreshSource(e.ctx, id); err != nil {
			t.Fatal(err)
		}
	})
	if m := cost.Tables["public.messages"]; m.TupIns != 0 || m.TupUpd != 0 || m.TupDel != 0 {
		t.Errorf("unchanged reparse wrote message rows: %s", m)
	}
	// Per 500-row batch: a lookup and a few conversation statements; no
	// statement per row.
	if limit := int64(60 + 10*n/sinkBatch); cost.Statements > limit {
		t.Errorf("unchanged reparse sent %d statements, want at most %d\n%s", cost.Statements, limit, counter)
	}
	if got := e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded`, id); got != n {
		t.Fatalf("%d live rows after unchanged reparse, want %d", got, n)
	}
	if e.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_parser='claude@2.0'`, id) != 0 {
		t.Fatal("reparse did not checkpoint")
	}
	if !t.Failed() {
		t.Logf("unchanged reparse of %d messages: %s", n, cost)
	}
}
