package localindex

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func storedDigest(t *testing.T, s *Store, session string) (*digest.Digest, []string) {
	t.Helper()
	var d, br *string
	if err := s.DB().QueryRow(`SELECT digest, branches FROM conversations WHERE session_id = ?`, session).Scan(&d, &br); err != nil {
		t.Fatal(err)
	}
	var branches []string
	if br != nil {
		_ = json.Unmarshal([]byte(*br), &branches)
	}
	if d == nil {
		return nil, branches
	}
	return digest.Parse([]byte(*d)), branches
}

// Every batch refreshes the digests of the conversations it touched:
// folded facts accumulate, counts are recounted (no double counting on a
// re-parse), tokens count each API message once, and a subagent bumps
// its parent's count.
func TestDigestRefreshedOnEveryAppend(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/s1.jsonl")
	conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s1", Cwd: "/repo/a", Branches: []string{"main"}, Title: "Fix uploads"}
	user := msg("s1", "u1", 1, transcript.KindUser, "fix the flaky upload test please")
	call := msg("s1", "c1", 2, transcript.KindToolCall, `{"command":"git commit -am fix"}`)
	call.ToolName, call.ToolCallID = "Bash", "t1"
	res := msg("s1", "r1", 3, transcript.KindToolResult, "[main 1a2b3c4] fix")
	res.ToolName, res.ToolCallID = "Bash", "t1"
	// Two lines of one API message repeat its usage; the larger counts.
	a1 := msg("s1", "a1", 4, transcript.KindThinking, "thinking")
	a1.Enrichment = map[string]any{"message_id": "m1", "usage": map[string]int64{"input_tokens": 10, "output_tokens": 5}}
	a2 := msg("s1", "a2", 5, transcript.KindAssistant, "Committed the fix.")
	a2.Enrichment = map[string]any{"message_id": "m1", "usage": map[string]int64{"input_tokens": 10, "output_tokens": 50}}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{conv}, Messages: []*transcript.Message{user, call}})
	d, branches := storedDigest(t, s, "s1")
	if d == nil || d.Intent != "fix the flaky upload test please" || d.Commands != 1 || d.Messages["user"] != 1 || fmt.Sprint(branches) != "[main]" {
		t.Fatalf("after batch 1: %+v branches %v", d, branches)
	}
	// The call's result arrives in the next batch and names the commit.
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{res, a1, a2}})
	d, _ = storedDigest(t, s, "s1")
	if fmt.Sprint(d.Commits) != "[1a2b3c4]" || d.Last != "Committed the fix." || d.Tokens == nil || d.Tokens.Output != 50 || d.Tokens.Input != 10 ||
		d.Repos[0] != "/repo/a" || d.DurationS != 4 {
		t.Fatalf("after batch 2: %+v tokens %+v", d, d.Tokens)
	}
	// A re-parse of every row counts nothing twice.
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{conv}, Messages: []*transcript.Message{user, call, res, a1, a2}})
	if d2, _ := storedDigest(t, s, "s1"); d2.Messages["tool_call"] != 1 || d2.Tokens.Output != 50 || len(d2.Commits) != 1 {
		t.Fatalf("after a re-parse: %+v", d2)
	}
	// A subagent updates its parent's count.
	sub := source(t, s, transcript.AgentClaude, "/p/s1/subagents/agent-x.jsonl")
	apply(t, s, Batch{SourceID: sub.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "agent-x", ParentSessionID: "s1", Depth: 1, Branches: []string{"main", "feat/y"}}},
		Messages:      []*transcript.Message{msg("agent-x", "x1", 1, transcript.KindUser, "scan the call sites for retries")}})
	if d, _ = storedDigest(t, s, "s1"); d.Subagents != 1 {
		t.Fatalf("parent's subagents: %d", d.Subagents)
	}
	if _, br := storedDigest(t, s, "agent-x"); fmt.Sprint(br) != "[main feat/y]" {
		t.Fatalf("subagent branches %v", br)
	}
	// The branch filter and the listing see the branches.
	rows, err := s.ListConversations(ctx, ListOptions{Filter: Filter{Branches: []string{"feat/%"}}})
	if err != nil || len(rows) != 1 || rows[0].SessionID != "agent-x" || len(rows[0].Digest) == 0 {
		t.Fatalf("branch filter: %v %+v", err, rows)
	}
	if n, err := s.CountConversations(ctx, ListOptions{Filter: Filter{Branches: []string{"main"}}}); err != nil || n != 2 {
		t.Fatalf("branch count: %d %v", n, err)
	}
	// Oldest first.
	rows, _ = s.ListConversations(ctx, ListOptions{Oldest: true})
	if len(rows) != 2 || rows[0].SessionID != "agent-x" {
		t.Fatalf("oldest first: %+v", rows)
	}
	// IdleBefore leaves out sessions active since.
	if n, _ := s.CountConversations(ctx, ListOptions{Filter: Filter{IdleBefore: t0.Add(3 * time.Second)}}); n != 1 {
		t.Fatalf("idle before: %d", n)
	}
}

func TestScanOldestAndSearchByTime(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/s1.jsonl")
	var msgs []*transcript.Message
	for i := range 5 {
		msgs = append(msgs, msg("s1", fmt.Sprint("m", i), int64(i+1), transcript.KindAssistant, fmt.Sprintf("retry backoff number %d", i)))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s1"}}, Messages: msgs})
	order := func(oldest bool) []string {
		var got []string
		_, err := s.Scan(ctx, s.SubstringQuery("backoff"), ScanOptions{Oldest: oldest}, func(r *Row) bool {
			got = append(got, r.NativeID)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := order(true); !slices.Equal(got, []string{"m0", "m1", "m2", "m3", "m4"}) {
		t.Fatalf("oldest scan %v", got)
	}
	if got := order(false); got[0] != "m4" {
		t.Fatalf("newest scan %v", got)
	}
	for _, tc := range []struct {
		by   string
		want string
	}{{"oldest", "m0 m1"}, {"newest", "m4 m3"}} {
		res, err := s.Rank(ctx, "retry backoff", SearchOptions{ByTime: tc.by, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range res.Hits {
			got = append(got, h.NativeID)
		}
		if fmt.Sprint(got) != "["+tc.want+"]" {
			t.Fatalf("search by %s: %v", tc.by, got)
		}
	}
}

func TestOutline(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/s1.jsonl")
	u := msg("s1", "u", 1, transcript.KindUser, "do it")
	c := msg("s1", "c", 2, transcript.KindToolCall, `{"command":"false"}`)
	c.ToolName, c.ToolCallID = "Bash", "t1"
	r := msg("s1", "r", 3, transcript.KindToolResult, "exit 1")
	r.ToolCallID, r.IsError = "t1", true
	sp := msg("s1", "sp", 4, transcript.KindToolCall, `{"description":"scan"}`)
	sp.ToolName, sp.ToolCallID = "Task", "t2"
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s1"}},
		Messages: []*transcript.Message{u, c, r, sp}})
	sub := source(t, s, transcript.AgentClaude, "/p/s1/subagents/agent-k.jsonl")
	apply(t, s, Batch{SourceID: sub.ID, Generation: 1,
		Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "agent-k", ParentSessionID: "s1", SpawnedByToolCallID: "t2", Depth: 1}},
		Messages:      []*transcript.Message{msg("agent-k", "k", 1, transcript.KindUser, "scan")}})
	var conv int64
	if err := s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id = 's1'`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	o, err := s.Outline(ctx, conv, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if o.Total != 3 || len(o.Rows) != 2 || o.Rows[0].NativeID != "c" || !o.Failed["t1"] || len(o.Spawned[o.Rows[1].ID]) != 1 || o.Spawned[o.Rows[1].ID][0] != "agent-k" {
		t.Fatalf("outline %+v failed %v spawned %v", o, o.Failed, o.Spawned)
	}
}

// Appends add to the stored counts rather than recounting; the result is
// what a full recount gives, across batches that split an API message's
// lines and a call's failed rows.
func TestDigestAppendMatchesRecount(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/s1.jsonl")
	conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s1"}
	var batches [][]*transcript.Message
	ord := int64(0)
	next := func(kind transcript.Kind, text string) *transcript.Message {
		ord++
		return msg("s1", fmt.Sprint("n", ord), ord, kind, text)
	}
	for b := range 6 {
		var ms []*transcript.Message
		ms = append(ms, next(transcript.KindUser, "please look at the retry path again"))
		c := next(transcript.KindToolCall, `{"command":"go test ./..."}`)
		c.ToolName, c.ToolCallID = "Bash", fmt.Sprint("call", b/2) // two batches per call
		r := next(transcript.KindToolResult, "FAIL")
		r.ToolCallID, r.IsError = c.ToolCallID, true
		ms = append(ms, c, r)
		// An API message whose lines straddle the batch boundary.
		for j := range 2 {
			a := next(transcript.KindAssistant, "working on it")
			a.Enrichment = map[string]any{"message_id": fmt.Sprint("m", (2*b+j+1)/2), "usage": map[string]int64{"input_tokens": 7, "output_tokens": int64(10 * (j + 1)), "cache_read_input_tokens": 3}}
			ms = append(ms, a)
		}
		orphan := next(transcript.KindToolResult, "error with no call")
		orphan.IsError = true
		ms = append(ms, orphan)
		batches = append(batches, ms)
	}
	for i, ms := range batches {
		bt := Batch{SourceID: src.ID, Generation: 1, Messages: ms}
		if i == 0 {
			bt.Conversations = []*transcript.Conversation{conv}
		}
		apply(t, s, bt)
	}
	got, _ := storedDigest(t, s, "s1")
	var want digest.Counts
	var convID int64
	if err := s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id = 's1'`).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	if err := s.write(ctx, func(w *writeTx) error {
		var err error
		want, err = w.digestCounts(convID, "claude", "s1", s.opts.DeviceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got.Messages) != fmt.Sprint(want.Messages) || fmt.Sprint(got.Tools) != fmt.Sprint(want.Tools) || got.Failed != want.Failed ||
		got.Tokens == nil || *got.Tokens != want.Tokens {
		t.Fatalf("appended counts %v %v failed %d tokens %+v; recount %v %v failed %d tokens %+v",
			got.Messages, got.Tools, got.Failed, got.Tokens, want.Messages, want.Tools, want.Failed, want.Tokens)
	}
	if want.Failed != 3+6 {
		t.Fatalf("recount failed %d, want 9", want.Failed)
	}
}

// FLOPWIRE_DIGEST_COST=1 measures a digest refresh on a long session (20k
// messages appended 100 at a time): a full recount, as every append did
// before, against adding the batch to the stored counts.
func TestDigestAppendCost(t *testing.T) {
	if os.Getenv("FLOPWIRE_DIGEST_COST") == "" {
		t.Skip("set FLOPWIRE_DIGEST_COST=1")
	}
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/long.jsonl")
	conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "long"}
	var last []*transcript.Message
	start := time.Now()
	var firstBatches, lastBatches time.Duration
	for b := range 200 {
		var ms []*transcript.Message
		for j := range 100 {
			n := int64(b*100 + j + 1)
			m := msg("long", fmt.Sprint("n", n), n, transcript.KindAssistant, fmt.Sprintf("reply %d with some words", n))
			if j%2 == 0 {
				m.Kind, m.ToolName, m.ToolCallID, m.Text = transcript.KindToolCall, "Bash", fmt.Sprint("c", n), `{"command":"ls"}`
			}
			m.Enrichment = map[string]any{"message_id": fmt.Sprint("m", n/2), "usage": map[string]int64{"input_tokens": 5, "output_tokens": n % 7}}
			ms = append(ms, m)
		}
		bt := Batch{SourceID: src.ID, Generation: 1, Messages: ms}
		if b == 0 {
			bt.Conversations = []*transcript.Conversation{conv}
		}
		t0 := time.Now()
		apply(t, s, bt)
		switch {
		case b < 10:
			firstBatches += time.Since(t0)
		case b >= 190:
			lastBatches += time.Since(t0)
		}
		last = ms
	}
	var convID int64
	if err := s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id = 'long'`).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	refresh := func(full bool) time.Duration {
		t0 := time.Now()
		for range 10 {
			if err := s.write(ctx, func(w *writeTx) error { return w.refreshDigest(convID, last, last, full) }); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(t0) / 10
	}
	t.Logf("20k messages in %v; batches 1-10 %v, 191-200 %v; refresh at 20k rows: recount %v, append %v",
		time.Since(start).Round(time.Millisecond), firstBatches.Round(time.Millisecond), lastBatches.Round(time.Millisecond), refresh(true), refresh(false))
}

// A re-parse in batches refreshes rows instead of recounting the
// conversation per batch when nothing counted changed: the stored counts
// must still equal a recount after every batch, including batches that
// change a counted attribute (a result no longer failed, a renamed tool,
// a row leaving the active path, new usage) or mix in new rows.
func TestDigestReparseMatchesRecount(t *testing.T) {
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/p/s1.jsonl")
	conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "s1"}
	build := func(variant int) [][]*transcript.Message {
		var batches [][]*transcript.Message
		ord := int64(0)
		next := func(kind transcript.Kind, text string) *transcript.Message {
			ord++
			return msg("s1", fmt.Sprint("n", ord), ord, kind, text)
		}
		for b := range 4 {
			var ms []*transcript.Message
			ms = append(ms, next(transcript.KindUser, "please look at the retry path again"))
			c := next(transcript.KindToolCall, `{"command":"go test ./..."}`)
			c.ToolName, c.ToolCallID = "Bash", fmt.Sprint("call", b)
			r := next(transcript.KindToolResult, "FAIL")
			r.ToolCallID, r.IsError = c.ToolCallID, true
			a := next(transcript.KindAssistant, "working on it")
			a.Enrichment = map[string]any{"message_id": fmt.Sprint("m", b), "usage": map[string]int64{"input_tokens": 7, "output_tokens": 10}}
			switch {
			case variant == 1 && b == 1:
				r.IsError = false
			case variant == 1 && b == 2:
				c.ToolName = "Shell"
			case variant == 2 && b == 0:
				off := false
				a.OnActivePath = &off
			case variant == 2 && b == 3:
				a.Enrichment = map[string]any{"message_id": fmt.Sprint("m", b), "usage": map[string]int64{"input_tokens": 7, "output_tokens": 99}}
			case variant == 3 && b == 1:
				// A new row in a re-parsed batch.
				ms = append(ms, msg("s1", "extra", 50, transcript.KindUser, "and one more thing"))
			}
			ms = append(ms, c, r, a)
			batches = append(batches, ms)
		}
		return batches
	}
	var convID int64
	check := func(what string) {
		t.Helper()
		if convID == 0 {
			if err := s.DB().QueryRow(`SELECT id FROM conversations WHERE session_id = 's1'`).Scan(&convID); err != nil {
				t.Fatal(err)
			}
		}
		got, _ := storedDigest(t, s, "s1")
		var want digest.Counts
		if err := s.write(ctx, func(w *writeTx) error {
			var err error
			want, err = w.digestCounts(convID, "claude", "s1", s.opts.DeviceID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var tok digest.Tokens
		if got.Tokens != nil {
			tok = *got.Tokens
		}
		if fmt.Sprint(got.Messages) != fmt.Sprint(want.Messages) || fmt.Sprint(got.Tools) != fmt.Sprint(want.Tools) ||
			got.Failed != want.Failed || tok != want.Tokens {
			t.Fatalf("%s: stored %v %v failed %d tokens %+v; recount %v %v failed %d tokens %+v",
				what, got.Messages, got.Tools, got.Failed, tok, want.Messages, want.Tools, want.Failed, want.Tokens)
		}
	}
	for variant := range 4 {
		for i, ms := range build(variant) {
			bt := Batch{SourceID: src.ID, Generation: 1, Messages: ms}
			if i == 0 {
				bt.Conversations = []*transcript.Conversation{conv}
			}
			apply(t, s, bt)
			check(fmt.Sprintf("variant %d batch %d", variant, i))
		}
	}
}
