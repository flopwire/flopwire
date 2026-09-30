package local

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// TestCorpus runs the retrieval verbs over an index of the real corpus
// built by localindex's TestCorpus (which indexes this machine's
// transcripts read-only):
//
//	FLOPWIRE_CORPUS=1 FLOPWIRE_CORPUS_DB=/path/index.db go test -run TestCorpus ./internal/localindex/
//	FLOPWIRE_CORPUS=1 FLOPWIRE_CORPUS_DB=/path/index.db go test -run TestCorpus -v ./internal/retrieval/local/
//
// It asserts no errors, non-empty results for common patterns (own session
// included) and no own-session hits with self-exclusion on, and reports
// latency and the calling-session detection for the shell running it.
func TestCorpus(t *testing.T) {
	path := os.Getenv("FLOPWIRE_CORPUS_DB")
	if os.Getenv("FLOPWIRE_CORPUS") == "" || path == "" {
		t.Skip("set FLOPWIRE_CORPUS=1 and FLOPWIRE_CORPUS_DB to an index built by localindex's TestCorpus")
	}
	s, err := localindex.Open(path, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var rows, convs int64
	s.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&rows)
	s.DB().QueryRow(`SELECT count(*) FROM conversations`).Scan(&convs)
	t.Logf("index: %d messages, %d conversations", rows, convs)
	b := &Backend{Store: s}
	c, ok := NewDetector().Detect(ctx)
	t.Logf("calling session: %+v found=%v", c, ok)

	type q struct {
		kind, text string
		regex      bool
		wantHits   bool
	}
	queries := []q{
		{"find", "go test -race", false, true}, {"find", "internal/transcript", false, true}, {"find", "gh pr create", false, true},
		{"find", "no such file or directory", false, true}, {"find", "ENOENT", false, false}, {"find", "::", false, false},
		{"regex", `func \w+\(ctx context\.Context`, true, true}, {"regex", `exit (code|status) [1-9]`, true, true},
		{"regex", `\b[A-Z][a-z]+Error\b`, true, true}, {"regex", `v\d+\.\d+\.\d+`, true, true}, {"regex", `(?i)panic: .*nil pointer`, true, false},
		{"regex", `https?://github\.com/[\w-]+/[\w-]+/pull/\d+`, true, true}, {"regex", `a.b`, true, true},
		{"search", "sqlite fts5 trigram", false, true}, {"search", "worktree", false, true}, {"search", "exponential backoff retry", false, true},
		{"search", "cass search", false, false}, {"search", "devin session", false, false},
	}
	lat := map[string][]time.Duration{}
	for _, x := range queries {
		for self := range 2 {
			f := format.Filters{}
			if self == 1 && ok {
				f.ExcludeSession = c.SessionID
			}
			t0 := time.Now()
			var page *format.Page
			var err error
			if x.kind == "search" {
				page, err = b.Search(ctx, format.SearchQuery{Query: x.text}, f)
			} else {
				page, err = b.Grep(ctx, format.GrepQuery{Pattern: x.text, Fixed: !x.regex}, f)
			}
			var hits []format.Hit
			var note string
			if page != nil {
				hits, note = page.Hits, strings.Join(append(page.Notes, page.Reason), "; ")
			}
			d := time.Since(t0)
			if err != nil {
				t.Fatalf("%s %q: %v", x.kind, x.text, err)
			}
			if self == 0 && x.wantHits && len(hits) == 0 {
				t.Errorf("%s %q: no hits", x.kind, x.text)
			}
			if self == 1 {
				for _, h := range hits {
					if h.SessionID == c.SessionID {
						t.Errorf("%s %q: own session %s not excluded", x.kind, x.text, c.SessionID)
					}
				}
				lat[x.kind] = append(lat[x.kind], d)
				t.Logf("%-6s %-44q hits=%-3d %8s %s", x.kind, x.text, len(hits), d.Round(time.Microsecond), note)
			}
		}
	}
	for _, k := range []string{"find", "regex", "search"} {
		ds := lat[k]
		slices.Sort(ds)
		t.Logf("%-6s n=%d p50=%s max=%s", k, len(ds), ds[len(ds)/2].Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond))
	}
}
