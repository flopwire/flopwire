package retrieval_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// FLOPWIRE_CORPUS=1 go test -run Corpus -timeout 2h ./internal/retrieval
//
// Ingests the last FLOPWIRE_CORPUS_DAYS days (default 7) of this machine's
// Claude and Codex transcripts plus every Devin session, read-only, through
// devicesync into a real server, then measures search and find latency
// through the HTTP API (auth and audit included).
func TestCorpusRetrieval(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to run against the real corpus")
	}
	days := 7
	if v, err := strconv.Atoi(os.Getenv("FLOPWIRE_CORPUS_DAYS")); err == nil && v > 0 {
		days = v
	}
	ctx := context.Background()
	s := newServerChunks(t, devicesync.DefaultChunkParams)
	s.queue.Workers = max(2, runtime.GOMAXPROCS(0)/2)
	qctx, cancel := context.WithCancel(ctx)
	go s.queue.Run(qctx)
	start := time.Now()
	files, size := syncCorpus(t, s, time.Now().Add(-time.Duration(days)*24*time.Hour))
	for s.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq AND attempts=0`) > 0 {
		time.Sleep(time.Second)
	}
	cancel()
	t.Logf("ingested %d sources, %.2f GB in %v; %d messages; %d sources failed to parse", files, float64(size)/1e9,
		time.Since(start).Round(time.Second), s.count(`SELECT count(*) FROM messages`), s.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq`))
	sizes := func(q string) string {
		var out string
		_ = s.pool.QueryRow(ctx, q).Scan(&out)
		return out
	}
	t.Logf("postgres: database %s; messages heap %s, toast %s, indexes: tsv %s, trgm %s, all %s",
		sizes(`SELECT pg_size_pretty(pg_database_size(current_database()))`), sizes(`SELECT pg_size_pretty(pg_relation_size('messages'))`),
		sizes(`SELECT pg_size_pretty(pg_total_relation_size(reltoastrelid)) FROM pg_class WHERE relname='messages'`),
		sizes(`SELECT pg_size_pretty(pg_relation_size('messages_tsv_idx'))`), sizes(`SELECT pg_size_pretty(pg_relation_size('messages_text_trgm_idx'))`),
		sizes(`SELECT pg_size_pretty(pg_indexes_size('messages'))`))

	searches := []string{"error", "panic", "migration", "postgres", "docker compose", "flaky test", "rate limit", "retry backoff",
		"worktree", "timeout", "permission denied", "race condition", "deadlock", "memory leak", "tailscale", "FastCDC",
		"\"go test\"", "useEffect", "sqlite fts5", "credential rotation"}
	finds := []struct {
		p     string
		regex bool
	}{{"exit status 1", false}, {"ECONNREFUSED", false}, {"git worktree add", false}, {"func main()", false}, {"TODO", false},
		{"pg_advisory", false}, {"context deadline exceeded", false}, {"--- FAIL", false}, {"docker compose", false}, {"panic: runtime error", false},
		{`panic: .*nil`, true}, {`[0-9]+ passed`, true}, {`FAIL\s+github\.com/`, true}, {`sha256:[0-9a-f]{12}`, true}, {`https://github\.com/[^/]+/[^/ ]+/pull/[0-9]+`, true}}
	measure := func(name string, runs int, fn func() (int, error)) {
		var lat []time.Duration
		hits := 0
		for i := 0; i < runs; i++ {
			t0 := time.Now()
			n, err := fn()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			lat = append(lat, time.Since(t0))
			hits += n
		}
		slices.Sort(lat)
		t.Logf("%s: %d runs, p50 %v, p99 %v, max %v, %.1f hits/run", name, runs, lat[len(lat)/2].Round(time.Millisecond),
			lat[(len(lat)*99)/100].Round(time.Millisecond), lat[len(lat)-1].Round(time.Millisecond), float64(hits)/float64(runs))
	}
	i := 0
	measure("search", 5*len(searches), func() (int, error) {
		q := searches[i%len(searches)]
		i++
		p, err := s.client.Search(ctx, format.SearchQuery{Query: q}, format.Filters{})
		return len(p.Hits), err
	})
	i = 0
	measure("find", 5*len(finds), func() (int, error) {
		f := finds[i%len(finds)]
		i++
		p, err := s.client.Grep(ctx, format.GrepQuery{Pattern: f.p, Fixed: !f.regex}, format.Filters{})
		return len(p.Hits), err
	})
	i = 0
	measure("find --agent codex --kind tool_result --since 72h", 5*len(finds), func() (int, error) {
		f := finds[i%len(finds)]
		i++
		p, err := s.client.Grep(ctx, format.GrepQuery{Pattern: f.p, Fixed: !f.regex}, format.Filters{Agent: "codex", Kinds: []string{"tool_result"}, Since: time.Now().Add(-72 * time.Hour)})
		return len(p.Hits), err
	})
	page, _ := s.client.Search(ctx, format.SearchQuery{Query: "error", Limit: 50}, format.Filters{})
	hits := page.Hits
	i = 0
	measure("read ±5", len(hits), func() (int, error) {
		cx, err := s.client.Read(ctx, format.ReadQuery{Address: hits[i].Address, Before: 5, After: 5}, format.Filters{})
		i++
		return len(cx.Messages), err
	})
}

func syncCorpus(t *testing.T, s *server, cutoff time.Time) (int, int64) {
	ctx := context.Background()
	home, _ := os.UserHomeDir()
	var specs, companions []devicesync.SourceSpec
	var size int64
	recent := func(p string) bool {
		fi, err := os.Stat(p)
		if err != nil || fi.ModTime().Before(cutoff) {
			return false
		}
		size += fi.Size()
		return true
	}
	sessions, _ := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	for _, sess := range sessions {
		for _, src := range sess.Sources() {
			if recent(src.Path) {
				specs = append(specs, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName})
			}
		}
		for _, c := range sess.Companions {
			parent := filepath.Join(sess.ProjectDir, sess.SessionID+".jsonl")
			if c.Role == claude.CompanionMeta {
				parent = strings.TrimSuffix(c.Path, ".meta.json") + ".jsonl"
			}
			if recent(c.Path) {
				companions = append(companions, devicesync.SourceSpec{Path: c.Path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: parent})
			}
		}
	}
	rollouts, _ := codex.Discover(codex.Home())
	for _, src := range rollouts {
		if recent(src.Path) {
			specs = append(specs, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name})
		}
	}
	for _, sp := range append(specs, companions...) {
		if err := s.sy.Sync(ctx, sp); err != nil {
			t.Errorf("sync %s: %v", sp.Path, err)
		}
	}
	db := devin.DefaultPath(home)
	ids, _ := devin.ListSessions(ctx, db)
	for _, id := range ids {
		data, err := devin.Export(ctx, db, id)
		if err == nil {
			size += int64(len(data))
			err = s.sy.SyncExport(ctx, devicesync.SourceSpec{Path: devin.ExportPath(db, id), Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat}, data)
		}
		if err != nil {
			t.Errorf("devin %s: %v", id, err)
		}
	}
	return len(specs) + len(companions) + len(ids), size
}
