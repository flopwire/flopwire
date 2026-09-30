package localindex

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// L2: a shard that keeps failing must not hang Close. Its backpressure
// blocks the writer once the queue passes maxShardQueue; Close has to
// release that writer, and the unapplied entries stay in fts_queue.
func TestCloseWithFailingShardQueueFull(t *testing.T) {
	defer func(min time.Duration) { shardRetryMin = min }(shardRetryMin)
	shardRetryMin = time.Millisecond
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	shardFault = func(sh *ftsShard) error {
		if sh.table == "fts_tok" {
			return errors.New("injected permanent shard failure")
		}
		return nil
	}
	defer func() { shardFault = nil }()
	src := source(t, s, transcript.AgentClaude, "/h/full.jsonl")
	big := strings.Repeat("x", 512<<10)
	go func() {
		for i := range 12 { // 6MB of text, well past maxShardQueue
			s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("f", "f"+itoa(int64(i)), int64(i), transcript.KindUser, big+itoa(int64(i)))}})
		}
	}()
	time.Sleep(500 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind a failing shard's backpressure")
	}
	// Open replays the unapplied entries; with the shard still failing it
	// must fail, not hang behind the same backpressure.
	opened := make(chan error, 1)
	go func() {
		s, err := Open(path, Options{})
		if err == nil {
			s.Close()
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err == nil {
			t.Fatal("Open succeeded with a failing shard")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open hung replaying into a failing shard")
	}
}

// D7/D6: the rank cap takes the newest rows that pass the filters, with
// injected rows left out, on both orderings.
func TestRankCapSkipsHiddenRows(t *testing.T) { bothOrderings(t, testRankCapSkipsHiddenRows) }

func testRankCapSkipsHiddenRows(t *testing.T) {
	defer func(c int) { searchRankCap = c }(searchRankCap)
	searchRankCap = 3
	s := openTest(t, DetailColumn)
	src := source(t, s, transcript.AgentClaude, "/h/caphide.jsonl")
	var ms []*transcript.Message
	for i := range 8 {
		ms = append(ms, msg("h", "h"+itoa(int64(i)), int64(100-i*10), transcript.KindUser, "quokka "+strings.Repeat("z ", i)))
	}
	apply(t, s, Batch{SourceID: src.ID, Generation: 1, Messages: ms})
	if err := s.write(ctx, func(w *writeTx) error {
		_, err := w.exec(`UPDATE messages SET kind = ? WHERE native_id IN ('h0', 'h1')`, KindInjected)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Rank(ctx, "quokka", SearchOptions{Limit: 10, Filter: Filter{Agents: []transcript.Agent{transcript.AgentClaude}}})
	if err != nil || !r.RankCapped {
		t.Fatalf("rank cap: %+v %v", r, err)
	}
	var got []string
	for _, h := range r.Hits {
		got = append(got, h.NativeID)
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "h2,h3,h4" {
		t.Fatalf("ranked %v, want h2,h3,h4", got)
	}
}
