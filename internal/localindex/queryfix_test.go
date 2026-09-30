package localindex

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// bothOrderings runs a test with candidates ordered by row lookups and by
// the messages_ts walk.
func bothOrderings(t *testing.T, fn func(t *testing.T)) {
	for _, n := range []int{directLookup, 0} {
		t.Run("direct="+itoa(int64(n)), func(t *testing.T) {
			defer func(v int) { directLookup = v }(directLookup)
			directLookup = n
			fn(t)
		})
	}
}

// D7: find orders by message time, not row id, and stops at the limit
// with the newest; the unindexed scan does too.
func TestFindNewestByMessageTime(t *testing.T) { bothOrderings(t, testFindNewestByMessageTime) }

func testFindNewestByMessageTime(t *testing.T) {
	s := openTest(t, DetailFull)
	src := source(t, s, transcript.AgentClaude, "/h/time.jsonl")
	// Row ids ascend while message times descend (a long-lived session
	// indexed after newer ones).
	var ms []*transcript.Message
	for i := range 5 {
		ms = append(ms, msg("t", "t"+itoa(int64(i)), int64(100-i*10), transcript.KindUser, "ocelot "+itoa(int64(i))))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: ms})
	eq(t, "find", findIDs(t, s, "ocelot", FindOptions{}), []string{"t0", "t1", "t2", "t3", "t4"})
	eq(t, "find limit 2", findIDs(t, s, "ocelot", FindOptions{Limit: 2}), []string{"t0", "t1"})
	eq(t, "unindexed scan", findIDs(t, s, "oc", FindOptions{Limit: 2}), []string{"t0", "t1"})
	eq(t, "conversation filter", findIDs(t, s, "ocelot", FindOptions{Limit: 2, Filter: Filter{Agents: []transcript.Agent{transcript.AgentClaude}}}), []string{"t0", "t1"})
	eq(t, "conversation filter, none", findIDs(t, s, "ocelot", FindOptions{Filter: Filter{Agents: []transcript.Agent{transcript.AgentCodex}}}), nil)
	r, err := s.Grep(ctx, "ocelot", FindOptions{Limit: 2})
	if err != nil || r.Checked != 2 || r.Total != 5 || r.Truncated {
		t.Fatalf("early stop: %+v %v", r.Partial, err)
	}
}

// D7/D2: over the rank cap, search ranks the newest matches by message
// time and reports how many matched.
func TestRankCapNewestByTime(t *testing.T) {
	// Over the cap the ordering always walks messages_ts.
	defer func(c int) { searchRankCap = c }(searchRankCap)
	searchRankCap = 3
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/cap.jsonl")
	var ms []*transcript.Message
	for i := range 6 {
		// Row ids ascend, times descend: t0 is the newest.
		ms = append(ms, msg("c", "c"+itoa(int64(i)), int64(100-i*10), transcript.KindUser, "capybara "+strings.Repeat("x ", i)))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: ms})
	r, err := s.Rank(ctx, "capybara", SearchOptions{Limit: 10})
	if err != nil || !r.RankCapped || r.Total != 6 {
		t.Fatalf("rank cap: capped=%v total=%d %v", r.RankCapped, r.Total, err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.NativeID)
	}
	slices.Sort(got) // ranked by score among them
	if strings.Join(got, ",") != "c0,c1,c2" {
		t.Fatalf("ranked %v, want the three newest by time", got)
	}
}

// D8: find stops at its budget with partial hits, never an error.
func TestFindBudget(t *testing.T) { bothOrderings(t, testFindBudget) }

func testFindBudget(t *testing.T) {
	s := openTest(t, DetailFull)
	src := source(t, s, transcript.AgentClaude, "/h/budget.jsonl")
	var ms []*transcript.Message
	for i := range 20 {
		ms = append(ms, msg("b", "b"+itoa(int64(i)), int64(i), transcript.KindUser, "bandicoot "+strings.Repeat("y", 100)+itoa(int64(i))))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: ms})
	r, err := s.Grep(ctx, "bandicoot", FindOptions{Limit: 100, Budget: Budget{VerifyBytes: 500}})
	if err != nil || !r.Truncated || r.Reason != ReasonVerifyBudget || r.Total != 20 || r.Checked == 0 || r.Checked >= 20 || len(r.Hits) != r.Checked {
		t.Fatalf("verify budget: %+v hits=%d %v", r.Partial, len(r.Hits), err)
	}
	if r.Hits[0].NativeID != "b19" {
		t.Fatalf("partial hits not newest first: %s", r.Hits[0].NativeID)
	}
	r, err = s.Grep(ctx, "bandicoot", FindOptions{Budget: Budget{Timeout: time.Nanosecond}})
	if err != nil || !r.Truncated || r.Reason != ReasonTimeout {
		t.Fatalf("timeout: %+v %v", r.Partial, err)
	}
	sr, err := s.Rank(ctx, "bandicoot", SearchOptions{Budget: Budget{Timeout: time.Nanosecond}})
	if err != nil || !sr.Truncated || sr.Reason != ReasonTimeout {
		t.Fatalf("search timeout: %+v %v", sr.Partial, err)
	}
	// The caller's own cancellation is still an error.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Grep(cctx, "bandicoot", FindOptions{}); err == nil {
		t.Fatal("cancelled find returned no error")
	}
	if b := (Budget{Timeout: time.Hour}).norm(); b.Timeout != MaxTimeout || b.VerifyBytes != DefaultVerifyBytes {
		t.Fatalf("budget bounds: %+v", b)
	}
}

// D6: injected text is hidden from search and find by default, and hits
// with identical text collapse into one with a copy count.
func TestInjectedHiddenAndCopiesCollapsed(t *testing.T) {
	bothOrderings(t, testInjectedHiddenAndCopiesCollapsed)
}

func testInjectedHiddenAndCopiesCollapsed(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/dup.jsonl")
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{
		msg("a", "a1", 0, transcript.KindUser, "same wallaby text"),
		msg("b", "b1", 1, transcript.KindUser, "same wallaby text"),
		msg("c", "c1", 2, transcript.KindUser, "same wallaby text"),
		msg("c", "c2", 3, transcript.KindAssistant, "other wallaby text"),
		msg("c", "c3", 4, transcript.KindUser, "wallaby rules from AGENTS.md"),
	}})
	// The parsers emit kind "injected"; set it directly until they do.
	if err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`UPDATE messages SET kind = ? WHERE native_id = 'c3'`, KindInjected)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Find(ctx, "wallaby", FindOptions{})
	if err != nil || len(hits) != 2 || hits[0].NativeID != "c2" || hits[1].NativeID != "c1" || hits[1].Copies != 2 {
		t.Fatalf("find: %+v %v", hits, err)
	}
	sh, err := s.Search(ctx, "wallaby", SearchOptions{})
	if err != nil || len(sh) != 2 {
		t.Fatalf("search: %d hits %v", len(sh), err)
	}
	copies := 0
	for _, h := range sh {
		if h.NativeID == "c3" {
			t.Fatal("injected row in search")
		}
		copies += h.Copies
	}
	if copies != 2 {
		t.Fatalf("search copies %d", copies)
	}
	// An explicit kind filter is honoured as given.
	if hits, _ := s.Find(ctx, "wallaby", FindOptions{Filter: Filter{Kinds: []transcript.Kind{transcript.KindAssistant}}}); len(hits) != 1 {
		t.Fatalf("kind filter: %d", len(hits))
	}
}
