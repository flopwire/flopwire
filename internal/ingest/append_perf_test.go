package ingest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Performance guards for append parse (notes/perf-guards.md, known
// violation #4): flushing and parsing a few lines appended to a live
// session costs the same however long the session is and however large
// the rest of the corpus. They gate rows touched and statements sent.

// appendBatch is how many records one measured append adds.
const appendBatch = 8

// appendSession is a synthetic Claude session on a device, synced and
// parsed, ready for appends.
type appendSession struct {
	e       *env
	sy      *devicesync.Syncer
	spec    devicesync.SourceSpec
	session perfguard.ClaudeSession
	next    int // the next record to append
	id      string
}

func newAppendSession(t testing.TB, e *env, records int) *appendSession {
	t.Helper()
	return newAppendSessionWith(t, e, records, devicesync.Config{SealAfter: -1})
}

func newAppendSessionWith(t testing.TB, e *env, records int, cfg devicesync.Config) *appendSession {
	t.Helper()
	a := &appendSession{e: e, sy: e.syncer(cfg), session: perfguard.ClaudeSession{SessionID: "a99e0000-0000-4000-8000-000000000001"}}
	path := filepath.Join(t.TempDir(), a.session.SessionID+".jsonl")
	if err := os.WriteFile(path, a.session.Lines(0, records), 0o644); err != nil {
		t.Fatal(err)
	}
	a.spec = devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}
	a.next = records
	if err := a.sy.Sync(e.ctx, a.spec); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE path=$1`, path).Scan(&a.id); err != nil {
		t.Fatal(err)
	}
	return a
}

// add appends lines to the file, then syncs and parses them as a live
// session does: one flush, one parse of the source.
func (a *appendSession) add(t testing.TB, lines []byte) {
	t.Helper()
	fh, err := os.OpenFile(a.spec.Path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.Write(lines); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.sy.Sync(a.e.ctx, a.spec); err != nil {
		t.Fatal(err)
	}
	if err := a.e.queue.ParseSource(a.e.ctx, a.id); err != nil {
		t.Fatal(err)
	}
}

// records appends the next k synthetic records.
func (a *appendSession) records(t testing.TB, k int) {
	t.Helper()
	a.add(t, a.session.Lines(a.next, a.next+k))
	a.next += k
}

// toolCalls is k tool calls (a tool_use and its result each), the first
// failed of them failing, continuing the session's record chain at record
// from.
func (a *appendSession) toolCalls(from, k, failed int) []byte {
	var b bytes.Buffer
	s := a.session
	for i := range k {
		use, res := from+2*i, from+2*i+1
		ts := func(r int) string { return fmt.Sprintf("2026-09-02T00:%02d:%02d.000Z", r/60%60, r%60) }
		fmt.Fprintf(&b, `{"type":"assistant","isSidechain":false,"userType":"external","cwd":"/workspace/perf","sessionId":%q,"version":"2.1.0","gitBranch":"main","uuid":%q,"parentUuid":%q,"timestamp":%q,"requestId":"req_f%d","message":{"id":"msg_f%d","type":"message","role":"assistant","model":"claude-opus-4-1","content":[{"type":"tool_use","id":"toolu_f%d","name":"Bash","input":{"command":"false"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":3}}}`+"\n",
			s.SessionID, s.UUID(use), s.UUID(use-1), ts(use), use, use, use)
		fmt.Fprintf(&b, `{"type":"user","isSidechain":false,"userType":"external","cwd":"/workspace/perf","sessionId":%q,"version":"2.1.0","gitBranch":"main","uuid":%q,"parentUuid":%q,"timestamp":%q,"message":{"role":"user","content":[{"tool_use_id":"toolu_f%d","type":"tool_result","is_error":%t,"content":"exit status 1"}]}}`+"\n",
			s.SessionID, s.UUID(res), s.UUID(use), ts(res), use, i < failed)
	}
	return b.Bytes()
}

// unrelatedCorpus adds n other sessions on the same device (4 messages
// each) and 8n redacted lines, none of which the append touches.
func unrelatedCorpus(t testing.TB, e *env, n int) {
	t.Helper()
	dir := t.TempDir()
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	for i := range n {
		s := perfguard.ClaudeSession{SessionID: fmt.Sprintf("0be70000-0000-4000-8000-%012d", i+1)}
		p := filepath.Join(dir, s.SessionID+".jsonl")
		lines := bytes.ReplaceAll(s.Lines(0, 4), []byte("5e550000-0000-4000-9000-"), fmt.Appendf(nil, "0be7%04x-0000-4000-9000-", i+1))
		if err := os.WriteFile(p, lines, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := sy.Sync(e.ctx, devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}); err != nil {
			t.Fatal(err)
		}
	}
	e.drain()
	e.exec(`INSERT INTO message_redactions(id,requested_by,message_id,all_copies,by_admin,messages,chunks,tails,created_at)
		VALUES('0be70000-0000-4000-a000-000000000001',$1,gen_random_uuid(),false,false,0,0,0,now())`, e.userID)
	e.exec(`INSERT INTO redacted_lines(line_sha,spans,redaction_id)
		SELECT sha256(convert_to('unrelated line '||i,'UTF8')),'[{"start":1,"end":4}]','0be70000-0000-4000-a000-000000000001' FROM generate_series(1,$1::int) i`, 8*n)
}

// Appending to a session costs the same at n and 8n prior messages: the
// flush and the parse read the new bytes and the rows they write, not the
// whole manifest or conversation.
func TestPerfAppendConstantInSessionLength(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 500, 8, func(_ testing.TB, n int) perfguard.Cost {
		e, counter := perfEnv(t)
		a := newAppendSession(t, e, n)
		a.records(t, appendBatch) // warm: the first append after a full parse
		e.exec(`ANALYZE`)
		return perfguard.Measure(t, e.pool, counter, func() { a.records(t, appendBatch) })
	})
}

// Appending to a session costs the same however many other sessions,
// sources and redacted lines the server holds.
func TestPerfAppendConstantInCorpus(t *testing.T) {
	perfguard.AssertScaling(t, perfguard.Constant, 8, 8, func(_ testing.TB, n int) perfguard.Cost {
		e, counter := perfEnv(t)
		a := newAppendSession(t, e, 64)
		unrelatedCorpus(t, e, n)
		a.records(t, appendBatch)
		e.exec(`ANALYZE`)
		return perfguard.Measure(t, e.pool, counter, func() { a.records(t, appendBatch) })
	})
}

// One append sends a bounded number of statements, however many of its
// tool calls failed: no statement per failed call id.
func TestPerfAppendStatementBound(t *testing.T) {
	const calls = 32
	cost := func(failed int) perfguard.Cost {
		e, counter := perfEnv(t)
		a := newAppendSession(t, e, 64)
		a.records(t, appendBatch)
		lines := a.toolCalls(a.next, calls, failed)
		a.next += 2 * calls
		c := perfguard.Measure(t, e.pool, counter, func() { a.add(t, lines) })
		sameDigest(t, e, a.session.SessionID)
		return c
	}
	few, many := cost(2), cost(calls)
	if many.Statements != few.Statements {
		t.Errorf("an append of %d calls sent %d statements with all of them failed, %d with 2: a statement per failed call", calls, many.Statements, few.Statements)
	}
	t.Logf("append of %d tool calls: %d statements", calls, many.Statements)
}

// An append's statements grow by at most one per added record: the
// insert of its row. Everything else (auth, the flush, the parse, the
// checkpoint, the digest) is per append, not per record or per failed
// call. Every record here is half of a failed tool call. Chunks are large
// enough that both appends travel as one tail, so chunk reservations do
// not vary with the append's size.
func TestPerfAppendStatementsPerRecord(t *testing.T) {
	const small, large = 8, 64
	cost := func(records int) int64 {
		e, counter := perfEnv(t)
		a := newAppendSessionWith(t, e, 64, devicesync.Config{SealAfter: -1,
			Chunk: devicesync.ChunkParams{Min: 256 << 10, Avg: 512 << 10, Max: 1 << 20}})
		a.records(t, appendBatch)
		lines := a.toolCalls(a.next, records/2, records/2)
		a.next += records
		counter.Reset() // a failure lists only the append's statements
		c := perfguard.Measure(t, e.pool, counter, func() { a.add(t, lines) })
		if t.Failed() {
			t.Logf("append of %d records:\n%s", records, counter)
		}
		return c.Statements
	}
	few, many := cost(small), cost(large)
	// One insert per added record, plus a small allowance for statements
	// that a larger append legitimately repeats (none today).
	const slack = 4
	if limit := few + (large - small) + slack; many > limit {
		t.Errorf("an append of %d records sent %d statements, one of %d sent %d: more than one more statement per record (limit %d)",
			large, many, small, few, limit)
	}
	t.Logf("statements: %d records → %d, %d records → %d", small, few, large, many)
}

// After many appends, the incrementally kept digest equals a full
// recount: every field, not only the counts.
func TestAppendDigestMatchesRecount(t *testing.T) {
	e := newEnv(t)
	a := newAppendSession(t, e, 40)
	for i := range 12 {
		if i%3 == 2 {
			n := 1 + i%4
			a.add(t, a.toolCalls(a.next, n+1, n))
			a.next += 2 * (n + 1)
		} else {
			a.records(t, 1+i%5)
		}
		sameDigest(t, e, a.session.SessionID)
	}
	if got := e.count(`SELECT count(*) FROM messages WHERE NOT superseded`); got != a.next {
		t.Fatalf("%d live rows, want %d", got, a.next)
	}
}

// sameDigest checks the session's stored digest against a full recount of
// its live rows, field by field.
func sameDigest(t testing.TB, e *env, session string) {
	t.Helper()
	var conv, stored string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text,digest::text FROM conversations WHERE session_id=$1`, session).Scan(&conv, &stored); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if err := recountDigests(e.ctx, tx, []string{conv}); err != nil {
		t.Fatal(err)
	}
	var same bool
	var recounted string
	if err := tx.QueryRow(e.ctx, `SELECT digest::text, digest=$2::jsonb FROM conversations WHERE id=$1`, conv, stored).Scan(&recounted, &same); err != nil {
		t.Fatal(err)
	}
	if !same {
		t.Fatalf("stored digest differs from a recount\nstored:    %s\nrecounted: %s", stored, recounted)
	}
}
