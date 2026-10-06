package ingest

import (
	"fmt"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5"
)

// Exercise the real sink's initial recount, append, replacement fold and
// completion recount, with absent and stale harness activity timestamps.
func TestDigestMessageActivity(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			s := batchTestSink(t)
			start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			harness := time.Time{}
			if stale {
				harness = start.Add(time.Second)
			}
			conversation := func() {
				t.Helper()
				if err := s.Conversation(&transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "batch-test", StartedAt: start, LastActivityAt: harness}); err != nil {
					t.Fatal(err)
				}
			}
			write := func(m *transcript.Message) {
				t.Helper()
				if err := s.Message(m); err != nil {
					t.Fatal(err)
				}
				if err := s.flush(); err != nil {
					t.Fatal(err)
				}
			}
			// Submicrosecond message timestamps must match stored activity;
			// the digest duration must use that same stored timestamp.
			first := batchMessage("first", 1, "first prompt")
			first.TS = start.Add(2*time.Second - time.Nanosecond)
			conversation()
			write(first) // nil digest forces recount
			assertDigestActivity(t, s, first.TS, 1, false)

			newest := batchMessage("newest", 2, "another prompt")
			newest.TS = start.Add(4*time.Second - time.Nanosecond)
			active := false
			newest.OnActivePath = &active // excluded from counts, still activity
			older := batchMessage("older", 3, "older prompt")
			older.TS = start.Add(3 * time.Second)
			conversation()
			if err := s.Message(newest); err != nil {
				t.Fatal(err)
			}
			write(older) // newest timestamp is not the last batch element
			assertDigestActivity(t, s, newest.TS, 2, false)

			oldest := batchMessage("oldest", 4, "oldest prompt")
			oldest.TS = start.Add(time.Second)
			write(oldest) // no conversation metadata in this batch
			assertDigestActivity(t, s, newest.TS, 3, false)

			replacement := batchMessage("first", 1, "replacement prompt")
			replacement.TS = start.Add(6*time.Second - time.Nanosecond)
			conversation()
			write(replacement) // changed version uses fold, deferring counts
			assertDigestActivity(t, s, replacement.TS, 3, true)
			if err := pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return recountDigests(s.ctx, tx, s.dirtyConversations())
			}); err != nil {
				t.Fatal(err)
			}
			assertDigestActivity(t, s, replacement.TS, 3, false)

			// Harness activity still participates, with identical precision
			// and a monotonic timestamp even when later batches are older.
			harness = start.Add(8*time.Second - time.Nanosecond)
			conversation()
			if err := s.flush(); err != nil {
				t.Fatal(err)
			}
			assertDigestActivity(t, s, harness, 3, true) // sink remains dirty
			write(batchMessage("untimed", 5, "no timestamp"))
			assertDigestActivity(t, s, harness, 3, true)
			if err := pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
				return recountDigests(s.ctx, tx, []string{s.convIDs["batch-test"]})
			}); err != nil {
				t.Fatal(err)
			}
			assertDigestActivity(t, s, harness, 4, false)
		})
	}
}

// Recount must repair an old timestamp from stored rows even with no
// batch messages. Digest count filters do not define activity: superseded
// versions and inactive-path rows were ingested too.
func TestDigestRecountStoredActivity(t *testing.T) {
	for _, row := range []string{"live", "superseded", "inactive", "superseded-inactive"} {
		t.Run(row, func(t *testing.T) {
			s := batchTestSink(t)
			start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := s.Conversation(&transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "batch-test", StartedAt: start}); err != nil {
				t.Fatal(err)
			}
			m := batchMessage("stored", 1, "stored prompt")
			m.TS = start.Add(time.Second)
			if err := s.Message(m); err != nil {
				t.Fatal(err)
			}
			older := batchMessage("older", 2, "older stored prompt")
			older.TS = start
			if err := s.Message(older); err != nil {
				t.Fatal(err)
			}
			if err := s.flush(); err != nil {
				t.Fatal(err)
			}
			conv := s.convIDs["batch-test"]
			newest := start.Add(4*time.Second - time.Nanosecond).Truncate(time.Microsecond)
			superseded := row == "superseded" || row == "superseded-inactive"
			active := row == "live" || row == "superseded"
			if _, err := s.pool.Exec(s.ctx, `UPDATE messages SET ts=$2,superseded=$3,on_active_path=$4 WHERE conversation_id=$1 AND native_id='stored'`, conv, newest, superseded, active); err != nil {
				t.Fatal(err)
			}
			// Simulate a legacy activity row, both missing and stale. The
			// second pass also covers an absent digest forcing a recount.
			for _, missing := range []bool{false, true} {
				var stored *time.Time
				if !missing {
					stored = &start
				}
				if _, err := s.pool.Exec(s.ctx, `UPDATE conversation_activity SET last_activity_at=$2,digest=CASE WHEN $3 THEN NULL ELSE digest END WHERE conversation_id=$1`, conv, stored, missing); err != nil {
					t.Fatal(err)
				}
				if err := pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
					if missing {
						if _, err := lockWithParents(s.ctx, tx, []string{conv}); err != nil {
							return err
						}
						return refreshDigest(s.ctx, tx, conv, nil, digestAppend, time.Time{})
					}
					return recountDigests(s.ctx, tx, []string{conv})
				}); err != nil {
					t.Fatal(err)
				}
				count := 1
				if row == "live" {
					count = 2
				}
				assertDigestActivity(t, s, newest, count, false)
			}
		})
	}
}

// Unchanged repeats advance legacy activity without being counted twice or
// requiring a full-history recount.
func TestDigestRepeatedLegacyActivity(t *testing.T) {
	s := batchTestSink(t)
	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Conversation(&transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "batch-test", StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	m := batchMessage("repeat", 1, "same prompt")
	m.TS = start.Add(2*time.Second - time.Nanosecond)
	if err := s.Message(m); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	assertDigestActivity(t, s, m.TS, 1, false)
	conv := s.convIDs["batch-test"]
	if _, err := s.pool.Exec(s.ctx, `UPDATE conversation_activity SET last_activity_at=$2 WHERE conversation_id=$1`, conv, start); err != nil {
		t.Fatal(err)
	}
	if err := s.Message(m); err != nil {
		t.Fatal(err)
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	assertDigestActivity(t, s, m.TS, 1, false)
	if err := pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return recountDigests(s.ctx, tx, []string{conv})
	}); err != nil {
		t.Fatal(err)
	}
	assertDigestActivity(t, s, m.TS, 1, false)
}

func assertDigestActivity(t *testing.T, s *sink, want time.Time, users int, stale bool) {
	t.Helper()
	var got, start, newest *time.Time
	var duration int64
	var count int
	var gotStale bool
	if err := s.pool.QueryRow(s.ctx, `SELECT a.last_activity_at,c.started_at,
		(SELECT max(ts) FROM messages WHERE conversation_id=c.id),
		COALESCE((a.digest->>'duration_s')::bigint,0),COALESCE((a.digest->'messages'->>'user')::int,0),a.digest_stale
		FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE c.id=$1`, s.convIDs["batch-test"]).
		Scan(&got, &start, &newest, &duration, &count, &gotStale); err != nil {
		t.Fatal(err)
	}
	want = want.Truncate(time.Microsecond)
	if got == nil || !got.Equal(want) {
		t.Fatalf("activity=%v, want %v", got, want)
	}
	if newest != nil && got.Before(*newest) {
		t.Fatalf("activity %v precedes newest stored message %v", got, newest)
	}
	if start == nil || duration != int64(got.Sub(*start)/time.Second) {
		t.Fatalf("digest duration=%d, activity=%v, start=%v", duration, got, start)
	}
	if count != users || gotStale != stale {
		t.Fatalf("users=%d stale=%v, want users=%d stale=%v", count, gotStale, users, stale)
	}
}
