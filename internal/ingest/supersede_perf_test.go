package ingest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// A new version of a message supersedes the old row with one update that
// also links it to the new row (superseded_by, a deferred foreign key, so
// it may name the row inserted after it). Two updates would write a
// second row version and, since superseded_by is indexed, a second entry
// in every messages index.
func TestPerfSupersedeIsOneUpdate(t *testing.T) {
	const n = 50
	ctx := context.Background()
	pool, counter := perfguard.NewPool(t)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	src := source{id: uuid.NewString(), deviceID: uuid.NewString(), userID: uuid.NewString(), agent: "claude", generation: 0}
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'a@example.test','n','member','human',$2)`, []any{src.userID, now}},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, []any{src.deviceID, src.userID, now}},
		{`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/s.jsonl','1:1','jsonl_append','claude@1',$3)`, []any{src.id, src.deviceID, now}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(version string) {
		t.Helper()
		s := newSink(ctx, pool, src)
		for i := range n {
			m := &transcript.Message{SessionID: "sess", NativeID: fmt.Sprintf("m%d", i), Ordinal: int64(i), Kind: transcript.KindAssistant, Parser: "claude@1"}
			m.SetText(fmt.Sprintf("%s %d", version, i), transcript.CapConfig{})
			if err := s.Message(m); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.flush(); err != nil {
			t.Fatal(err)
		}
	}
	write("first")
	cost := perfguard.Measure(t, pool, counter, func() { write("second") })
	if got := cost.Tables["public.messages"]; got.TupUpd != n || got.TupIns != n {
		t.Errorf("superseding %d messages: %d updates and %d inserts, want %d each\n%s", n, got.TupUpd, got.TupIns, n, cost)
	}
	var linked, live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE o.superseded AND o.superseded_by=nw.id AND NOT nw.superseded),
			count(*) FILTER (WHERE NOT nw.superseded)
		FROM messages o JOIN messages nw ON nw.native_id=o.native_id AND nw.version=o.version+1`).Scan(&linked, &live); err != nil {
		t.Fatal(err)
	}
	if linked != n || live != n {
		t.Fatalf("linked %d, live %d, want %d", linked, live, n)
	}
}
