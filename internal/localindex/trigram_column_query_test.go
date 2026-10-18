package localindex_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	li "github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/grep"
	"github.com/flopwire/flopwire/internal/retrieval/regexq"
	"github.com/flopwire/flopwire/internal/transcript"
)

const columnQueryPath = "quokka/saffron/zircon.go"
const columnQueryTail = "end-of-tool-output:quokka/zircon/final-result.go"

// Every query is bounded, but these tests assert semantics rather than latency.
var columnQueryBudget = li.Budget{Timeout: 5 * time.Second, VerifyBytes: 1 << 20}

type columnQueryMessage struct {
	id, text string
	order    int
}

func columnQueryCollisions() []columnQueryMessage {
	return []columnQueryMessage{
		{"old-match", "old " + columnQueryPath, 1},
		{"old-collision", "first collision " + columnQueryFragments(columnQueryPath), 2},
		{"new-match", "new " + columnQueryPath, 3},
		{"new-collision", "second collision " + columnQueryFragments(columnQueryPath), 4},
	}
}

// Separated overlapping triples have all the needle's trigrams, but never its
// phrase. The fixture deliberately exposes extra work without changing results.
func columnQueryFragments(text string) string {
	r := []rune(text)
	var parts []string
	for i := 0; i+3 <= len(r); i++ {
		parts = append(parts, string(r[i:i+3]))
	}
	return strings.Join(parts, " | ")
}

func openColumnQueryFixture(t *testing.T, detail li.Detail, rows []columnQueryMessage) *li.Store {
	t.Helper()
	s, err := li.Open(filepath.Join(t.TempDir(), "index.db"), li.Options{TokDetail: li.DetailFull, TriDetail: detail, TriParts: 2, ReadConns: 2, RepoRoot: func(string) string { return "/synthetic" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentCodex, Path: "/synthetic/column-query.jsonl", FileID: transcript.FileID{Dev: 1, Ino: 1}, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"})
	if err != nil {
		t.Fatal(err)
	}
	var messages []*transcript.Message
	for i, row := range rows {
		messages = append(messages, &transcript.Message{SessionID: "column-query", NativeID: row.id, Ordinal: transcript.OrdinalAt(int64(i), 0), Kind: transcript.KindToolResult, TS: time.Unix(1750000000+int64(row.order), 0), LineNo: int64(i + 1), ByteOffset: int64(i * 100), ByteLen: int64(len(row.text)), Parser: "codex@1", Text: row.text, FullLen: len(row.text), ContentSHA: sha256.Sum256([]byte(row.text))})
	}
	_, err = s.ApplyBatch(ctx, li.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentCodex, SessionID: "column-query", Cwd: "/synthetic"}}, Messages: messages})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func columnQueryIDs(hits []li.FindHit) []string {
	var ids []string
	for _, hit := range hits {
		ids = append(ids, hit.NativeID)
	}
	return ids
}
func requireColumnQueryIDs(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered matches = %v, want %v", got, want)
	}
}

func TestTrigramColumnLiteralExactness(t *testing.T) {
	for _, detail := range []li.Detail{li.DetailFull, li.DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			s := openColumnQueryFixture(t, detail, columnQueryCollisions())
			result, err := s.Grep(context.Background(), columnQueryPath, li.FindOptions{Limit: 20, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			requireColumnQueryIDs(t, columnQueryIDs(result.Hits), []string{"new-match", "old-match"})
			wantCandidates := 2
			if detail == li.DetailColumn {
				wantCandidates = 4
			}
			if result.Truncated || result.Checked != wantCandidates || result.Total != wantCandidates {
				t.Fatalf("candidate accounting = %+v, want %d complete candidates", result.Partial, wantCandidates)
			}

			// Query the actual path token: the tokenizer keeps slash and dot characters.
			// Separately declared ordered hits prevent equal wrong results in both arms.
			for _, order := range []string{"newest", "oldest"} {
				ranked, err := s.Rank(context.Background(), columnQueryPath, li.SearchOptions{Phrases: []string{columnQueryPath}, ByTime: order, Limit: 20, Budget: columnQueryBudget})
				if err != nil {
					t.Fatal(err)
				}
				var ids []string
				for _, h := range ranked.Hits {
					ids = append(ids, h.NativeID)
				}
				want := []string{"new-match", "old-match"}
				if order == "oldest" {
					want = []string{"old-match", "new-match"}
				}
				requireColumnQueryIDs(t, ids, want)
				if ranked.Truncated || ranked.RankCapped {
					t.Fatalf("small quoted-path result incomplete: %+v", ranked)
				}
			}
			ranked, err := s.Rank(context.Background(), columnQueryPath, li.SearchOptions{Phrases: []string{columnQueryPath}, Limit: 20, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			if len(ranked.Hits) != 2 {
				t.Fatalf("ranked path hits = %d, want 2", len(ranked.Hits))
			}
			for _, h := range ranked.Hits {
				if h.Score >= 0 {
					t.Fatalf("token-full BM25 score = %g, want negative score", h.Score)
				}
			}
		})
	}
}

func TestTrigramColumnUnicodeAndLongBody(t *testing.T) {
	padding := strings.Repeat("ordinary tool output\n", 1+(256<<10)/len("ordinary tool output\n"))
	huge := padding + columnQueryTail
	rows := []columnQueryMessage{
		{"greek-lower", "κόσμος lower", 1}, {"greek-upper", "ΚΌΣΜΟΣ upper", 2},
		{"ascii-lower", "sky lower", 3}, {"long-s", "ſky fold", 4}, {"ascii-upper", "SKY upper", 5},
		{"accent-lower", "naïve lower", 6}, {"accent-upper", "NAÏVE upper", 7},
		{"huge-tool", huge, 8}, {"tail-collision", columnQueryFragments(columnQueryTail), 9},
	}
	for _, detail := range []li.Detail{li.DetailFull, li.DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			s := openColumnQueryFixture(t, detail, rows)
			cases := []struct {
				name, pattern string
				ignore        bool
				want          []string
			}{
				{"greek-insensitive", "κόσμος", false, []string{"greek-upper", "greek-lower"}},
				{"greek-smart-case", "ΚΌΣΜΟΣ", false, []string{"greek-upper"}},
				{"greek-explicit-ignore-case", "ΚΌΣΜΟΣ", true, []string{"greek-upper", "greek-lower"}},
				{"simple-fold-ascii", "sky", false, []string{"ascii-upper", "long-s", "ascii-lower"}},
				{"simple-fold-long-s", "ſky", true, []string{"ascii-upper", "long-s", "ascii-lower"}},
				{"ascii-smart-case", "SKY", false, []string{"ascii-upper"}},
				{"accent-fold", "naïve", false, []string{"accent-upper", "accent-lower"}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					q := format.GrepQuery{}
					if err := (grep.Spec{Patterns: []string{tc.pattern}, Fixed: true, IgnoreCase: tc.ignore}).Query(&q); err != nil {
						t.Fatal(err)
					}
					result, err := s.Grep(context.Background(), q.Pattern, li.FindOptions{CaseSensitive: q.CaseSensitive, Limit: 20, Budget: columnQueryBudget})
					if err != nil {
						t.Fatal(err)
					}
					requireColumnQueryIDs(t, columnQueryIDs(result.Hits), tc.want)
					if result.Truncated {
						t.Fatalf("positive Unicode result incomplete: %+v", result.Partial)
					}
				})
			}
			result, err := s.Grep(context.Background(), columnQueryTail, li.FindOptions{CaseSensitive: true, Limit: 20, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			requireColumnQueryIDs(t, columnQueryIDs(result.Hits), []string{"huge-tool"})
			if result.Truncated || len(result.Hits) != 1 {
				t.Fatalf("long-tail query incomplete: %+v", result.Partial)
			}
			hit := result.Hits[0]
			if len(hit.Text) < 256<<10 || hit.Text != huge || hit.FullLen != len(huge) {
				t.Fatalf("long tool body lost coverage: text=%d full=%d want=%d", len(hit.Text), hit.FullLen, len(huge))
			}
			if len(hit.Matches) != 1 || !strings.Contains(hit.Matches[0].Text, columnQueryTail) {
				t.Fatalf("tail match missing from reported line: %+v", hit.Matches)
			}
		})
	}
}

func columnRegexPlan(t *testing.T, pattern string, multiline bool) *regexq.Plan {
	t.Helper()
	q := format.GrepQuery{Multiline: multiline}
	if err := (grep.Spec{Patterns: []string{pattern}}).Query(&q); err != nil {
		t.Fatal(err)
	}
	plan, err := grep.Compile(q)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Indexed() {
		t.Fatal("fixture must exercise indexed regex candidates")
	}
	return plan
}

func TestTrigramColumnRegexExactnessAndCandidateLimit(t *testing.T) {
	rows := append(columnQueryCollisions(), columnQueryMessage{"regex-match", "alternate-4821 ready", 5}, columnQueryMessage{"regex-collision", "alternate-4ABC", 6}, columnQueryMessage{"multiline", "begin-4821\nmiddle\nfinish-4821", 7})
	for _, detail := range []li.Detail{li.DetailFull, li.DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			s := openColumnQueryFixture(t, detail, rows)
			plan := columnRegexPlan(t, regexp.QuoteMeta(columnQueryPath)+"|alternate-[0-9]{4}", false)
			matches, stats, err := regexq.Find(context.Background(), s, plan, regexq.Options{Limit: 20, MaxCandidates: 100, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, m := range matches {
				ids = append(ids, m.NativeID)
			}
			requireColumnQueryIDs(t, ids, []string{"regex-match", "new-match", "old-match"})
			if stats.Unindexed || stats.Truncated || stats.Total < len(ids) {
				t.Fatalf("indexed regex result incomplete: %+v", stats)
			}

			limited, stats, err := regexq.Find(context.Background(), s, plan, regexq.Options{Limit: 20, MaxCandidates: 1, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			if len(limited) != 0 || stats.Unindexed || !stats.Truncated || stats.Reason != li.ReasonCandidateLimit || stats.Candidates != 1 || stats.Total <= 1 {
				t.Fatalf("candidate limit not reported truthfully: hits=%d stats=%+v", len(limited), stats)
			}

			multi := columnRegexPlan(t, "begin-4821.*finish-4821", true)
			matches, stats, err = regexq.Find(context.Background(), s, multi, regexq.Options{Limit: 20, MaxCandidates: 100, Budget: columnQueryBudget})
			if err != nil {
				t.Fatal(err)
			}
			ids = nil
			for _, m := range matches {
				ids = append(ids, m.NativeID)
			}
			requireColumnQueryIDs(t, ids, []string{"multiline"})
			if stats.Truncated {
				t.Fatalf("multiline result incomplete: %+v", stats)
			}
		})
	}
}

func TestTrigramColumnIndexedBudgetsAndCancellation(t *testing.T) {
	for _, detail := range []li.Detail{li.DetailFull, li.DetailColumn} {
		t.Run(string(detail), func(t *testing.T) {
			s := openColumnQueryFixture(t, detail, columnQueryCollisions())
			t.Run("verification-budget", func(t *testing.T) {
				result, err := s.Grep(context.Background(), columnQueryPath, li.FindOptions{Limit: 20, Budget: li.Budget{Timeout: 5 * time.Second, VerifyBytes: 1}})
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"new-match"}
				wantTotal := 2
				if detail == li.DetailColumn {
					want = nil
					wantTotal = 4
				}
				requireColumnQueryIDs(t, columnQueryIDs(result.Hits), want)
				if !result.Truncated || result.Reason != li.ReasonVerifyBudget || result.Checked != 1 || result.Total != wantTotal {
					t.Fatalf("verification budget not reported truthfully: %+v", result.Partial)
				}
			})
			t.Run("expired-query-budget", func(t *testing.T) {
				result, err := s.Grep(context.Background(), columnQueryPath, li.FindOptions{Limit: 20, Budget: li.Budget{Timeout: time.Nanosecond, VerifyBytes: 1 << 20}})
				if err != nil {
					t.Fatal(err)
				}
				if !result.Truncated || result.Reason != li.ReasonTimeout {
					t.Fatalf("exhausted indexed timeout returned complete: %+v", result.Partial)
				}
			})
			t.Run("caller-already-canceled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err := s.Grep(ctx, columnQueryPath, li.FindOptions{Limit: 20, Budget: columnQueryBudget})
				if err == nil {
					t.Fatal("indexed query discarded caller cancellation")
				}
			})
			t.Run("caller-canceled-during-indexed-scan", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				plan := columnRegexPlan(t, "quokka", false)
				visited := 0
				part, err := s.Scan(ctx, plan.Query.TrigramQuery(), li.ScanOptions{Budget: columnQueryBudget}, func(*li.Row) bool { visited++; cancel(); return true })
				if visited != 1 || part.Checked != 1 || part.Total < 2 {
					t.Fatalf("did not stop after the first indexed candidate: visited=%d partial=%+v", visited, part)
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("indexed scan error = %v, want caller cancellation; partial=%+v", err, part)
				}
			})
		})
	}
}
