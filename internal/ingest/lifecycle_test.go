package ingest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/google/uuid"
)

func sync1(t *testing.T, sy *devicesync.Syncer, sp devicesync.SourceSpec) {
	t.Helper()
	if err := sy.Sync(context.Background(), sp); err != nil {
		t.Fatalf("sync %s: %v", sp.Path, err)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// bulkSpec writes a synthetic Claude transcript of n lines.
func bulkSpec(t *testing.T, n int) devicesync.SourceSpec {
	t.Helper()
	path := filepath.Join(t.TempDir(), "0b7e2c1a-0000-4000-8000-0000000000b1.jsonl")
	var b bytes.Buffer
	for i := range n {
		fmt.Fprintf(&b, `{"type":"user","uuid":"bulk-%04d","sessionId":"0b7e2c1a-0000-4000-8000-0000000000b1","timestamp":"2026-09-20T00:%02d:%02dZ","message":{"role":"user","content":"bulk line %d about the flux capacitor, padded %s"}}`+"\n",
			i, i/60%60, i%60, i, noise(i, 300))
	}
	appendFile(t, path, b.String())
	return devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"}
}

// noise is n pseudo-random bytes as base64, the same for the same seed:
// padding that compresses about as little as base64 does, so a test that
// needs several requests still gets them with compressed bodies.
func noise(seed, n int) string {
	r := rand.New(rand.NewPCG(uint64(seed), 1))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return base64.StdEncoding.EncodeToString(b)
}

func fileIDOf(t *testing.T, path string) string {
	t.Helper()
	id, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return id.ID.String()
}

// restartingTransport restarts the server after the Nth flush commits,
// losing its answer, as a crash between commit and reply would.
type restartingTransport struct {
	e     *env
	n, at int
}

func (r *restartingTransport) Has(ctx context.Context, h []syncproto.Hash) ([]syncproto.Hash, error) {
	return r.e.client.Has(ctx, h)
}

func (r *restartingTransport) Flush(ctx context.Context, req *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	resp, err := r.e.client.Flush(ctx, req)
	if r.n++; r.n == r.at {
		r.e.start()
		return nil, &syncproto.HTTPError{Status: 503, Body: syncproto.ErrorResponse{Code: "unavailable"}}
	}
	return resp, err
}

// The server restarts mid-stream, after committing a flush it never
// answered. The device retries; the archive holds the file exactly, and
// the parse request survived in Postgres.
func TestServerRestartMidStream(t *testing.T) {
	e := newEnv(t)
	sp := bulkSpec(t, 400)
	dir := t.TempDir()
	st, _ := devicesync.OpenStore(filepath.Join(dir, "s.db"))
	defer st.Close()
	spool, _ := devicesync.OpenSpool(filepath.Join(dir, "spool"), 1<<30)
	tr := &restartingTransport{e: e, at: 3}
	sy, err := devicesync.NewSyncer(devicesync.Config{Chunk: devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, MaxRequestBytes: 20 << 10, SealAfter: -1}, st, spool, tr)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	first := e.queue
	if err = sy.Sync(context.Background(), sp); !syncproto.Retryable(err) {
		t.Fatalf("want a retryable failure at the restart, got %v", err)
	}
	if first == e.queue {
		t.Fatal("server did not restart")
	}
	sync1(t, sy, sp)
	if tr.n < 5 {
		t.Fatalf("only %d flushes; the file should need several", tr.n)
	}
	got, err := e.reconstruct(sp.Path, fileIDOf(t, sp.Path), 0)
	orig, _ := os.ReadFile(sp.Path)
	if err != nil || !bytes.Equal(got, orig) {
		t.Fatalf("reconstruct: %d of %d bytes, err %v", len(got), len(orig), err)
	}
	if n := e.count(`SELECT count(*) FROM source_parse_state WHERE requested_seq>parsed_seq`); n != 1 {
		t.Fatalf("pending parse requests: %d", n)
	}
	if n := e.count(`SELECT count(*) FROM provisional_tails`); n != 1 {
		t.Fatalf("provisional tails: %d", n)
	}
	// The restarted server's queue parses what the first one never did.
	e.drain()
	if n := e.count(`SELECT count(*) FROM messages WHERE NOT superseded AND native_id LIKE 'bulk-%'`); n != 400 {
		t.Fatalf("parsed %d of 400 rows", n)
	}
}

// Quota: a flush that would pass the collection policy is refused with a
// retryable 507 and commits nothing; backpressure: a parse queue that is
// behind refuses flushes with a retryable 503; only device credentials
// sync; a tombstoned source acknowledges and discards uploads.
func TestQuotaBackpressureCredentialsTombstone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	sp := bulkSpec(t, 50)
	e.exec(`UPDATE collection_policy SET max_storage_bytes=100`)
	err := e.syncer(devicesync.Config{}).Sync(ctx, sp)
	var he *syncproto.HTTPError
	if !errors.As(err, &he) || he.Status != 507 || he.Body.Code != "quota_exceeded" || !he.Retryable() {
		t.Fatalf("quota: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM manifest_entries`); n != 0 {
		t.Fatalf("quota: %d entries committed", n)
	}
	e.exec(`UPDATE collection_policy SET max_storage_bytes=0`)

	e.queue.init()
	e.queue.backlog.Store(e.queue.MaxBacklog + 1)
	err = e.syncer(devicesync.Config{}).Sync(ctx, sp)
	if !errors.As(err, &he) || he.Status != 503 || he.Body.Code != "parse_backlog" {
		t.Fatalf("backpressure: %v", err)
	}
	e.queue.backlog.Store(0)
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	var source string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE path=$1`, sp.Path).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND requested_seq>parsed_seq`, source); n != 1 {
		t.Fatal("no parse requested")
	}

	// A login session is not a device.
	plain, hash, _ := auth.NewToken()
	e.exec(`INSERT INTO credentials(id,user_id,kind,token_hash,created_at) VALUES($1,$2,'session',$3,now())`, uuid.NewString(), e.userID, hash)
	c := *e.client
	c.Token = plain
	if _, err := c.Has(ctx, nil); !errors.As(err, &he) || he.Status != 403 {
		t.Fatalf("session credential: %v", err)
	}

	// Delete the conversation the source holds: its evidence is purged and
	// the source row stays as a tombstone that swallows uploads.
	conv := uuid.NewString()
	e.exec(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id) VALUES($1,$2,'claude','s-b1',$3,$4)`, conv, source, e.deviceID, e.userID)
	if _, err := e.store.RequestConversationDeletion(ctx, conv, e.userID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	appendFile(t, sp.Path, `{"type":"user","uuid":"after","message":{"role":"user","content":"after delete"}}`+"\n")
	sync1(t, sy, sp)
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp) // a device that lost its state
	if n := e.count(`SELECT count(*) FROM generations WHERE source_id=$1`, source); n != 0 {
		t.Fatalf("tombstoned source stored %d generations", n)
	}
	if n := e.count(`SELECT count(*) FROM chunks WHERE state='committed'`); n != 0 {
		t.Fatalf("%d committed chunks after the purge", n)
	}
}

func (f *fixture) spec(t *testing.T, suffix string) devicesync.SourceSpec {
	t.Helper()
	for _, sp := range f.transcripts {
		if strings.HasSuffix(sp.Path, suffix) {
			return sp
		}
	}
	t.Fatalf("no transcript %s", suffix)
	return devicesync.SourceSpec{}
}

// Appends extend a generation in place; a rewrite starts a new one whose
// missing rows are superseded, never deleted.
func TestAppendAndRewrite(t *testing.T) {
	e := newEnv(t)
	f := newFixture(t)
	sp := f.spec(t, "c0de.jsonl")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	orig, _ := os.ReadFile(sp.Path)
	lines := strings.SplitAfter(strings.TrimRight(string(orig), "\n"), "\n")
	live := func() int {
		return e.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.path=$1 AND NOT m.superseded`, sp.Path)
	}
	before := live()

	extra := `{"timestamp":"2026-09-23T10:09:00.000Z","type":"response_item","payload":{"type":"message","id":"msg_appended","role":"assistant","content":[{"type":"output_text","text":"appended reply zebra"}]}}` + "\n"
	appendFile(t, sp.Path, extra)
	sync1(t, sy, sp)
	e.drain()
	if got := live(); got != before+1 {
		t.Fatalf("after append: %d live rows, want %d", got, before+1)
	}
	if n := e.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, sp.Path); n != 1 {
		t.Fatalf("append made %d generations", n)
	}
	// The append added to the digest's counts: they equal a recount.
	sameCounts(t, e, "%c0de")

	// Truncate to the first half: a new generation.
	half := strings.Join(lines[:len(lines)/2], "")
	if !strings.HasSuffix(half, "\n") {
		half += "\n"
	}
	if err := os.WriteFile(sp.Path, []byte(half), 0o644); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, sp)
	e.drain()
	sameRows(t, pick(e.live(), "codex|"), pick(f.want(t), "codex|"))
	if n := e.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.path=$1 AND m.superseded AND m.superseded_in_generation=1`, sp.Path); n == 0 {
		t.Fatal("no row superseded by the rewrite")
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE native_id='msg_appended' AND superseded`); n != 1 {
		t.Fatal("appended row not kept as superseded")
	}
	// The digest counts the rows the rewrite left live, not the ones it
	// superseded.
	rows := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.session_id LIKE '%c0de' AND NOT m.superseded AND m.on_active_path IS NOT FALSE`)
	counted := e.count(`SELECT COALESCE(sum(v::int),0) FROM conversations c, jsonb_each_text(c.digest->'messages') AS x(k,v) WHERE c.session_id LIKE '%c0de'`)
	if counted != rows {
		t.Fatalf("digest counts %d messages; %d are live", counted, rows)
	}
}

// pick keeps the sessions whose key has the prefix and that the other side
// could know about (only synced sources).
func pick(m map[string][]string, prefix string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		if strings.HasPrefix(k, prefix) && strings.HasSuffix(k, "c0de") {
			out[k] = v
		}
	}
	return out
}

// A deleted conversation stays deleted: uploads to its source are
// acknowledged and discarded, and the same session at a new file identity
// is parsed, found tombstoned, and dropped.
func TestTombstoneBlocksReupload(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	sp := f.spec(t, "000000000001.jsonl")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	const session = "0b7e2c1a-0000-4000-8000-000000000001"
	var conv string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE session_id=$1`, session).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RequestConversationDeletion(ctx, conv, e.userID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	gone := func(when string) {
		t.Helper()
		e.drain()
		if n := e.count(`SELECT count(*) FROM conversations WHERE session_id=$1`, session); n != 0 {
			t.Fatalf("%s: conversation resurrected", when)
		}
		if n := e.count(`SELECT count(*) FROM generations g JOIN sources s ON s.id=g.source_id WHERE s.path=$1`, sp.Path); n != 0 {
			t.Fatalf("%s: raw evidence stored again (%d generations)", when, n)
		}
	}
	gone("after deletion")

	// The same device appends: acknowledged, discarded.
	appendFile(t, sp.Path, `{"type":"user","uuid":"u-after-delete","sessionId":"`+session+`","timestamp":"2026-09-25T00:00:00Z","message":{"role":"user","content":"after delete"}}`+"\n")
	sync1(t, sy, sp)
	gone("append")
	// A device that lost its state re-sends the whole file.
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sp)
	gone("fresh device state")
	// A new file identity at the path: a new source, parsed, found
	// tombstoned by session, and dropped.
	data, _ := os.ReadFile(sp.Path)
	os.Remove(sp.Path)
	if err := os.WriteFile(sp.Path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, sp)
	gone("new file identity")
	if n := e.count(`SELECT count(*) FROM sources WHERE path=$1 AND tombstoned_at IS NULL`, sp.Path); n != 0 {
		t.Fatalf("%d live sources for the deleted session", n)
	}
	// Chunks nothing references any more go to the orphan reconciler.
	if n := e.count(`SELECT count(*) FROM chunks c WHERE state='committed' AND NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=c.hash)`); n != 0 {
		t.Fatalf("%d committed chunks without references", n)
	}
}

// A Devin session deleted from the store becomes superseded; a main-chain
// change moves on_active_path.
func TestDevinDeletionAndChainChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	sy := e.syncer(devicesync.Config{})
	f.syncAll(t, sy)
	e.drain()
	db, err := sql.Open("sqlite", "file:"+f.devinDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	resync := func(id string) {
		t.Helper()
		data, err := devin.Export(ctx, f.devinDB, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := sy.SyncExport(ctx, devinSpec(f.devinDB, id), data); err != nil {
			t.Fatal(err)
		}
		e.drain()
	}
	onPath := func(native string) string {
		var v *bool
		if err := e.pool.QueryRow(ctx, `SELECT on_active_path FROM messages WHERE native_id=$1 AND NOT superseded`, native).Scan(&v); err != nil {
			t.Fatalf("%s: %v", native, err)
		}
		return fmt.Sprint(*v)
	}
	if onPath("dm-004") != "false" || onPath("dm-006") != "true" {
		t.Fatal("initial active path")
	}
	// Revert to the abandoned branch: node 4 becomes the tip.
	if _, err := db.Exec(`UPDATE sessions SET main_chain_id=4 WHERE id='devin-oracle-001'`); err != nil {
		t.Fatal(err)
	}
	resync("devin-oracle-001")
	if onPath("dm-004") != "true" || onPath("dm-006") != "false" {
		t.Fatal("active path after revert")
	}
	// Delete a whole session.
	for _, q := range []string{`DELETE FROM message_nodes WHERE session_id='devin-oracle-002'`, `DELETE FROM sessions WHERE id='devin-oracle-002'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	resync("devin-oracle-002")
	if n := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.session_id='devin-oracle-002' AND NOT m.superseded`); n != 0 {
		t.Fatalf("%d live rows of a deleted session", n)
	}
	if n := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.session_id='devin-oracle-002' AND m.superseded`); n == 0 {
		t.Fatal("deleted session's rows were dropped")
	}
}

// A subagent's meta.json that reaches the server after the subagent
// transcript was parsed still applies, and is linked to the subagent even
// when the device named the session's main transcript as its parent (it
// listed the sidecar before the transcript existed). Found by the
// two-device e2e, scenario h.
func TestMetaSidecarAfterSubagent(t *testing.T) {
	e := newEnv(t)
	f := newFixture(t)
	sub := f.spec(t, "agent-a1b2c3.jsonl")
	main := f.spec(t, "0b7e2c1a-0000-4000-8000-000000000001.jsonl")
	meta := strings.TrimSuffix(sub.Path, ".jsonl") + ".meta.json"
	if err := os.Rename(meta, meta+".later"); err != nil {
		t.Fatal(err)
	}
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, main)
	sync1(t, sy, sub)
	e.drain()
	agentType := func() string {
		var s *string
		if err := e.pool.QueryRow(e.ctx, `SELECT extra->>'agent_type' FROM conversations WHERE device_id=$1 AND session_id='agent-a1b2c3'`, e.deviceID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			return ""
		}
		return *s
	}
	if got := agentType(); got != "" {
		t.Fatalf("agent type %q before the sidecar arrived", got)
	}
	if err := os.Rename(meta+".later", meta); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, devicesync.SourceSpec{Path: meta, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: main.Path})
	e.drain()
	if got := agentType(); got != "general-purpose" {
		t.Fatalf("agent type %q after the sidecar arrived, want general-purpose", got)
	}
	if n := e.count(`SELECT count(*) FROM sources m JOIN sources s ON s.id=m.parent_source_id WHERE m.path=$1 AND s.path=$2`, meta, sub.Path); n != 1 {
		t.Fatal("sidecar not linked to its subagent transcript")
	}
}

// D9: a deletion forgets the session for its user on every device, and a
// subagent transcript of a deleted session that arrives later is dropped
// and tombstoned too. Dropping raw evidence at parse time is audited (V3).
func TestDeletionAcrossDevicesAndLateSubagents(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	main := f.spec(t, "0b7e2c1a-0000-4000-8000-000000000001.jsonl")
	sub := f.spec(t, "agent-a1b2c3.jsonl")
	const session = "0b7e2c1a-0000-4000-8000-000000000001"
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), main)
	e.drain()
	var conv string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE session_id=$1`, session).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RequestConversationDeletion(ctx, conv, e.userID, "", true); err != nil {
		t.Fatal(err)
	}
	// The subagent transcript reaches the server after the deletion.
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), sub)
	e.drain()
	if n := e.count(`SELECT count(*) FROM conversations WHERE session_id='agent-a1b2c3'`); n != 0 {
		t.Fatal("a deleted session's late subagent was kept")
	}
	if n := e.count(`SELECT count(*) FROM conversation_tombstones WHERE user_id=$1 AND session_id='agent-a1b2c3'`, e.userID); n != 1 {
		t.Fatal("the late subagent was not tombstoned")
	}

	// The user's second laptop syncs the same session.
	device2 := uuid.NewString()
	plain, hash, _ := auth.NewToken()
	e.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop-2','darwin',now())`, device2, e.userID)
	e.exec(`INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,now())`, uuid.NewString(), e.userID, device2, hash)
	first := e.client
	e.client = &syncproto.Client{Server: e.http.URL, Token: plain, HTTP: e.http.Client()}
	sync1(t, e.syncer(devicesync.Config{SealAfter: -1}), main)
	e.client = first
	e.drain()
	if n := e.count(`SELECT count(*) FROM conversations WHERE session_id=$1`, session); n != 0 {
		t.Fatal("the deleted session came back from the user's other device")
	}
	if n := e.count(`SELECT count(*) FROM sources WHERE device_id=$1 AND tombstoned_at IS NULL AND storage_kind<>'companion'`, device2); n != 0 {
		t.Fatalf("%d live sources on the second device", n)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action='source.tombstoned'`); n < 2 {
		t.Fatalf("parse-time tombstoning audited %d times", n)
	}
}

// D1: a transcript file the harness deletes (Claude prunes old sessions)
// keeps its rows live and searchable on the server; nothing supersedes
// them. Only a deleted Devin session is superseded
// (TestDevinDeletionAndChainChange).
func TestDeletedTranscriptFileKeepsRowsLive(t *testing.T) {
	e := newEnv(t)
	f := newFixture(t)
	sp := f.spec(t, "c0de.jsonl")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	live := func() int {
		return e.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.path=$1 AND NOT m.superseded`, sp.Path)
	}
	before := live()
	if before == 0 {
		t.Fatal("nothing parsed")
	}
	if err := os.Remove(sp.Path); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, sp)
	e.drain()
	if got := live(); got != before {
		t.Fatalf("after the file was deleted: %d live rows, want %d", got, before)
	}
}

// V4: a parse that must tombstone its source while a deletion job or
// backup holds the purge lock does not wait on it and does not count a
// failed attempt: it is requeued, and parses once the lock is free.
func TestPurgeLockBusyRequeuesWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := newFixture(t)
	sp := f.spec(t, "000000000001.jsonl")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	var conv string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE session_id='0b7e2c1a-0000-4000-8000-000000000001'`).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.RequestConversationDeletion(ctx, conv, e.userID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	e.drain()
	// The same session at a new file identity: its parse tombstones it.
	// Write the copy before the original goes, so Linux cannot hand the
	// freed inode straight back (remove-then-write keeps the identity there).
	data, _ := os.ReadFile(sp.Path)
	if err := os.WriteFile(sp.Path+".new", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sp.Path+".new", sp.Path); err != nil {
		t.Fatal(err)
	}
	sync1(t, sy, sp)
	var src string
	if err := e.pool.QueryRow(ctx, `SELECT s.id::text FROM sources s JOIN source_parse_state p ON p.source_id=s.id
		WHERE s.path=$1 AND p.requested_seq>p.parsed_seq`, sp.Path).Scan(&src); err != nil {
		t.Fatal(err)
	}
	holder, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, store.PurgeLockID); err != nil {
		t.Fatal(err)
	}
	q := &Queue{Pool: e.pool, Objects: e.objects, Log: e.queue.Log, MaxAttempts: 1}
	q.init()
	start := time.Now()
	q.work(ctx, src)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the parse waited %v for the purge lock", d)
	}
	var attempts int
	var quarantined, later bool
	if err := e.pool.QueryRow(ctx, `SELECT attempts,quarantined_at IS NOT NULL,next_attempt_at>now() FROM source_parse_state WHERE source_id=$1`, src).
		Scan(&attempts, &quarantined, &later); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || quarantined || !later {
		t.Fatalf("attempts %d, quarantined %v, requeued later %v", attempts, quarantined, later)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, store.PurgeLockID); err != nil {
		t.Fatal(err)
	}
	e.drain()
	if n := e.count(`SELECT count(*) FROM sources WHERE id=$1 AND tombstoned_at IS NOT NULL`, src); n != 1 {
		t.Fatal("source not tombstoned once the lock was free")
	}
}

// sameCounts checks that the stored digest of the session matching like
// holds what a full recount gives.
func sameCounts(t *testing.T, e *env, like string) {
	t.Helper()
	ctx := context.Background()
	var conv string
	var stored []byte
	if err := e.pool.QueryRow(ctx, `SELECT id::text,digest FROM conversations WHERE session_id LIKE $1`, like).Scan(&conv, &stored); err != nil {
		t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	want, err := digestCounts(ctx, tx, conv)
	if err != nil {
		t.Fatal(err)
	}
	got := digest.Parse(stored)
	var tokens digest.Tokens
	if got.Tokens != nil {
		tokens = *got.Tokens
	}
	if fmt.Sprint(got.Messages) != fmt.Sprint(want.Messages) || fmt.Sprint(got.Tools) != fmt.Sprint(want.Tools) || got.Failed != want.Failed || tokens != want.Tokens {
		t.Fatalf("stored counts %v %v failed %d tokens %+v; recount %v %v failed %d tokens %+v",
			got.Messages, got.Tools, got.Failed, tokens, want.Messages, want.Tools, want.Failed, want.Tokens)
	}
}

// A parse that runs again from an older cursor (a crash or error after its
// rows were written, before the cursor was saved) re-emits rows the
// server already holds, unchanged: the digest must not count them twice.
func TestDigestCountsSurviveARepeatedParse(t *testing.T) {
	e := newEnv(t)
	f := newFixture(t)
	sp := f.spec(t, "c0de.jsonl")
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	sameCounts(t, e, "%c0de")
	e.exec(`UPDATE source_parse_state SET cursor_offset=0,cursor_line=0,cursor_state=NULL,requested_seq=requested_seq+1`)
	e.drain()
	sameCounts(t, e, "%c0de")
}
