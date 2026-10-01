package ingest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/jackc/pgx/v5"
)

func refreshedSource(t *testing.T, e *env) string {
	t.Helper()
	sp := bulkSpec(t, 1)
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	e.drain()
	var id string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text FROM sources WHERE path=$1`, sp.Path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// The server's archive, not the still-live device file, must suffice.
	if err := os.Remove(sp.Path); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestReparseVersionsAndCheckpoint(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	var attempt, ordinal int64
	if err := e.pool.QueryRow(e.ctx, `SELECT parse_attempt,ordinal FROM messages WHERE source_id=$1 LIMIT 1`, id).Scan(&attempt, &ordinal); err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE source_parse_state SET applied_parser='claude@3.99',extraction_report=jsonb_set(extraction_report,'{contract}',to_jsonb(replace(extraction_report->>'contract','claude@3/','claude@3.99/'))) WHERE source_id=$1`, id)
	e.drain()
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND parse_attempt=$2`, id, attempt) != 1 {
		t.Fatal("minor change reparsed unchanged source")
	}
	e.exec(`UPDATE source_parse_state SET applied_redaction_rules='old-rules' WHERE source_id=$1`, id)
	// A failed final checkpoint must leave the source stale and old rows live.
	e.exec(`CREATE FUNCTION reject_version() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'blocked version checkpoint'; END $$`)
	e.exec(`CREATE TRIGGER reject_version BEFORE UPDATE ON source_parse_state FOR EACH ROW WHEN (NEW.applied_redaction_rules IS DISTINCT FROM OLD.applied_redaction_rules) EXECUTE FUNCTION reject_version()`)
	if err := e.queue.ParseSource(e.ctx, id); err == nil || !strings.Contains(err.Error(), "blocked version checkpoint") {
		t.Fatalf("checkpoint failure: %v", err)
	}
	if e.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_redaction_rules='old-rules'`, id) != 1 {
		t.Fatal("failed extraction stamped fresh")
	}
	e.exec(`DROP TRIGGER reject_version ON source_parse_state`)
	e.start() // Work is derived from durable stamps after restart.
	e.drain()
	if e.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_parser=$2 AND applied_redaction_rules=$3 AND requested_seq=parsed_seq`, id, claude.ParserName, redact.RulesVersion) != 1 {
		t.Fatal("refresh not completed")
	}
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded AND parser=$2 AND redaction_rules=$3`, id, claude.ParserName, redact.RulesVersion) != 1 {
		t.Fatal("row versions not stamped")
	}
	if next, err := e.queue.nextRefresh(e.ctx); err != nil || next != "" {
		t.Fatalf("refresh repeats: %q %v", next, err)
	}
	e.exec(`UPDATE messages SET kind='tool_call',role='assistant',ordinal=99,tool_call_id='stale-call',text=left(text,12),parser='claude@2.0' WHERE source_id=$1 AND NOT superseded`, id)
	e.exec(`UPDATE source_parse_state SET applied_parser='claude@2.99' WHERE source_id=$1`, id)
	e.drain()
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded AND kind='user' AND role='user' AND ordinal=$2 AND tool_call_id IS NULL AND strpos(text,'flux capacitor')>0`, id, ordinal) != 1 {
		t.Fatal("major refresh did not replace old metadata or capped text")
	}
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND parse_attempt=$2`, id, attempt) != 0 {
		t.Fatal("major change did not rederive")
	}
}

func TestReparsePriorityBackoffAndLiveQueue(t *testing.T) {
	e := newEnv(t)
	older := refreshedSource(t, e)
	newer := refreshedSource(t, e)
	e.exec(`UPDATE generations SET captured_at=now()-interval '1 day' WHERE source_id=$1`, older)
	e.exec(`UPDATE source_parse_state SET applied_redaction_rules='old-rules'`)
	if next, err := e.queue.nextRefresh(e.ctx); err != nil || next != newer {
		t.Fatalf("newest first: %q %v", next, err)
	}
	e.exec(`UPDATE messages SET source_id=$1 WHERE source_id=$2`, older, newer)
	var conv string
	if err := e.pool.QueryRow(e.ctx, `SELECT conversation_id::text FROM messages WHERE source_id=$1 LIMIT 1`, older).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	e.queue.RefreshSession(e.ctx, conv)
	if next, err := e.queue.nextRefresh(e.ctx); err != nil || next != older {
		t.Fatalf("read priority: %q %v", next, err)
	}
	e.exec(`UPDATE source_parse_state SET next_attempt_at=now()+interval '1 minute' WHERE source_id=$1`, older)
	if next, _ := e.queue.nextRefresh(e.ctx); next != newer {
		t.Fatal("refresh bypassed backoff")
	}
	e.exec(`UPDATE source_parse_state SET quarantined_at=now() WHERE source_id=$1`, newer)
	if next, _ := e.queue.nextRefresh(e.ctx); next != "" {
		t.Fatal("refresh bypassed quarantine")
	}
	// Upgrade discovery must not turn idle backlog into flush backpressure.
	if e.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq`) != 0 {
		t.Fatal("idle refresh entered live backlog")
	}
	e.exec(`UPDATE source_parse_state SET next_attempt_at=NULL,requested_seq=requested_seq+1 WHERE source_id=$1`, older)
	if next, _ := e.queue.nextRefresh(e.ctx); next != "" {
		t.Fatal("idle lane selected live ingestion")
	}
}

func TestParseFenceAcrossQueues(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	conn, err := pgx.ConnectConfig(e.ctx, e.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(e.ctx)
	if _, err := conn.Exec(e.ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "flopwire:source-parse:"+id); err != nil {
		t.Fatal(err)
	}
	other := &Queue{Pool: e.pool, Objects: e.objects}
	ctx, cancel := context.WithTimeout(e.ctx, time.Second)
	defer cancel()
	if err := other.ParseSource(ctx, id); !errors.Is(err, errParseBusy) {
		t.Fatalf("source fence bypassed: %v", err)
	}
	conn.Exec(e.ctx, `SELECT pg_advisory_unlock_all()`)
	if _, err := conn.Exec(e.ctx, `SELECT pg_advisory_lock(hashtextextended('flopwire:idle-reparse',0))`); err != nil {
		t.Fatal(err)
	}
	if err := other.refreshSource(ctx, id); !errors.Is(err, errParseBusy) {
		t.Fatalf("idle global fence bypassed: %v", err)
	}
	// A global idle lease does not block ordinary live ingestion.
	if err := other.ParseSource(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestRuleUpgradeMasksHistoricalRowsAndSummaries(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	secret := "ghp_" + strings.Repeat("aB9x", 9)
	var message, conv string
	if err := e.pool.QueryRow(e.ctx, `SELECT id::text,conversation_id::text FROM messages WHERE source_id=$1 LIMIT 1`, id).Scan(&message, &conv); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(secret))
	e.exec(`UPDATE messages SET text=$2,content_sha=$3,superseded=true,redaction_rules='old-rules',enrichment=jsonb_build_object('secret',$2::text) WHERE id=$1`, message, secret, sum[:])
	e.exec(`INSERT INTO messages(id,conversation_id,source_id,native_id,part,ordinal,kind,text,text_len,content_sha,version,superseded,source_generation,parser,enrichment,redaction_rules)
 SELECT gen_random_uuid(),m.conversation_id,m.source_id,m.native_id,m.part,m.ordinal,m.kind,m.text,m.text_len,m.content_sha,g+1,true,m.source_generation,m.parser,m.enrichment,'old-rules'
 FROM messages m CROSS JOIN generate_series(1,65) g WHERE m.id=$1`, message)
	e.exec(`UPDATE conversations SET title=$2,digest=jsonb_build_object('intent',$2::text,'state',jsonb_build_object('f',$2::text)) WHERE id=$1`, conv, secret)
	e.exec(`UPDATE source_parse_state SET applied_redaction_rules='old-rules' WHERE source_id=$1`, id)
	e.drain()
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND (strpos(text,$2)>0 OR strpos(enrichment::text,$2)>0)`, id, secret) != 0 {
		t.Fatal("historical version or enrichment retains secret")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id=$1 AND (strpos(title,$2)>0 OR strpos(digest::text,$2)>0)`, conv, secret) != 0 {
		t.Fatal("stored summary retains secret")
	}
	if e.count(`SELECT count(*) FROM messages WHERE id=$1 AND superseded`, message) != 1 {
		t.Fatal("historical version disappeared")
	}
}

func TestHistoryMaskContentionIsRetryable(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	e.exec(`UPDATE messages SET redaction_rules='old-rules' WHERE source_id=$1`, id)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if _, err := tx.Exec(e.ctx, `SELECT id FROM messages WHERE source_id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	if err := e.queue.maskStoredVersions(e.ctx, id); !errors.Is(err, ErrArchiveChanged) {
		t.Fatalf("transient row lock counted as failure: %v", err)
	}
}

func TestIdleWorkerWakesForReadPriority(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	worker := &Queue{Pool: e.pool, Objects: e.objects, Log: e.queue.Log, RefreshInterval: time.Hour, Sweep: time.Hour}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { defer close(done); worker.Run(ctx) }()
	defer func() { cancel(); <-done }()
	// Let the empty worker enter its long idle wait. A read must wake it
	// instead of waiting for the next periodic scan.
	time.Sleep(100 * time.Millisecond)
	e.exec(`UPDATE source_parse_state SET applied_redaction_rules='old-rules' WHERE source_id=$1`, id)
	var conv string
	if err := e.pool.QueryRow(e.ctx, `SELECT conversation_id::text FROM messages WHERE source_id=$1 LIMIT 1`, id).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	worker.RefreshSession(e.ctx, conv)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if e.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_redaction_rules=$2 AND refresh_requested_at IS NULL`, id, redact.RulesVersion) == 1 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("read did not wake idle refresh")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A full reparse recounts the digests of the conversations whose rows it
// replaced once, at the end, not per batch. When it fails after writing
// them, the digest must still be recounted to match the rows left live.
func TestFailedReparseRecountsDigest(t *testing.T) {
	e := newEnv(t)
	id := refreshedSource(t, e)
	e.exec(`UPDATE messages SET kind='tool_call',parser='claude@2.0' WHERE source_id=$1`, id)
	e.exec(`UPDATE conversations SET digest=jsonb_set(digest,'{messages}','{"tool_call":1}') WHERE id IN (SELECT conversation_id FROM messages WHERE source_id=$1)`, id)
	e.exec(`UPDATE source_parse_state SET applied_parser='claude@2.99' WHERE source_id=$1`, id)
	e.exec(`CREATE FUNCTION reject_version() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'blocked version checkpoint'; END $$`)
	e.exec(`CREATE TRIGGER reject_version BEFORE UPDATE ON source_parse_state FOR EACH ROW WHEN (NEW.applied_parser IS DISTINCT FROM OLD.applied_parser) EXECUTE FUNCTION reject_version()`)
	if err := e.queue.ParseSource(e.ctx, id); err == nil || !strings.Contains(err.Error(), "blocked version checkpoint") {
		t.Fatalf("checkpoint failure: %v", err)
	}
	if e.count(`SELECT count(*) FROM messages WHERE source_id=$1 AND NOT superseded AND kind='user'`, id) != 1 {
		t.Fatal("test did not replace the stored row")
	}
	if e.count(`SELECT count(*) FROM conversations WHERE id IN (SELECT conversation_id FROM messages WHERE source_id=$1) AND digest->'messages'='{"user":1}'`, id) != 1 {
		t.Fatal("failed reparse left a digest that does not match its live rows")
	}
}

// A parse that replaces a conversation's rows still folds the messages it
// writes into the digest (intent, last reply, files): only the counts wait
// for the recount at the end.
func TestReplacingParseFoldsDigest(t *testing.T) {
	e := newEnv(t)
	const session = "0b7e2c1a-0000-4000-8000-0000000000f0"
	path := filepath.Join(t.TempDir(), session+".jsonl")
	line := func(kind, uuid, parent, text string, sec int) string {
		content := `"` + text + `"`
		if kind == "assistant" {
			content = `[{"type":"text","text":"` + text + `"}]`
		}
		return fmt.Sprintf(`{"type":%q,"uuid":%q,"parentUuid":%q,"sessionId":%q,"timestamp":"2026-09-20T00:00:%02dZ","message":{"id":"msg_%s","role":%q,"content":%s}}`+"\n",
			kind, uuid, parent, session, sec, uuid, kind, content)
	}
	appendFile(t, path, line("user", "u1", "", "fix the flux capacitor", 1)+line("assistant", "a1", "u1", "first answer", 2))
	sp := devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	// A rewrite: the first prompt is grows (its row is replaced in place) and
	// a reply follows.
	if err := os.WriteFile(path, []byte(line("user", "u1", "", "fix the flux capacitor now", 1)+line("assistant", "a1", "u1", "first answer", 2)+
		line("user", "u2", "a1", "and the warp core", 3)+line("assistant", "a2", "u2", "second answer", 4)), 0o644); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, sp)
	e.drain()
	if e.count(`SELECT count(*) FROM messages WHERE native_id LIKE 'u1%' AND NOT superseded AND text LIKE '%now'`) != 1 {
		t.Fatal("test did not replace a stored row")
	}
	var last string
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(digest->>'last','') FROM conversations WHERE session_id=$1`, session).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != "second answer" {
		t.Fatalf("digest last reply %q, want the replacing parse's %q", last, "second answer")
	}
	sameCounts(t, e, session)
}
