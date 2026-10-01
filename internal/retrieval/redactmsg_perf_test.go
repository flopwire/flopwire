package retrieval

import (
	"context"
	"testing"

	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// copyCorpus is perfCorpus with sessions sessions of eight messages, plus
// a record copied into three of them: the same text (content_sha) and
// native id, as the same session archived from three devices. It returns
// the first copy's id and conversation, the shared hash and native id.
func copyCorpus(t testing.TB, sessions int) (*Store, *perfguard.Counter, string, string, []byte, string) {
	t.Helper()
	s, counter := perfCorpus(t, sessions, 8)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,source_id,ordinal,kind,text,text_len,content_sha,native_id,source_generation,parser)
		SELECT md5('copy'||i)::uuid,md5('c'||i)::uuid,md5('f'||i)::uuid,100,'user','the copied record',17,sha256('the copied record'),'rec-1',0,'test'
		FROM generate_series(1,3) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	var id, conv string
	var sha []byte
	if err := s.Pool.QueryRow(ctx, `SELECT id::text,conversation_id::text,content_sha FROM messages WHERE id=md5('copy1')::uuid`).Scan(&id, &conv, &sha); err != nil {
		t.Fatal(err)
	}
	return s, counter, id, conv, sha, "rec-1"
}

// The queries a redaction runs while it holds the redacted-lines lock
// exclusively (every flush and parse write waits for it) are index
// probes: the target re-read, with and without all_copies, and the
// byte-copy probe on content_sha.
func TestRedactionProbesUnderLockPlan(t *testing.T) {
	s, _, id, conv, sha, native := copyCorpus(t, 200)
	perfguard.AssertIndexedPlan(t, s.Pool, byteCopiesSQL, sha, native)
	for _, all := range []bool{false, true} {
		perfguard.AssertIndexedPlan(t, s.Pool, redactTargetsSQL+" FOR UPDATE OF m", id, conv, native, all, sha, false, uuidZero, []string{id})
	}
}

// Their cost does not grow with the corpus.
func TestRedactionProbesUnderLockScaleConstant(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(t testing.TB, n int) perfguard.Cost {
		s, counter, id, conv, sha, native := copyCorpus(t, max(n, 3))
		return perfguard.Measure(t, s.Pool, counter, func() {
			probe(t, s.Pool, id, conv, sha, native)
		})
	})
}

func probe(t testing.TB, pool *pgxpool.Pool, id, conv string, sha []byte, native string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := ingest.LockRedactedLines(ctx, tx); err != nil {
			return err
		}
		targets, err := redactTargets(ctx, tx, true, id, conv, &native, true, sha, true, uuidZero, nil)
		if err != nil {
			return err
		}
		copies, err := byteCopyCandidates(ctx, tx, sha, &native)
		if err != nil {
			return err
		}
		if len(targets) != 3 || len(copies) != 3 {
			t.Fatalf("%d targets, %d copies; want 3 each", len(targets), len(copies))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// uuidZero is a caller that owns none of the copies (the owner route).
const uuidZero = "00000000-0000-0000-0000-000000000000"
