package local

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// TestLatency builds a synthetic index (200k rows by default) and reports
// p50/p99 latency of find (substring), find (regex) and search through the
// local backend, default filters, limit 20.
//
//	FLOPWIRE_BENCH=1 [FLOPWIRE_BENCH_ROWS=200000] [FLOPWIRE_BENCH_DB=/path.db] \
//	  go test -run TestLatency -timeout 30m -v ./internal/retrieval/local/
//
// An existing FLOPWIRE_BENCH_DB is reused as is.
func TestLatency(t *testing.T) {
	if os.Getenv("FLOPWIRE_BENCH") == "" {
		t.Skip("set FLOPWIRE_BENCH=1")
	}
	rows := 200_000
	if v, err := strconv.Atoi(os.Getenv("FLOPWIRE_BENCH_ROWS")); err == nil && v > 0 {
		rows = v
	}
	path := os.Getenv("FLOPWIRE_BENCH_DB")
	if path == "" {
		path = filepath.Join(t.TempDir(), "bench.db")
	}
	_, statErr := os.Stat(path)
	s, err := localindex.Open(path, localindex.Options{RepoRoot: func(cwd string) string { return cwd }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if statErr != nil {
		t0 := time.Now()
		buildSynthetic(t, s, rows)
		t.Logf("built %d rows in %s", rows, time.Since(t0).Round(time.Millisecond))
	}
	b := &Backend{Store: s}
	type q struct {
		kind, text string
		regex      bool
	}
	queries := []q{
		{"find", "handleRequest", false}, {"find", "connection reset", false}, {"find", "internal/api/server.go", false},
		{"find", "no such file or directory", false}, {"find", "TODO", false}, {"find", "exit status 1", false},
		{"regex", `func \w+\(ctx`, true}, {"regex", `error: .*timeout`, true}, {"regex", `[A-Z]\w+Error\b`, true},
		{"regex", `v\d+\.\d+\.\d+`, true}, {"regex", `(retry|backoff) after \d+ms`, true}, {"regex", `internal/\w+/\w+_test\.go:\d+`, true},
		{"search", "connection reset retry", false}, {"search", "upload backoff", false}, {"search", "handleRequest timeout", false},
		{"search", "flaky test fixture", false}, {"search", "migrate schema", false}, {"search", "server.go", false},
	}
	lat := map[string][]time.Duration{}
	for round := range 15 {
		for _, x := range queries {
			t0 := time.Now()
			var n int
			var err error
			switch x.kind {
			case "search":
				var p *format.Page
				p, err = b.Search(ctx, format.SearchQuery{Query: x.text}, format.Filters{})
				if p != nil {
					n = len(p.Hits)
				}
			default:
				var p *format.Page
				p, err = b.Grep(ctx, format.GrepQuery{Pattern: x.text, Fixed: !x.regex}, format.Filters{})
				if p != nil {
					n = len(p.Hits)
				}
			}
			d := time.Since(t0)
			if err != nil {
				t.Fatalf("%s %q: %v", x.kind, x.text, err)
			}
			if round == 0 {
				t.Logf("%-6s %-32q hits=%-3d first=%s", x.kind, x.text, n, d.Round(time.Microsecond))
				continue // cold round not counted
			}
			lat[x.kind] = append(lat[x.kind], d)
		}
	}
	for _, k := range []string{"find", "regex", "search"} {
		ds := lat[k]
		slices.Sort(ds)
		t.Logf("%-6s n=%d p50=%s p99=%s max=%s", k, len(ds), ds[len(ds)/2].Round(time.Microsecond),
			ds[(len(ds)*99)/100].Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond))
	}
}

// buildSynthetic writes rows transcript-like messages: Zipf prose, code
// identifiers, paths with line numbers, error lines; 100 rows per
// conversation, 10 conversations per source.
func buildSynthetic(t *testing.T, s *localindex.Store, rows int) {
	r := rand.New(rand.NewPCG(42, 7))
	syl := []string{"ka", "lo", "mi", "ne", "ru", "sa", "to", "vi", "po", "de", "an", "er", "in", "on", "st", "tr", "ch", "ex", "al", "or"}
	var words []string
	for len(words) < 20000 {
		var sb strings.Builder
		for range 1 + r.IntN(4) {
			sb.WriteString(syl[r.IntN(len(syl))])
		}
		words = append(words, sb.String())
	}
	// Domain words at moderately frequent Zipf ranks.
	words = slices.Insert(words, 60, "connection", "reset", "retry", "backoff", "upload", "timeout", "flaky", "test", "fixture", "migrate", "schema", "no such file or directory", "TODO")
	zw := rand.NewZipf(r, 1.1, 1, uint64(len(words)-1))
	idents := []string{"handleRequest", "parseConfig", "NewRouter", "uploadChunk", "retryWithBackoff", "ValidationError", "TimeoutError", "openStore"}
	pkgs := []string{"api", "store", "ingest", "transcript", "localindex", "retrieval"}
	line := func() string {
		switch r.IntN(10) {
		case 0:
			return fmt.Sprintf("internal/%s/%s.go:%d: %s", pkgs[r.IntN(len(pkgs))], []string{"server", words[r.IntN(500)] + "_test"}[r.IntN(2)], 1+r.IntN(400), words[zw.Uint64()])
		case 1:
			return fmt.Sprintf("func %s(ctx context.Context) error {", idents[r.IntN(len(idents))])
		case 2:
			return fmt.Sprintf("error: %s %s %s after %dms", words[zw.Uint64()], words[zw.Uint64()], []string{"timeout", "retry", "backoff"}[r.IntN(3)], r.IntN(5000))
		case 3:
			return fmt.Sprintf("%s: exit status %d", idents[r.IntN(len(idents))], r.IntN(3))
		case 4:
			return fmt.Sprintf("released v%d.%d.%d", r.IntN(3), r.IntN(30), r.IntN(10))
		}
		var sb strings.Builder
		for i := range 5 + r.IntN(15) {
			if i > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(words[zw.Uint64()])
		}
		return sb.String()
	}
	kinds := []transcript.Kind{transcript.KindUser, transcript.KindAssistant, transcript.KindToolCall, transcript.KindToolResult, transcript.KindThinking}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for src := 0; src*1000 < rows; src++ {
		st, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: fmt.Sprintf("/synthetic/%d.jsonl", src),
			FileID: transcript.FileID{Dev: 1, Ino: uint64(src + 1)}, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
		if err != nil {
			t.Fatal(err)
		}
		batch := localindex.Batch{SourceID: st.ID, Generation: 1}
		for i := 0; i < 1000 && src*1000+i < rows; i++ {
			sess := fmt.Sprintf("s%05d-%d", src, i/100)
			if i%100 == 0 {
				batch.Conversations = append(batch.Conversations, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: sess, Cwd: "/repo/" + pkgs[src%len(pkgs)]})
			}
			kind := kinds[r.IntN(len(kinds))]
			n := 1 + r.IntN(3)
			if kind == transcript.KindToolResult {
				n = 5 + r.IntN(40)
			}
			lines := make([]string, n)
			for j := range lines {
				lines[j] = line()
			}
			text := strings.Join(lines, "\n")
			m := &transcript.Message{SessionID: sess, NativeID: fmt.Sprintf("m%d-%d", src, i), Ordinal: int64(i), Kind: kind,
				TS: base.Add(time.Duration(src*1000+i) * time.Second), LineNo: int64(i + 1), ByteOffset: int64(i) * 1000, ByteLen: 1000,
				Parser: "claude@1", Text: text, FullLen: len(text)}
			m.ContentSHA = sha256.Sum256([]byte(text))
			batch.Messages = append(batch.Messages, m)
		}
		if _, err := s.ApplyBatch(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
}
