package retrieval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func continuationCorpus(t *testing.T, n int) (*Store, format.GrepQuery, format.Filters) {
	t.Helper()
	s, _ := perfCorpus(t, 1, n)
	s.GrepContinuations = NewGrepContinuations(s.Pool)
	t.Cleanup(s.GrepContinuations.Close)
	var owner string
	if err := s.Pool.QueryRow(context.Background(), `SELECT id::text FROM users LIMIT 1`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return s, format.GrepQuery{Pattern: "step", Fixed: true, Continue: true, Authority: "test-device", Limit: 1}, format.Filters{Owner: owner}
}
func walkPage(t *testing.T, s *Store, q format.GrepQuery, f format.Filters) *format.Page {
	t.Helper()
	p, err := s.Grep(context.Background(), q, f)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestGrepContinuationSnapshotReplayAndDistinctAddresses(t *testing.T) {
	s, q, f := continuationCorpus(t, 3)
	ctx := context.Background()
	// Identical text must retain all three independent addresses.
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET text='step same'`); err != nil {
		t.Fatal(err)
	}
	// A transaction began before the walk but committed afterwards must not
	// enter the retained history (a sequence/timestamp cutoff cannot prove this).
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser) SELECT $1,conversation_id,source_id,99,kind,now(),'step late',9,content_sha,source_generation,parser FROM messages LIMIT 1`, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	first := walkPage(t, s, q, f)
	if len(first.Hits) != 1 || first.Scan.Status != "more" {
		t.Fatalf("first: %+v", first)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	next := q
	next.Cursor = first.Scan.NextCursor
	second := walkPage(t, s, next, f)
	if len(second.Hits) != 1 || second.Hits[0].Address == first.Hits[0].Address {
		t.Fatalf("second: %+v", second)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE messages SET text='[redacted]' WHERE id=$1`, second.Hits[0].MessageID); err != nil {
		t.Fatal(err)
	}
	replay := walkPage(t, s, next, f)
	if len(replay.Hits) != 0 || replay.Scan.NextCursor != second.Scan.NextCursor {
		t.Fatalf("replay replaced revoked hit: %+v", replay)
	}
	next.Cursor = second.Scan.NextCursor
	third := walkPage(t, s, next, f)
	if len(third.Hits) != 1 || third.Hits[0].Ordinal == 99 {
		t.Fatalf("third: %+v", third)
	}
	next.Cursor = third.Scan.NextCursor
	terminal := walkPage(t, s, next, f)
	if terminal.Scan.Status != "complete" || len(terminal.Hits) != 0 {
		t.Fatalf("terminal: %+v", terminal)
	}
	id := strings.Split(first.Scan.NextCursor, ".")[0]
	s.GrepContinuations.mu.Lock()
	w := s.GrepContinuations.searches[id]
	s.GrepContinuations.mu.Unlock()
	if w.tx != nil {
		t.Fatal("completed scan retained PostgreSQL transaction")
	}
}
func TestGrepContinuationAdmissionExpiryAuthorityAndCancellation(t *testing.T) {
	s, q, f := continuationCorpus(t, 3)
	first := walkPage(t, s, q, f)
	id := strings.Split(first.Scan.NextCursor, ".")[0]
	s.GrepContinuations.mu.Lock()
	w := s.GrepContinuations.searches[id]
	s.GrepContinuations.mu.Unlock()
	w.mu.Lock()
	gotBusy, busyErr := s.Grep(context.Background(), q, f)
	w.mu.Unlock()
	if busyErr != nil || gotBusy.Scan.Status != "overloaded" {
		t.Fatalf("busy owner admission: %+v %v", gotBusy, busyErr)
	}
	q.Cursor = first.Scan.NextCursor
	wrong := q
	wrong.Authority = "other-device"
	if _, err := s.Grep(context.Background(), wrong, f); err == nil {
		t.Fatal("cursor accepted changed authority")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.Grep(ctx, q, f)
	if err != nil || got.Scan.Status != "restart_required" {
		t.Fatalf("cancellation: %+v %v", got, err)
	}
	s.GrepContinuations.mu.Lock()
	w = s.GrepContinuations.searches[id]
	s.GrepContinuations.mu.Unlock()
	w.mu.Lock()
	w.expires = time.Now().Add(-time.Second)
	w.mu.Unlock()
	s.GrepContinuations.expire(w)
	if got := walkPage(t, s, q, f); got.Scan.Status != "restart_required" {
		t.Fatalf("expiry: %+v", got)
	}
	if w.tx != nil {
		t.Fatal("expired scan retained transaction")
	}
	if got := walkPage(t, s, format.GrepQuery{Pattern: "step", Fixed: true, Continue: true, Authority: "test-device", Limit: 1}, f); got.Scan.Status != "more" {
		t.Fatalf("admission not released: %+v", got)
	}
}
func TestGrepContinuationOversizedBodyFailsExplicitly(t *testing.T) {
	s, q, f := continuationCorpus(t, 1)
	if _, err := s.Pool.Exec(context.Background(), `UPDATE messages SET text=$1`, "step "+strings.Repeat("x", grepContinuationBody)); err != nil {
		t.Fatal(err)
	}
	got := walkPage(t, s, q, f)
	if got.Scan.Status != "restart_required" || len(got.Hits) != 0 || !strings.Contains(got.Reason, "body limit") {
		t.Fatalf("oversized coverage: %+v", got)
	}
}

func TestGrepContinuationRollingReplayAndReplacement(t *testing.T) {
	s, q, f := continuationCorpus(t, 12)
	first := walkPage(t, s, q, f)
	cursor := first.Scan.NextCursor
	oldCursor := cursor
	for i := 0; i < 6; i++ {
		q.Cursor = cursor
		p := walkPage(t, s, q, f)
		if len(p.Hits) != 1 {
			t.Fatalf("page %d: %+v", i, p)
		}
		cursor = p.Scan.NextCursor
	}
	q.Cursor = oldCursor
	if got := walkPage(t, s, q, f); got.Scan.Status != "restart_required" {
		t.Fatalf("evicted replay: %+v", got)
	}
	q.Cursor = cursor
	if got := walkPage(t, s, q, f); len(got.Hits) != 1 {
		t.Fatalf("evicted replay killed forward scan: %+v", got)
	}
	q.Cursor = ""
	replacement := walkPage(t, s, q, f)
	if len(replacement.Hits) != 1 {
		t.Fatalf("replacement: %+v", replacement)
	}
	q.Cursor = cursor
	if got := walkPage(t, s, q, f); got.Scan.Status != "restart_required" {
		t.Fatalf("replaced cursor: %+v", got)
	}
}
func TestGrepContinuationCurrentDeletionVisibilityAndFilters(t *testing.T) {
	for _, change := range []string{"delete", "hide", "kind", "branch", "ordinal"} {
		t.Run(change, func(t *testing.T) {
			s, q, f := continuationCorpus(t, 3)
			q.Limit = 2
			first := walkPage(t, s, q, f)
			q.Cursor = first.Scan.NextCursor
			page := walkPage(t, s, q, f)
			if len(page.Hits) != 1 {
				t.Fatalf("second page: %+v", page)
			}
			id := page.Hits[0].MessageID
			var sql string
			switch change {
			case "delete":
				sql = `DELETE FROM messages WHERE id=$1`
			case "hide":
				sql = `UPDATE conversations SET hidden_at=now() WHERE id=(SELECT conversation_id FROM messages WHERE id=$1)`
			case "kind":
				sql = `UPDATE messages SET kind='injected' WHERE id=$1`
			case "branch":
				sql = `UPDATE messages SET on_active_path=false WHERE id=$1`
			case "ordinal":
				sql = `UPDATE messages SET ordinal=99 WHERE id=$1`
			}
			if _, err := s.Pool.Exec(context.Background(), sql, id); err != nil {
				t.Fatal(err)
			}
			replay := walkPage(t, s, q, f)
			if len(replay.Hits) != 0 || replay.Scan.NextCursor != page.Scan.NextCursor {
				t.Fatalf("revoked replay: %+v", replay)
			}
		})
	}
}
func TestGrepContinuationThirtyConcurrentRequestsBoundResources(t *testing.T) {
	s, q, _ := continuationCorpus(t, 30)
	config := s.Pool.Config()
	config.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s.GrepContinuations.Close()
	s.Pool = pool
	s.GrepContinuations = NewGrepContinuations(pool)
	t.Cleanup(s.GrepContinuations.Close)
	type result struct {
		page *format.Page
		err  error
	}
	results := make(chan result, 30)
	start := make(chan struct{})
	for i := 0; i < 30; i++ {
		go func() {
			<-start
			p, err := s.Grep(context.Background(), q, format.Filters{Owner: uuid.NewString()})
			results <- result{p, err}
		}()
	}
	close(start)
	accepted, overloaded := 0, 0
	for i := 0; i < 30; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			switch r.page.Scan.Status {
			case "more":
				accepted++
			case "overloaded":
				overloaded++
			default:
				t.Fatalf("unexpected stress status: %+v", r.page)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent requests deadlocked")
		}
	}
	if accepted != 4 || overloaded != 26 {
		t.Fatalf("admission accepted=%d overloaded=%d", accepted, overloaded)
	}
	s.GrepContinuations.mu.Lock()
	retained := len(s.GrepContinuations.searches)
	s.GrepContinuations.mu.Unlock()
	if retained != 4 || pool.Stat().AcquiredConns() > 4 {
		t.Fatalf("retained=%d acquired=%d", retained, pool.Stat().AcquiredConns())
	}
	s.GrepContinuations.Close()
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatalf("Close leaked %d pool connections", pool.Stat().AcquiredConns())
	}
}
