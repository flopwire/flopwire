package local

import (
	"os"
	"regexp"
	"slices"
	"strconv"
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
// Fixed patterns are smoke cases, not assumptions about this machine's text.
// A complete non-FTS reference establishes grep presence, and visible retained
// text supplies guaranteed positive witnesses for all three verbs. The test
// also checks self-exclusion and reports latency and calling-session detection.
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
	if err := s.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM conversations`).Scan(&convs); err != nil {
		t.Fatal(err)
	}
	t.Logf("index: %d messages, %d conversations", rows, convs)
	b := &Backend{Store: s}
	c, ok := NewDetector().Detect(ctx)
	t.Logf("calling session: %+v found=%v", c, ok)

	queries := []corpusQuery{
		{"find", "go test -race", false}, {"find", "internal/transcript", false}, {"find", "gh pr create", false},
		{"find", "no such file or directory", false}, {"find", "ENOENT", false}, {"find", "::", false},
		{"regex", `func \w+\(ctx context\.Context`, true}, {"regex", `exit (code|status) [1-9]`, true},
		{"regex", `\b[A-Z][a-z]+Error\b`, true}, {"regex", `v\d+\.\d+\.\d+`, true}, {"regex", `(?i)panic: .*nil pointer`, true},
		{"regex", `https?://github\.com/[\w-]+/[\w-]+/pull/\d+`, true}, {"regex", `a.b`, true},
		{"search", "sqlite fts5 trigram", false}, {"search", "worktree", false}, {"search", "exponential backoff retry", false},
		{"search", "cass search", false}, {"search", "devin session", false},
	}
	presence, witness, token, err := corpusReference(s, queries)
	if err != nil {
		t.Fatal(err)
	}
	derived := []corpusQuery{{"find", token, false}, {"regex", regexp.QuoteMeta(token), true}, {"search", token, false}}
	queries = append(queries, derived...)
	presence = append(presence, true, true, true)
	read, err := b.Read(ctx, format.ReadQuery{Address: strconv.FormatInt(witness.ID, 10), MaxChars: len(witness.Text) + 1}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	exact := false
	for _, m := range read.Messages {
		if m.ID == read.Focus && m.Text == witness.Text {
			exact = true
		}
	}
	if !exact {
		t.Fatal("derived witness read differs from stored text")
	}
	lat := map[string][]time.Duration{}
	for qi, x := range queries {
		label := x.text
		if qi >= len(queries)-3 {
			label = "derived-visible-witness"
		}
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
				t.Fatalf("%s %q: %v", x.kind, label, err)
			}
			if self == 0 && (x.kind != "search" || qi >= len(queries)-3) {
				if err := corpusPresence(presence[qi], page); err != nil {
					t.Errorf("%s query %d: %v", x.kind, qi, err)
				}
			}
			if x.kind != "search" {
				re, err := corpusPattern(x)
				if err != nil {
					t.Fatal(err)
				}
				for _, h := range hits {
					id, err := strconv.ParseInt(h.MessageID, 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					stored, err := s.Messages(ctx, []int64{id})
					if err != nil {
						t.Fatal(err)
					}
					if len(stored) != 1 || !re.MatchString(stored[0].Text) {
						t.Errorf("%s query %d returned nonmatching text", x.kind, qi)
					}
					if self == 0 && qi == len(queries)-3 && len(stored) == 1 {
						// Exercise the address returned by the positive grep, not
						// only the independent numeric witness address above.
						cx, err := b.Read(ctx, format.ReadQuery{Address: h.Address, LineOffset: 1, MaxChars: len(stored[0].Text) + 1}, format.Filters{})
						if err != nil {
							t.Fatal(err)
						}
						exact := false
						for _, m := range cx.Messages {
							if m.ID == h.MessageID && m.ID == cx.Focus && m.Text == stored[0].Text {
								exact = true
							}
						}
						if !exact {
							t.Fatal("derived grep address read differs from stored text")
						}
					}
				}
			}
			if self == 1 {
				for _, h := range hits {
					if ok && h.SessionID == c.SessionID {
						t.Errorf("%s %q: own session %s not excluded", x.kind, label, c.SessionID)
					}
				}
				lat[x.kind] = append(lat[x.kind], d)
				t.Logf("%-6s %-44q hits=%-3d %8s %s", x.kind, label, len(hits), d.Round(time.Microsecond), note)
			}
		}
	}
	for _, k := range []string{"find", "regex", "search"} {
		ds := lat[k]
		slices.Sort(ds)
		t.Logf("%-6s n=%d p50=%s max=%s", k, len(ds), ds[len(ds)/2].Round(time.Microsecond), ds[len(ds)-1].Round(time.Microsecond))
	}
}
