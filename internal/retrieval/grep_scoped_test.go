package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestGrepDottedVersionPlanningAndScopedSemantics(t *testing.T) {
	f := newFindFixture(t)
	f.add("assistant", "server 4040")
	f.add("assistant", "kickstart")
	f.add("assistant", "version 0.31.0")
	f.add("assistant", "wildcard 0x31y0")
	for _, tc := range []struct {
		pattern string
		filters format.Filters
		want    int
		scanned bool
	}{
		{`4040|kickstart|0\.31\.0`, format.Filters{}, 3, false},
		{`4040|kickstart|0.31.0`, format.Filters{Session: "sess-1"}, 4, true},
		{`0\.31\.0`, format.Filters{}, 1, false},
		{`0.31.0`, format.Filters{Session: "sess-1", Kinds: []string{"user"}}, 0, true},
	} {
		p, err := f.s.Grep(context.Background(), format.GrepQuery{Pattern: tc.pattern}, tc.filters)
		if err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		if len(p.Hits) != tc.want || !p.Exact || p.Truncated || (len(p.Notes) > 0) != tc.scanned {
			t.Fatalf("%s: %+v", tc.pattern, p)
		}
	}
	if _, err := f.s.Grep(context.Background(), format.GrepQuery{Pattern: `4040|kickstart|0.31.0`}, format.Filters{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("broad unindexed: %v", err)
	}
	if _, err := f.s.Grep(context.Background(), format.GrepQuery{Pattern: `.`}, format.Filters{Session: "unknown"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown scope: %v", err)
	}
}

func TestGrepScopedScanBodyBudget(t *testing.T) {
	f := newFindFixture(t)
	// The oldest message matches, but the newest exceeds the scan byte bound.
	// A misleading text_len must not allow that body through hydration.
	f.add("assistant", "x")
	f.add("assistant", strings.Repeat("z", grepSessionScanBytes+1))
	if _, err := f.s.Pool.Exec(context.Background(), `UPDATE messages SET text_len=1 WHERE ordinal=2`); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.Grep(context.Background(), format.GrepQuery{Pattern: "x"}, format.Filters{Session: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || p.Exact || len(p.Hits) != 0 || p.Reason == "" {
		t.Fatalf("oversized first body: %+v", p)
	}
	p, err = f.s.Grep(context.Background(), format.GrepQuery{Pattern: "x"}, format.Filters{Session: "sess-1", Sort: format.SortOldest})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || p.Exact || len(p.Hits) != 1 {
		t.Fatalf("oldest checked first: %+v", p)
	}
}

func TestGrepScopedScanRowBoundary(t *testing.T) {
	f := newFindFixture(t)
	// Exercise row limits independently of the default interactive timeout.
	ctx, cancel := context.WithTimeout(context.Background(), MaxBudget)
	defer cancel()
	// Nonmatching synthetic rows make the scan consume its full allowance.
	_, err := f.s.Pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser)
 SELECT gen_random_uuid(),$1,n,'assistant',$2::timestamptz + n*interval '1 microsecond','z',1,decode(repeat('00',32),'hex'),0,'test' FROM generate_series(1,$3::integer) n`, f.conv, time.Now(), grepSessionScanRows)
	if err != nil {
		t.Fatal(err)
	}
	query := format.GrepQuery{Pattern: "x"}
	filters := format.Filters{Session: "sess-1"}
	p, err := f.s.Grep(ctx, query, filters)
	if err != nil || p.Truncated || !p.Exact || len(p.Hits) != 0 {
		t.Fatalf("exact boundary: %+v %v", p, err)
	}
	// One more older row is visible to lookahead but cannot be verified.
	_, err = f.s.Pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser) VALUES(gen_random_uuid(),$1,$2,'assistant',$3,'x',1,decode(repeat('01',32),'hex'),0,'test')`, f.conv, grepSessionScanRows+1, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err = f.s.Grep(ctx, query, filters)
	if err != nil || !p.Truncated || p.Exact || len(p.Hits) != 0 {
		t.Fatalf("beyond boundary: %+v %v", p, err)
	}
}

func TestGrepScopedScanTimeoutIsIncomplete(t *testing.T) {
	f := newFindFixture(t)
	f.add("assistant", "x")
	ctx := context.Background()
	lock, err := f.s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(ctx)
	if _, err := lock.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p, err := f.s.Grep(deadlineOnly{ctx, time.Now().Add(150 * time.Millisecond)}, format.GrepQuery{Pattern: "x"}, format.Filters{Session: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second*2 || !p.Truncated || p.Exact || p.Reason == "" || len(p.Hits) != 0 {
		t.Fatalf("timeout: %+v after %s", p, time.Since(start))
	}
}
