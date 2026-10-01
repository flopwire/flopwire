package localindex

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The compaction after a redaction must not hold the shard: a live line
// written while it runs is findable (Sync returns) within a step, not after
// the whole shard is rewritten (tens of seconds on a real corpus).
func TestLocalRedactionScrubDoesNotStallWriter(t *testing.T) {
	if testing.Short() || perfguard.Race {
		t.Skip("times a compaction: not under -short or the race detector")
	}
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{DeferCommit: true, TriParts: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src := source(t, s, transcript.AgentClaude, "/h/s1.jsonl")
	conv := &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "sess-1"}
	r := rand.New(rand.NewPCG(1, 2))
	word := func() string {
		b := make([]byte, 4+r.IntN(8))
		for i := range b {
			b[i] = byte('a' + r.IntN(26))
		}
		return string(b)
	}
	sinkMsgs(t, s, src.ID, 1, conv, msg("sess-1", "u0", 0, transcript.KindUser, "hello\nmy pin is 4417 zebracorn\nbye"))
	off := int64(1)
	// About 7MB of text in many commits: many segments to merge.
	for range 12 {
		var ms []*transcript.Message
		for range 150 {
			var sb strings.Builder
			for sb.Len() < 4000 {
				sb.WriteString(word())
				sb.WriteByte(' ')
			}
			ms = append(ms, msg("sess-1", fmt.Sprintf("m%d", off), off, transcript.KindAssistant, sb.String()))
			off++
		}
		sinkMsgs(t, s, src.ID, 1, conv, ms...)
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var done atomic.Int64 // scrubs finished
	var took atomic.Int64
	testHookScrubbed = func(_ *ftsShard, d time.Duration) {
		took.Store(max(took.Load(), int64(d)))
		done.Add(1)
	}
	defer func() { testHookScrubbed = nil }()
	if _, err := s.RedactMessage(ctx, LocalRedaction{Session: "sess-1", Ordinal: transcript.OrdinalAt(0, 0), From: 2, To: 2}); err != nil {
		t.Fatal(err)
	}
	// Live lines while the shards compact.
	var worst time.Duration
	n := 0
	for done.Load() < int64(len(s.shards)) {
		start := time.Now()
		sinkMsgs(t, s, src.ID, 1, conv, msg("sess-1", fmt.Sprintf("live%d", n), off, transcript.KindAssistant, fmt.Sprintf("live line %d quokka%d", n, n)))
		off++
		if err := s.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		worst = max(worst, time.Since(start))
		if got := findIDs(t, s, fmt.Sprintf("quokka%d", n), FindOptions{}); len(got) != 1 {
			t.Fatalf("live line %d not findable after Sync: %v", n, got)
		}
		n++
		time.Sleep(20 * time.Millisecond)
	}
	total := time.Duration(took.Load())
	t.Logf("compaction %v, %d live lines, worst Sync %v", total, n, worst)
	if total < 300*time.Millisecond {
		t.Fatalf("compaction took %v: too short to tell", total)
	}
	if worst > 500*time.Millisecond || worst > total/3 {
		t.Fatalf("a live line waited %v for its Sync during a %v compaction", worst, total)
	}
	eq(t, "find", findIDs(t, s, "zebracorn", FindOptions{}), nil)
}
