package ingest

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// FLOPWIRE_CORPUS=1 go test -run Corpus -timeout 2h ./internal/ingest
//
// Syncs a slice of the real corpus on this machine, read-only, through
// devicesync to a real server (Postgres + MinIO from the test env): Claude
// and Codex transcripts changed in the last FLOPWIRE_CORPUS_DAYS days
// (default 7) with their companions, plus every Devin session as exports.
// Reports ingest throughput, parse throughput, Postgres size, object bytes
// and row counts. Retrieval latency is measured by the retrieval package.
func TestCorpusIngest(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to run against the real corpus")
	}
	days := 7
	if v, err := strconv.Atoi(os.Getenv("FLOPWIRE_CORPUS_DAYS")); err == nil && v > 0 {
		days = v
	}
	e := newEnv(t)
	// FLOPWIRE_CORPUS_RULES (';'-separated admin path rules) and
	// FLOPWIRE_CORPUS_UNPLACEABLE exercise server-side enforcement.
	var rules []string
	if v := os.Getenv("FLOPWIRE_CORPUS_RULES"); v != "" {
		rules = strings.Split(v, ";")
	}
	floor := os.Getenv("FLOPWIRE_CORPUS_UNPLACEABLE")
	if rules != nil || floor != "" {
		e.setRules(floor, rules...)
	}
	specs, bytes := corpusSpecs(t, time.Now().Add(-time.Duration(days)*24*time.Hour))
	sy := e.syncer(devicesync.Config{Chunk: devicesync.DefaultChunkParams})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.queue.Workers = runtime.GOMAXPROCS(0) / 2
	go e.queue.Run(ctx)

	start := time.Now()
	var errs int
	for _, sp := range specs {
		if err := sy.Sync(ctx, sp); err != nil {
			errs++
			t.Errorf("sync %s: %v", sp.Path, err)
		}
	}
	devinDB := devin.DefaultPath(os.Getenv("HOME"))
	sessions, _ := devin.ListSessions(ctx, devinDB)
	for _, id := range sessions {
		data, err := devin.Export(ctx, devinDB, id)
		if err == nil {
			err = sy.SyncExport(ctx, devicesync.SourceSpec{Path: devin.ExportPath(devinDB, id), Agent: transcript.AgentDevin,
				StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat}, data)
			bytes += int64(len(data))
		}
		if err != nil {
			errs++
			t.Errorf("devin %s: %v", id, err)
		}
	}
	synced := time.Since(start)
	for {
		time.Sleep(time.Second)
		if e.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq AND attempts=0`) == 0 {
			break
		}
	}
	parsed := time.Since(start)
	cancel()
	failed := e.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq`)
	var dbSize, objBytes, msgBytes, tails int64
	var chunks, msgs, convs int
	q := func(sql string, dst ...any) {
		if err := e.pool.QueryRow(context.Background(), sql).Scan(dst...); err != nil {
			t.Fatal(err)
		}
	}
	q(`SELECT pg_database_size(current_database())`, &dbSize)
	q(`SELECT count(*), COALESCE(sum(size),0) FROM chunks`, &chunks, &objBytes)
	q(`SELECT COALESCE(sum(octet_length(bytes)),0) FROM provisional_tails`, &tails)
	q(`SELECT count(*), pg_total_relation_size('messages') FROM messages`, &msgs, &msgBytes)
	q(`SELECT count(*) FROM conversations`, &convs)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("corpus slice: %d days, %d files + %d devin sessions, %.2f GB", days, len(specs), len(sessions), float64(bytes)/1e9)
	t.Logf("sync wall %v (%.1f MB/s); sync+parse wall %v (%.1f MB/s)", synced.Round(time.Second), float64(bytes)/1e6/synced.Seconds(),
		parsed.Round(time.Second), float64(bytes)/1e6/parsed.Seconds())
	t.Logf("objects: %d chunks, %.2f GB; provisional tails %.1f MB", chunks, float64(objBytes)/1e9, float64(tails)/1e6)
	t.Logf("postgres: %.2f GB total, messages %.2f GB; %d messages, %d conversations; %d sources failed to parse",
		float64(dbSize)/1e9, float64(msgBytes)/1e9, msgs, convs, failed)
	t.Logf("go heap sys %d MB", ms.Sys>>20)
	if rules != nil || floor != "" {
		refused, _ := e.queue.Refused(context.Background())
		r := mustRules(t, e)
		rows, err := e.pool.Query(context.Background(), `SELECT c.agent,COALESCE(c.cwd,''),COALESCE(c.extra->'git'->>'repository_url',''),s.path
			FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.hidden_at IS NULL`)
		if err != nil {
			t.Fatal(err)
		}
		leaked := 0
		for rows.Next() {
			var agent, cwd, remote, path string
			if err := rows.Scan(&agent, &cwd, &remote, &path); err != nil {
				t.Fatal(err)
			}
			if r.decide(deviceDirs{}, agent, path, cwd, remote).Mode != pathpolicy.Allow {
				leaked++
			}
		}
		rows.Close()
		t.Logf("path rules %q unplaceable %q: %d sources refused, %d stored conversations the rules cover", rules, floor, refused, leaked)
		if leaked > 0 {
			t.Errorf("%d stored conversations are covered by the admin rules", leaked)
		}
	}
	if errs > 0 || failed > 0 || msgs < 1000 {
		var last []string
		rows, _ := e.pool.Query(context.Background(), `SELECT last_error FROM source_parse_state WHERE last_error<>'' LIMIT 5`)
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			last = append(last, s)
		}
		rows.Close()
		t.Fatalf("errors %d, failed parses %d, messages %d; %q", errs, failed, msgs, last)
	}
}

// corpusSpecs lists the device's Claude and Codex sources changed since
// cutoff, companions after their transcripts.
func corpusSpecs(t *testing.T, cutoff time.Time) ([]devicesync.SourceSpec, int64) {
	home, _ := os.UserHomeDir()
	var out, companions []devicesync.SourceSpec
	var total int64
	recent := func(p string) bool {
		fi, err := os.Stat(p)
		if err != nil || fi.ModTime().Before(cutoff) {
			return false
		}
		total += fi.Size()
		return true
	}
	sessions, err := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		for _, src := range s.Sources() {
			if recent(src.Path) {
				out = append(out, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend,
					SessionKey: src.SessionKey, Parser: claude.ParserName})
			}
		}
		main := filepath.Join(s.ProjectDir, s.SessionID+".jsonl")
		for _, c := range s.Companions {
			parent := main
			if c.Role == claude.CompanionMeta {
				parent = strings.TrimSuffix(c.Path, ".meta.json") + ".jsonl"
			}
			if recent(c.Path) {
				companions = append(companions, devicesync.SourceSpec{Path: c.Path, Agent: transcript.AgentClaude,
					StorageKind: transcript.StorageCompanion, Parent: parent})
			}
		}
	}
	srcs, err := codex.Discover(codex.Home())
	if err != nil && !os.IsNotExist(err) && !strings.Contains(err.Error(), fs.ErrNotExist.Error()) {
		t.Fatal(err)
	}
	for _, src := range srcs {
		if recent(src.Path) {
			out = append(out, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name})
		}
	}
	return append(out, companions...), total
}
