package retrieval

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/jackc/pgx/v5/pgxpool"
)

// stallPlan picks one write on a stallStore's connections and holds it
// until the context deadline interrupts it, as a write in flight when the
// budget runs out is interrupted: pgx sets a deadline on the net.Conn when
// the context ends, and the write fails with an i/o timeout.
type stallPlan struct {
	afterRead []byte // stall the first write after the connection read these bytes
	onWrite   []byte // or the first write holding these bytes
	fail      bool   // fail the write at once with an i/o timeout instead of waiting

	mu    sync.Mutex
	fired bool
	cut   bool   // the held write failed on the deadline set while it was held
	hold  func() // run once the write is held (a test cancels its context here)
}

// wasCut fails t unless the chosen write was held and then cut by the
// deadline set while it was held, so a test of the cut write does not pass
// on some other path when pgx changes how it writes or interrupts.
func (p *stallPlan) wasCut(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.fired || !p.cut {
		t.Fatalf("the chosen write was not cut by the deadline: held=%v cut=%v", p.fired, p.cut)
	}
}

func (p *stallPlan) take() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fired {
		return false
	}
	p.fired = true
	return true
}

type stallConn struct {
	net.Conn
	plan *stallPlan

	mu   sync.Mutex
	seen bool
	set  chan time.Time // while a write is stalled: the next deadline set
}

func (c *stallConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.plan.afterRead != nil && bytes.Contains(b[:n], c.plan.afterRead) {
		c.mu.Lock()
		c.seen = true
		c.mu.Unlock()
	}
	return n, err
}

func (c *stallConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	match := c.plan.afterRead != nil && c.seen || c.plan.onWrite != nil && bytes.Contains(b, c.plan.onWrite)
	c.mu.Unlock()
	if match && c.plan.take() {
		if c.plan.fail {
			return 0, &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}
		}
		ch := make(chan time.Time, 1)
		c.mu.Lock()
		c.set = ch
		c.mu.Unlock()
		if c.plan.hold != nil {
			c.plan.hold()
		}
		select {
		case at := <-ch:
			time.Sleep(time.Until(at))
			// The runtime marks the deadline passed by a timer that
			// can fire after at; an empty write fails once it has.
			for range 1000 {
				if _, err := c.Conn.Write(nil); err != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			n, err := c.Conn.Write(b)
			var ne net.Error
			c.plan.mu.Lock()
			c.plan.cut = errors.As(err, &ne) && ne.Timeout()
			c.plan.mu.Unlock()
			return n, err
		case <-time.After(10 * time.Second):
		}
	}
	return c.Conn.Write(b)
}

func (c *stallConn) note(t time.Time) {
	if t.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil {
		c.set <- t
		c.set = nil
	}
}

func (c *stallConn) SetDeadline(t time.Time) error {
	c.note(t)
	return c.Conn.SetDeadline(t)
}

func (c *stallConn) SetWriteDeadline(t time.Time) error {
	c.note(t)
	return c.Conn.SetWriteDeadline(t)
}

// stallStore is f's store on a pool of its own whose connections follow p.
func (f *findFixture) stallStore(p *stallPlan) *Store {
	f.t.Helper()
	cfg := f.s.Pool.Config()
	var d net.Dialer
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &stallConn{Conn: c, plan: p}, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(pool.Close)
	return &Store{Pool: pool}
}

func (f *findFixture) uploads(n int) {
	f.t.Helper()
	if _, err := f.s.Pool.Exec(context.Background(), `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
		SELECT gen_random_uuid(),$1,g,'tool_result',now()-g*interval '1 second','upload number '||g,20,sha256(('upload number '||g)::bytea),0,'test'
		FROM generate_series(1,$2::int) g`, f.conv, n); err != nil {
		f.t.Fatal(err)
	}
}

// A grep whose budget runs out after it has verified hits returns them as
// a truncated page that says it timed out, even when the deadline
// interrupts a write to Postgres (an i/o timeout, not a context error):
// the lookups that follow the scan (session infos, addresses) must not
// fail on the spent deadline either (#121).
func TestGrepBudgetEndsAfterHits(t *testing.T) {
	f := newFindFixture(t)
	f.uploads(2000)
	// The first fetch returns 256 candidates; the write after it is held
	// until the deadline cuts it.
	plan := &stallPlan{afterRead: []byte("upload number")}
	s := f.stallStore(plan)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := s.Grep(ctx, format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || !strings.HasPrefix(p.Reason, "timed out after") || len(p.Hits) == 0 || p.Exact {
		t.Fatalf("want a truncated page of the hits so far, got truncated=%v exact=%v reason=%q hits=%d", p.Truncated, p.Exact, p.Reason, len(p.Hits))
	}
	plan.wasCut(t)
	// The hits so far are a full run's first hits, in order, each whole.
	full, err := f.s.Grep(context.Background(), format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hits) > len(full.Hits) {
		t.Fatalf("%d partial hits, %d in a full run", len(p.Hits), len(full.Hits))
	}
	for i, h := range p.Hits {
		w := full.Hits[i]
		if h.Address == "" || h.Address != w.Address || h.Ordinal != w.Ordinal || snip(h) != snip(w) {
			t.Fatalf("partial hit %d is %q %q, a full run's is %q %q", i, h.Address, snip(h), w.Address, snip(w))
		}
	}
	// The connection whose write was cut is not reused: the pool's next
	// queries see whole protocol messages.
	for i := range 5 {
		var n int
		if err := s.Pool.QueryRow(context.Background(), `SELECT $1::int`, i).Scan(&n); err != nil || n != i {
			t.Fatalf("query %d after the cut write: %d, %v", i, n, err)
		}
	}
	again, err := s.Grep(context.Background(), format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
	if err != nil || again.Truncated || len(again.Hits) != len(full.Hits) {
		t.Fatalf("grep after the cut write: %v", err)
	}
}

// A grep its caller cancels while a write is in flight returns an error,
// not the hits so far as a page truncated by the budget.
func TestGrepCancelMidWrite(t *testing.T) {
	f := newFindFixture(t)
	f.uploads(2000)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plan := &stallPlan{afterRead: []byte("upload number"), hold: cancel}
	s := f.stallStore(plan)
	p, err := s.Grep(ctx, format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
	plan.wasCut(t)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an error that is not a spent budget, got %v (page %+v)", err, p)
	}
	t.Logf("cancelled grep: %v (is Canceled: %v)", err, errors.Is(err, context.Canceled))
}

// An i/o timeout the budget did not cause is a transport error, not a
// spent budget: grep surfaces it.
func TestGrepTransportTimeoutSurfaces(t *testing.T) {
	f := newFindFixture(t)
	f.uploads(2000)
	s := f.stallStore(&stallPlan{afterRead: []byte("upload number"), fail: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.Grep(ctx, format.GrepQuery{Pattern: "upload number", Fixed: true, Limit: 500}, format.Filters{})
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the transport's i/o timeout, got %v", err)
	}
}

// A search whose deadline interrupts its write says it timed out, as a
// search whose statement timeout fires does.
func TestSearchBudgetDeadlineMidWrite(t *testing.T) {
	f := newFindFixture(t)
	f.add("user", "alpha beta")
	plan := &stallPlan{onWrite: []byte("ts_rank_cd")}
	s := f.stallStore(plan)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, err := s.Search(ctx, format.SearchQuery{Query: "alpha"}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || !strings.HasPrefix(p.Reason, "timed out after") {
		t.Fatalf("want a timed-out page, got truncated=%v reason=%q", p.Truncated, p.Reason)
	}
	plan.wasCut(t)
}

// A read whose deadline interrupts its write is a timed-out error, as a
// read whose statement timeout fires is, not a transport error.
func TestReadBudgetDeadlineMidWrite(t *testing.T) {
	f := newFindFixture(t)
	f.add("user", "hello")
	plan := &stallPlan{onWrite: []byte("sess-1")}
	s := f.stallStore(plan)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := s.Read(ctx, "", format.ReadQuery{Address: "sess-1/1"}, format.Filters{})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("want a timed-out error, got %v", err)
	}
	plan.wasCut(t)
}
