package localindex

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

// TestSearchPaths checks the candidate-first ranking, its growth under
// selective filters, and the full-ranking fallback against a reference
// query that ranks every match; and the rank cap against a reference over
// the newest matches.
func TestSearchPaths(t *testing.T) {
	s := openTest(t, DetailColumn)
	ctx := context.Background()
	src := source(t, s, transcript.AgentClaude, "/h/a.jsonl")
	var convs []*transcript.Conversation
	for i := range 4 {
		cwd := fmt.Sprintf("/other/r%d", i)
		if i == 3 {
			cwd = "/repo/a" // openTest's only repo root
		}
		convs = append(convs, &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: fmt.Sprintf("s%d", i), Cwd: cwd})
	}
	var msgs []*transcript.Message
	for i := range 600 {
		// Lengths and term counts repeat, so scores tie often (ties go
		// to the newer row); repo /repo/a holds only every 50th row, so
		// filtering on it is selective.
		sess := i % 3
		if i%50 == 0 {
			sess = 3
		}
		text := strings.Repeat("alpha ", 1+i%7) + strings.Repeat("filler ", i%11)
		msgs = append(msgs, msg(fmt.Sprintf("s%d", sess), fmt.Sprintf("m%03d", i), int64(i), transcript.KindUser, text))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: convs, Messages: msgs})

	reference := func(f Filter, limit, newest int) []string {
		where, args := f.where()
		inner := `SELECT rowid, bm25(fts_tok) AS score FROM fts_tok WHERE fts_tok MATCH 'alpha'`
		if newest > 0 {
			inner = `SELECT t.rowid AS rowid, bm25(fts_tok) AS score FROM fts_tok t JOIN messages m ON m.id = t.rowid
				JOIN conversations c ON c.id = m.conversation_id WHERE fts_tok MATCH 'alpha' AND ` + where + fmt.Sprintf(` ORDER BY t.rowid DESC LIMIT %d`, newest)
		}
		q := `SELECT m.native_id FROM (` + inner + `) f JOIN messages m ON m.id = f.rowid JOIN conversations c ON c.id = m.conversation_id
			WHERE ` + where + ` ORDER BY f.score, m.id DESC LIMIT ?`
		all := append([]any(nil), args...)
		if newest > 0 {
			all = append(all, args...)
		}
		rows, err := s.rdb.QueryContext(ctx, q, append(all, limit)...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	selective := Filter{Repos: []string{"/repo/a"}}
	for _, tc := range []struct {
		name          string
		min, max, cap int
		f             Filter
		newest        int
	}{
		{"candidates suffice", 64, 1 << 15, 1 << 20, Filter{}, 0},
		{"candidates grow", 16, 1 << 15, 1 << 20, selective, 0},
		{"full ranking fallback", 16, 32, 1 << 20, selective, 0},
		{"rank cap", 256, 1 << 15, 100, Filter{}, 100},
		{"rank cap with filter", 256, 1 << 15, 100, selective, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func(a, b, c int) { searchMinCandidates, searchMaxCandidates, searchRankCap = a, b, c }(searchMinCandidates, searchMaxCandidates, searchRankCap)
			searchMinCandidates, searchMaxCandidates, searchRankCap = tc.min, tc.max, tc.cap
			got := searchIDs(t, s, "alpha", SearchOptions{Filter: tc.f, Limit: 10})
			eq(t, tc.name, got, reference(tc.f, 10, tc.newest))
			if len(got) != 10 {
				t.Fatalf("%d hits", len(got))
			}
			page2 := searchIDs(t, s, "alpha", SearchOptions{Filter: tc.f, Limit: 3, Offset: 5})
			eq(t, tc.name+" offset", page2, got[5:8])
		})
	}
}

// TestFindLongPatternWindow: a pattern longer than phraseWindow queries
// the trigram index with one window of it and verifies the rest, so rows
// holding only the window do not match.
func TestFindLongPatternWindow(t *testing.T) {
	if got := rareWindow("missing required scope read:project", phraseWindow); !strings.Contains(got, "read:proj") || len(got) != phraseWindow {
		t.Fatalf("window %q", got)
	}
	if got := rareWindow("short one", phraseWindow); got != "short one" {
		t.Fatalf("short pattern %q", got)
	}
	s := openTest(t, DetailFull)
	src := source(t, s, transcript.AgentClaude, "/h/w.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "w"}},
		Messages: []*transcript.Message{
			msg("w", "full", 0, transcript.KindToolResult, "gh: Missing Required Scope read:project for this token"),
			msg("w", "window", 1, transcript.KindToolResult, "needs read:project only"),
			msg("w", "split", 2, transcript.KindToolResult, "missing required\nscope read:project"),
		}})
	eq(t, "long pattern", findIDs(t, s, "missing required scope read:project", FindOptions{}), []string{"full"})
}
