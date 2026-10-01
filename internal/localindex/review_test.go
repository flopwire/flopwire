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
//
// The test closes only once the writer is blocked on the failing shard's
// backpressure, and keeps the batches small: with 512KB messages a healthy
// trigram shard needed seconds per batch under -race on a loaded machine,
// and Close (which lets the writer's commit finish) outran a fixed timeout
// before the queue was ever full.
func TestCloseWithFailingShardQueueFull(t *testing.T) {
	defer func(min time.Duration, max int) { shardRetryMin, maxShardQueue = min, max }(shardRetryMin, maxShardQueue)
	shardRetryMin = time.Millisecond
	maxShardQueue = 4 << 10
	// Hooks are set before Open and cleared after the stores close, so the
	// shard goroutines never race with the writes.
	shardFault = func(sh *ftsShard) error {
		if sh.table == "fts_tok" {
			return errors.New("injected permanent shard failure")
		}
		return nil
	}
	blocked := make(chan struct{}, 1)
	shardBlocked = func(sh *ftsShard) {
		if sh.table == "fts_tok" {
			select {
			case blocked <- struct{}{}:
			default:
			}
		}
	}
	defer func() { shardFault, shardBlocked = nil, nil }()
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	src := source(t, s, transcript.AgentClaude, "/h/full.jsonl")
	big := strings.Repeat("x", 1<<10)
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := range 12 { // 12KB of text, well past maxShardQueue
			s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Messages: []*transcript.Message{msg("f", "f"+itoa(int64(i)), int64(i), transcript.KindUser, big+itoa(int64(i)))}})
		}
	}()
	select {
	case <-blocked:
	case <-produced:
		t.Fatal("the writer never blocked on the failing shard's backpressure")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung behind a failing shard's backpressure")
	}
	<-produced // the remaining writes fail with ErrClosed
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
