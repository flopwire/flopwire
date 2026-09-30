package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/google/uuid"
)

// codename is text no rule catches: only a message redaction hides it.
const codename = "codename BLUEFALCON-7731 is internal"

var hidden = "first line\n" + codename + "\nlast line"

func jline(v any) string {
	b, _ := json.Marshal(v)
	return string(b) + "\n"
}

// writeRedactFixtures writes one Claude session and one Codex rollout,
// each holding the same user message, and returns their specs and the
// Devin export (same text again).
func (s *server) writeRedactFixtures() ([]devicesync.SourceSpec, devicesync.SourceSpec, []byte) {
	t := s.t
	sess := "30000000-0000-4000-8000-000000000010"
	cl := filepath.Join(s.home, ".claude", "projects", "-w-redact", sess+".jsonl")
	cx := filepath.Join(s.home, ".codex", "sessions", "2026", "09", "30", "rollout-2026-09-30T12-00-00-30000000-0000-4000-8000-0000000000c1.jsonl")
	for _, d := range []string{filepath.Dir(cl), filepath.Dir(cx)} {
		os.MkdirAll(d, 0o700)
	}
	claudeData := jline(map[string]any{"type": "user", "uuid": "30000000-0000-4000-8000-000000000001", "timestamp": "2026-09-30T12:00:00.000Z",
		"sessionId": sess, "cwd": "/w/redact", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": hidden}}}}) +
		jline(map[string]any{"type": "assistant", "uuid": "30000000-0000-4000-8000-000000000002", "parentUuid": "30000000-0000-4000-8000-000000000001",
			"timestamp": "2026-09-30T12:00:01.000Z", "sessionId": sess, "cwd": "/w/redact",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "noted, nothing else here"}}}})
	// Filler after the message, so it lands in a finalized chunk rather
	// than the provisional tail.
	for i := range 60 {
		claudeData += jline(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("30000000-0000-4000-8000-1%011d", i), "timestamp": "2026-09-30T12:00:02.000Z",
			"sessionId": sess, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("filler %d %s", i, strings.Repeat("lorem ipsum ", 15))}}}})
	}
	codexData := jline(map[string]any{"timestamp": "2026-09-30T12:00:00Z", "type": "session_meta", "payload": map[string]any{"id": "30000000-0000-4000-8000-0000000000c1",
		"timestamp": "2026-09-30T12:00:00Z", "cwd": "/w/redact", "originator": "fixture", "cli_version": "1.0.0", "source": "cli"}}) +
		jline(map[string]any{"timestamp": "2026-09-30T12:00:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "id": "msg_r1", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": hidden}}}})
	for i := range 60 {
		codexData += jline(map[string]any{"timestamp": "2026-09-30T12:00:02Z", "type": "response_item", "payload": map[string]any{"type": "message", "id": fmt.Sprintf("msg_f%d", i), "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("filler %d %s", i, strings.Repeat("dolor sit ", 18))}}}})
	}
	if err := os.WriteFile(cl, []byte(claudeData), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cx, []byte(codexData), 0o600)
	inner, _ := json.Marshal(map[string]any{"message_id": "dr-1", "role": "user", "content": hidden, "metadata": map[string]any{"is_user_input": true}})
	export := jline(map[string]any{"t": "session", "id": "devin-redact-2", "working_directory": "/w/redact", "created_at": 1790157600, "last_activity_at": 1790157601, "main_chain_id": 1}) +
		jline(map[string]any{"t": "node", "row_id": 1, "node_id": 1, "created_at": 1790157601, "chat_message": string(inner)})
	for i := 2; i < 60; i++ {
		msg, _ := json.Marshal(map[string]any{"message_id": fmt.Sprintf("dr-%d", i), "role": "assistant", "content": fmt.Sprintf("filler %d %s", i, strings.Repeat("amet ", 30))})
		export += jline(map[string]any{"t": "node", "row_id": i, "node_id": i, "parent_node_id": i - 1, "created_at": 1790157601 + i, "chat_message": string(msg)})
	}
	specs := []devicesync.SourceSpec{
		{Path: cl, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, SessionKey: sess, Parser: claude.ParserName},
		{Path: cx, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name},
	}
	dv := devicesync.SourceSpec{Path: devin.ExportPath(filepath.Join(s.home, "sessions.db"), "devin-redact-2"), Agent: transcript.AgentDevin,
		StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat, Export: true}
	return specs, dv, []byte(export)
}

func (s *server) syncRedact(sy *devicesync.Syncer, specs []devicesync.SourceSpec, dv devicesync.SourceSpec, export []byte) {
	s.t.Helper()
	ctx := context.Background()
	for _, sp := range specs {
		if err := sy.Sync(ctx, sp); err != nil {
			s.t.Fatal(err)
		}
	}
	if err := sy.SyncExport(ctx, dv, export); err != nil {
		s.t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		s.t.Fatal(err)
	}
}

// archiveHas reports whether any archived byte (chunks and tails of every
// generation) contains needle.
func (s *server) archiveHas(needle string) bool {
	s.t.Helper()
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT source_id::text,generation FROM generations`)
	if err != nil {
		s.t.Fatal(err)
	}
	type sg struct {
		id  string
		gen int64
	}
	var all []sg
	for rows.Next() {
		var x sg
		rows.Scan(&x.id, &x.gen)
		all = append(all, x)
	}
	rows.Close()
	for _, x := range all {
		g, err := ingest.LoadGeneration(ctx, s.pool, x.id, x.gen)
		if err != nil {
			s.t.Fatal(err)
		}
		r := ingest.NewReader(ctx, s.objects, g)
		b, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
		if err != nil {
			s.t.Fatal(err)
		}
		if bytes.Contains(b, []byte(needle)) {
			var path string
			s.pool.QueryRow(ctx, `SELECT path FROM sources WHERE id=$1`, x.id).Scan(&path)
			i := bytes.Index(b, []byte(needle))
			var off, n *int64
			s.pool.QueryRow(ctx, `SELECT byte_offset,byte_len FROM messages WHERE source_id=$1 AND source_generation=$2 AND strpos(text,'first line')>0 LIMIT 1`, x.id, x.gen).Scan(&off, &n)
			if off != nil && n != nil {
				s.t.Logf("archive: %s gen %d at %d; row range %d+%d", filepath.Base(path), x.gen, i, *off, *n)
			} else {
				s.t.Logf("archive: %s gen %d at %d; row range nil", filepath.Base(path), x.gen, i)
			}
			return true
		}
	}
	return false
}

func (s *server) rowsWith(needle string) int {
	return s.count(`SELECT count(*) FROM messages WHERE strpos(text,$1)>0`, needle)
}

func (s *server) redact(c client.HTTP, path string, req format.RedactRequest) (format.RedactResult, error) {
	var res format.RedactResult
	err := c.JSON(context.Background(), "POST", path, req, &res)
	return res, err
}

// An owner redacts a line of a message with all its copies: the rows, the
// archived chunks (Claude, Codex and a Devin export, where the text sits
// in JSON inside a JSON string) and raw reads no longer hold it; the old
// chunks are purged; and a device that re-uploads the same bytes cannot
// bring it back.
func TestRedactMessageEverywhere(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	if n := s.rowsWith("BLUEFALCON"); n != 3 {
		t.Fatalf("%d rows hold the codename before, want 3", n)
	}
	var addr string
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&addr)

	// Another member may not redact it.
	other := s.member("mallory@example.test")
	if _, err := s.redact(other, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2"}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("another member: %v", err)
	}

	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2", AllCopies: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 3 || res.Chunks == 0 || res.JobID == "" {
		t.Fatalf("result %+v", res)
	}
	if n := s.rowsWith("BLUEFALCON"); n != 0 {
		t.Fatalf("%d rows still hold the codename", n)
	}
	if s.rowsWith("first line") != 3 || s.rowsWith("last line") != 3 {
		t.Fatal("lines outside the range were lost")
	}
	if s.archiveHas("BLUEFALCON") {
		t.Fatal("the archive still holds the codename")
	}
	raw, err := s.client.RawAt(ctx, addr)
	if err != nil || bytes.Contains(raw, []byte("BLUEFALCON")) || !bytes.Contains(raw, []byte("[REDACTED")) || json.Unmarshal(raw, new(any)) != nil {
		t.Fatalf("raw read after redaction: %v %q", err, raw)
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='message.redaction.requested'`); n != 1 {
		t.Fatalf("%d audit events", n)
	}

	// The purge worker deletes the old chunks.
	var oldKeys []string
	rows, _ := s.pool.Query(ctx, `SELECT object_key FROM chunks WHERE state='deletion_pending'`)
	for rows.Next() {
		var k string
		rows.Scan(&k)
		oldKeys = append(oldKeys, k)
	}
	rows.Close()
	if len(oldKeys) == 0 {
		t.Fatal("no old chunk queued for purge")
	}
	if n, err := s.store.ProcessDeletionJobs(ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	for _, k := range oldKeys {
		if _, err := s.objects.Get(ctx, k); err == nil {
			t.Fatalf("old chunk %s still in object storage", k)
		}
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='message.redaction.complete'`); n != 1 {
		t.Fatalf("%d purge audit events", n)
	}

	// A second device state (the same files, nothing known) re-uploads
	// every chunk: redirected chunks are not stored again.
	fresh := s.syncer()
	s.syncRedact(fresh, specs, dv, export)
	if s.rowsWith("BLUEFALCON") != 0 || s.archiveHas("BLUEFALCON") {
		t.Fatal("re-upload of the same chunks brought the codename back")
	}

	// The same line under new chunk boundaries (the file rewritten with a
	// new first line) is masked in rows and raw reads by its line hash.
	b, _ := os.ReadFile(specs[0].Path)
	pre := jline(map[string]any{"type": "summary", "summary": strings.Repeat("new head ", 40)})
	os.WriteFile(specs[0].Path+".new", append([]byte(pre), b...), 0o600)
	os.Rename(specs[0].Path+".new", specs[0].Path)
	s.syncRedact(fresh, specs[:1], dv, export)
	if n := s.rowsWith("BLUEFALCON"); n != 0 {
		t.Fatalf("%d rows hold the codename after a rewrite", n)
	}
	if s.archiveHas("BLUEFALCON") {
		t.Fatal("new chunk boundaries restored the redacted line in the stored archive")
	}
	var src string
	s.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE path=$1 ORDER BY first_seen_at DESC LIMIT 1`, specs[0].Path).Scan(&src)
	var gen, size int64
	s.pool.QueryRow(ctx, `SELECT generation,size FROM generations WHERE source_id=$1 ORDER BY generation DESC LIMIT 1`, src).Scan(&gen, &size)
	if all, err := s.client.Raw(ctx, src, gen, 0, size); err != nil || bytes.Contains(all, []byte("BLUEFALCON")) {
		t.Fatalf("raw read of the rewritten file: %v", err)
	}
}

// An admin may redact any member's whole message.
func TestAdminRedactsWholeMessage(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var id string
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='codex' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id)
	admin := s.admin("root@example.test")
	res, err := s.redact(admin, "/v1/admin/redactions", format.RedactRequest{Address: id})
	if err != nil || res.Messages != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	var text string
	s.pool.QueryRow(ctx, `SELECT text FROM messages WHERE id=$1`, id).Scan(&text)
	if !strings.HasPrefix(text, "[REDACTED:message]") || strings.Contains(text, "first line") {
		t.Fatalf("text %q", text)
	}
	if s.rowsWith("BLUEFALCON") != 2 {
		t.Fatal("copies were redacted without all_copies")
	}
	// A Devin row has no byte range: its record is found by native id.
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='devin' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id)
	if _, err := s.redact(admin, "/v1/admin/redactions", format.RedactRequest{Address: id}); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='devin' AND strpos(m.text,'filler 10 ')>0`); n != 1 {
		t.Fatalf("a neighbour (dr-10) was redacted too: %d", n)
	}
	if s.rowsWith("BLUEFALCON") != 1 {
		t.Fatal("devin row not redacted")
	}
	for range 2 {
		if n, err := s.store.ProcessDeletionJobs(ctx); err != nil || n != 1 {
			t.Fatalf("purge: %d %v", n, err)
		}
	}
	if s.archiveHas("first line\\\\ncodename") {
		t.Fatal("devin export still holds the redacted record")
	}
}

// member adds a user with a device credential and returns their client.
func (s *server) member(email string) client.HTTP {
	s.t.Helper()
	return s.addUser(email, "member", "device")
}

// admin adds an administrator with a session credential.
func (s *server) admin(email string) client.HTTP {
	s.t.Helper()
	return s.addUser(email, "admin", "session")
}

func (s *server) addUser(email, role, kind string) client.HTTP {
	ctx := context.Background()
	user, device := uuid.NewString(), uuid.NewString()
	plain, hash, _ := auth.NewToken()
	now := time.Now().UTC()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,'X',$3,'human',$4)`, []any{user, email, role, now}},
		{`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop-x','darwin',$3)`, []any{device, user, now}},
	} {
		if _, err := s.pool.Exec(ctx, q.sql, q.args...); err != nil {
			s.t.Fatal(err)
		}
	}
	dev := any(device)
	if kind == "session" {
		dev = nil
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,$4,$5,$6)`,
		uuid.NewString(), user, dev, kind, hash, now); err != nil {
		s.t.Fatal(err)
	}
	c := s.client
	c.Token = plain
	return c
}

// syncer is a second sync state for the same device: it knows no chunk
// and re-sends everything.
func (s *server) syncer() *devicesync.Syncer {
	s.t.Helper()
	st, err := devicesync.OpenStore(filepath.Join(s.t.TempDir(), "sync.db"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { st.Close() })
	spool, _ := devicesync.OpenSpool(filepath.Join(s.t.TempDir(), "spool"), 1<<30)
	sy, err := devicesync.NewSyncer(devicesync.Config{Chunk: devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}},
		st, spool, &syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: http.DefaultClient})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(sy.Close)
	return sy
}
