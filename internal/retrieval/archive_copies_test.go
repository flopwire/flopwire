package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/google/uuid"
)

// device adds another device of the server's member and returns its
// credential.
func (s *server) device(name string) client.HTTP {
	s.t.Helper()
	ctx := context.Background()
	device := uuid.NewString()
	plain, hash, _ := auth.NewToken()
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,$3,'darwin',$4)`, device, s.userID, name, now); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO credentials(id,user_id,device_id,kind,token_hash,created_at) VALUES($1,$2,$3,'device',$4,$5)`,
		uuid.NewString(), s.userID, device, hash, now); err != nil {
		s.t.Fatal(err)
	}
	c := s.client
	c.Token = plain
	return c
}

// The redacted-line catalog is keyed by the SHA-256 of the raw record
// line, globally: raw reads and later parses of any source mask a
// byte-identical line. Here the same session was archived from a second
// laptop (another device, so another conversation; one chunk, so no chunk
// is shared with the target) and parsed before a redaction without
// --all-copies. Its raw read is masked by the catalog, so its rows, its
// conversation and its stored archive bytes must be masked too.
func TestAtRestNonTargetByteCopyParsedBefore(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, _, _ := s.writeRedactFixtures()
	if err := s.sy.Sync(ctx, specs[0]); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	laptopB := s.device("laptop-b")
	s.rawUploadAs(laptopB, syncproto.Source{Path: "/w/laptop-b.jsonl", FileID: "copy:b", Agent: "claude", StorageKind: "jsonl_append",
		Parser: specs[0].Parser, SessionKey: specs[0].SessionKey}, 0, data)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(path string) string {
		t.Helper()
		var id string
		if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN sources src ON src.id=m.source_id
			WHERE src.path=$1 AND strpos(m.text,'first line')>0 AND NOT m.superseded`, path).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	target, other := row(specs[0].Path), row("/w/laptop-b.jsonl")
	if n := s.count(`SELECT count(DISTINCT conversation_id) FROM messages WHERE id=ANY($1::uuid[])`, []string{target, other}); n != 2 {
		t.Fatalf("the copy shares the target's conversation (%d conversations): it would be a target", n)
	}
	if s.rowsWith("BLUEFALCON") != 2 {
		t.Fatalf("%d rows hold the codename before", s.rowsWith("BLUEFALCON"))
	}

	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: target + ":2-2"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("redaction: %+v", res)

	// App reads of the copy.
	raw, err := s.client.RawAt(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	rawMasked := !bytes.Contains(raw, []byte("BLUEFALCON"))
	cx, err := s.client.Read(ctx, format.ReadQuery{Address: other}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	readJSON, _ := json.Marshal(cx)
	hits, err := find(t, s.client, "BLUEFALCON", false, true, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("non-target byte copy after redaction: raw read masked=%v, read masked=%v, grep hits=%d, rows holding the line=%d",
		rawMasked, !bytes.Contains(readJSON, []byte("BLUEFALCON")), len(hits), s.rowsWith("BLUEFALCON"))
	if !rawMasked {
		t.Fatal("the catalog no longer masks a raw read of a byte-identical copy")
	}
	if bytes.Contains(readJSON, []byte("BLUEFALCON")) || len(hits) != 0 {
		t.Errorf("read or grep serves the copy unmasked while its raw read is masked (%d grep hits)", len(hits))
	}
	if res.Messages != 2 {
		t.Errorf("redaction masked %d messages, want the target and its byte copy", res.Messages)
	}
	if n := s.count(`SELECT count(*) FROM conversations WHERE strpos(digest::text,'BLUEFALCON')>0 OR strpos(COALESCE(title,''),'BLUEFALCON')>0`); n != 0 {
		t.Errorf("%d conversations keep the line in a title or digest", n)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// A copy that is not byte-identical (another agent's record of the same
// text) is outside the catalog: without --all-copies its raw read and its
// rows both keep the text, so app reads and storage agree.
func TestNonTargetTextCopyStaysUnmasked(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var codexRow string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent='codex' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&codexRow); err != nil {
		t.Fatal(err)
	}
	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.hiddenMessage() + ":2-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 1 {
		t.Fatalf("redaction masked %d messages, want only the target", res.Messages)
	}
	raw, err := s.client.RawAt(ctx, codexRow)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("BLUEFALCON")) {
		t.Fatal("raw read of a non-identical copy is masked; its rows are not")
	}
	if n := s.rowsWith("BLUEFALCON"); n != 2 {
		t.Fatalf("%d rows hold the codename, want the Codex and Devin copies", n)
	}
}

// A byte-identical record whose parsed text differs: a Claude tool result
// whose output was persisted to tool-results/. Laptop A has the companion,
// so its row holds the full output; laptop B archived the same session
// file without it, so its row holds the preview from the same record line.
// The texts differ (another content_sha) while the record is the same
// bytes. After a redaction of A's row the catalog masks B's raw read, so
// B's rows, conversation and stored bytes must be masked too.
func TestAtRestNonTargetByteCopyOtherText(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	sess := "30000000-0000-4000-8000-0000000000a7"
	dir := filepath.Join(s.home, ".claude", "projects", "-w-pc")
	main := filepath.Join(dir, sess+".jsonl")
	comp := filepath.Join(dir, sess, "tool-results", "toolu_pc1.txt")
	os.MkdirAll(filepath.Dir(comp), 0o700)
	preview := "<persisted-output>\nOutput too large (40KB). Full output saved to: " + comp +
		"\n\nPreview (first 2KB):\nkey=BLUEFALCON-PREVIEW\n...\n</persisted-output>"
	data := jline(map[string]any{"type": "user", "uuid": "30000000-0000-4000-8000-0000000000b1", "timestamp": "2026-09-30T12:00:00.000Z",
		"sessionId": sess, "cwd": "/w/pc", "message": map[string]any{"role": "user", "content": "dump the config"}}) +
		jline(map[string]any{"type": "assistant", "uuid": "30000000-0000-4000-8000-0000000000b2", "parentUuid": "30000000-0000-4000-8000-0000000000b1",
			"timestamp": "2026-09-30T12:00:01.000Z", "sessionId": sess, "cwd": "/w/pc",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_pc1", "name": "Bash", "input": map[string]any{"command": "cat config"}}}}}) +
		jline(map[string]any{"type": "user", "uuid": "30000000-0000-4000-8000-0000000000b3", "parentUuid": "30000000-0000-4000-8000-0000000000b2",
			"timestamp": "2026-09-30T12:00:02.000Z", "sessionId": sess, "cwd": "/w/pc",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_pc1", "content": preview}}}})
	for i := range 60 {
		data += jline(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("30000000-0000-4000-8000-2%011d", i), "timestamp": "2026-09-30T12:00:03.000Z",
			"sessionId": sess, "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("filler %d %s", i, strings.Repeat("lorem ipsum ", 15))}}}})
	}
	if err := os.WriteFile(main, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comp, []byte("key=BLUEFALCON-PREVIEW\nrest of the full output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The companion first: A's row is parsed once, with the full output.
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: comp, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: main}); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: main, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, SessionKey: sess, Parser: claude.ParserName}); err != nil {
		t.Fatal(err)
	}
	laptopB := s.device("laptop-b")
	s.rawUploadAs(laptopB, syncproto.Source{Path: "/w/laptop-b/" + sess + ".jsonl", FileID: "copy:pc", Agent: "claude", StorageKind: "jsonl_append",
		Parser: claude.ParserName, SessionKey: sess}, 0, []byte(data))
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(path string) string {
		t.Helper()
		var id string
		if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN sources src ON src.id=m.source_id
			WHERE src.path=$1 AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`, path).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	target, other := row(main), row("/w/laptop-b/"+sess+".jsonl")
	if n := s.count(`SELECT count(DISTINCT content_sha) FROM messages WHERE id=ANY($1::uuid[])`, []string{target, other}); n != 2 {
		t.Fatalf("fixture: the two rows have the same text (%d distinct)", n)
	}
	if n := s.count(`SELECT count(*) FROM messages WHERE id=$1 AND version=1`, target); n != 1 {
		t.Fatalf("fixture: the target has another version")
	}

	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: target}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.client.RawAt(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("BLUEFALCON")) {
		t.Fatal("fixture: the catalog does not mask the copy's raw read")
	}
	cx, err := s.client.Read(ctx, format.ReadQuery{Address: other}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	readJSON, _ := json.Marshal(cx)
	hits, err := find(t, s.client, "BLUEFALCON", false, true, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(readJSON, []byte("BLUEFALCON")) || len(hits) != 0 {
		t.Errorf("read or grep serves the byte copy unmasked while its raw read is masked (%d grep hits, %d rows)", len(hits), s.rowsWith("BLUEFALCON"))
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// With --all-copies the Codex record of the same text is a target too, so
// its line enters the catalog. A byte copy of that rollout archived from
// another user's laptop (outside the owner's all_copies reach) has the
// Codex record's native id, not the addressed row's: it must be found and
// masked as well.
func TestAllCopiesByteCopyOfOtherCopy(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	data, err := os.ReadFile(specs[1].Path)
	if err != nil {
		t.Fatal(err)
	}
	bob := s.member("bob@example.test")
	s.rawUploadAs(bob, syncproto.Source{Path: "/w/laptop-b-rollout.jsonl", FileID: "copy:cx", Agent: "codex", StorageKind: "jsonl_append",
		Parser: specs[1].Parser}, 0, data)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var other string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.path='/w/laptop-b-rollout.jsonl' AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.hiddenMessage() + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.client.RawAt(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("BLUEFALCON")) {
		t.Fatal("fixture: the catalog does not mask the copy's raw read")
	}
	if n := s.rowsWith("BLUEFALCON"); n != 0 {
		t.Errorf("%d rows keep the line while the raw read of the byte copy is masked", n)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// sourceHas reports whether any stored generation of the source at path
// holds needle.
func (s *server) sourceHas(path, needle string) bool {
	s.t.Helper()
	ctx := context.Background()
	rows, err := s.pool.Query(ctx, `SELECT g.source_id::text,g.generation FROM generations g JOIN sources src ON src.id=g.source_id WHERE src.path=$1`, path)
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
		if err := rows.Scan(&x.id, &x.gen); err != nil {
			s.t.Fatal(err)
		}
		all = append(all, x)
	}
	rows.Close()
	if len(all) == 0 {
		s.t.Fatalf("no generation for %s", path)
	}
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
			return true
		}
	}
	return false
}

// rowsAt counts the rows of the source at path that hold needle.
func (s *server) rowsAt(path, needle string) int {
	return s.count(`SELECT count(*) FROM messages m JOIN sources src ON src.id=m.source_id WHERE src.path=$1 AND strpos(m.text,$2)>0`, path, needle)
}

// First uploader wins. Bob obtains the bytes of Gary's session (a raw read
// would do), uploads them after Gary did, and redacts his own row. The catalog masks raw reads of Gary's identical record as before,
// but Gary's rows and archive are not rewritten: Gary stored the record
// first. The result names Gary's source, and --admin can redact it.
func TestRedactForgedCopyLeavesFirstUploader(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, _, _ := s.writeRedactFixtures()
	if err := s.sy.Sync(ctx, specs[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	bob := s.member("bob@example.test")
	const forged = "/w/bob/forged.jsonl"
	s.rawUploadAs(bob, syncproto.Source{Path: forged, FileID: "copy:forged", Agent: "claude", StorageKind: "jsonl_append",
		Parser: specs[0].Parser, SessionKey: specs[0].SessionKey}, 0, data)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var bobRow string
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.path=$1 AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`, forged).Scan(&bobRow); err != nil {
		t.Fatal(err)
	}
	res, err := s.redact(bob, "/v1/redactions", format.RedactRequest{Address: bobRow + ":2-2"})
	if err != nil {
		t.Fatal(err)
	}
	s.drainRepairs(s.queue)
	s.purge()
	if n := s.rowsAt(specs[0].Path, "BLUEFALCON"); n != 1 {
		t.Errorf("Gary's rows holding the line: %d, want 1 (untouched)", n)
	}
	if !s.sourceHas(specs[0].Path, "BLUEFALCON") {
		t.Error("Gary's archive was rewritten by Bob's redaction")
	}
	if s.rowsAt(forged, "BLUEFALCON") != 0 || s.sourceHas(forged, "BLUEFALCON") {
		t.Error("Bob's own copy still holds the line")
	}
	if res.Messages != 1 || len(res.Skipped) != 1 || res.Skipped[0].User != "gary@example.test" || res.Skipped[0].Messages != 1 || res.Skipped[0].Device != "laptop-a" {
		t.Errorf("result %+v, want Bob's row redacted and Gary's source listed", res)
	}

	// An admin may still redact Gary's copy.
	if _, err := s.redact(s.admin("root@example.test"), "/v1/admin/redactions", format.RedactRequest{Address: s.hiddenMessageAt(specs[0].Path) + ":2-2"}); err != nil {
		t.Fatal(err)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// A teammate who archived a copy of Gary's session after Gary did does not
// keep the line when Gary redacts: Gary uploaded it first.
func TestRedactTeammateLaterCopy(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, _, _ := s.writeRedactFixtures()
	if err := s.sy.Sync(ctx, specs[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	s.rawUploadAs(s.member("bob@example.test"), syncproto.Source{Path: "/w/bob/copy.jsonl", FileID: "copy:bob", Agent: "claude", StorageKind: "jsonl_append",
		Parser: specs[0].Parser, SessionKey: specs[0].SessionKey}, 0, data)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.hiddenMessageAt(specs[0].Path) + ":2-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 2 || len(res.Skipped) != 0 {
		t.Fatalf("result %+v, want Gary's row and Bob's later copy", res)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// The caller's own copies always count, even one their other device
// uploaded first.
func TestRedactOwnEarlierDeviceCopy(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, _, _ := s.writeRedactFixtures()
	data, err := os.ReadFile(specs[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	s.rawUploadAs(s.device("laptop-b"), syncproto.Source{Path: "/w/laptop-b.jsonl", FileID: "copy:b", Agent: "claude", StorageKind: "jsonl_append",
		Parser: specs[0].Parser, SessionKey: specs[0].SessionKey}, 0, data)
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, specs[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.hiddenMessageAt(specs[0].Path) + ":2-2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 2 || len(res.Skipped) != 0 {
		t.Fatalf("result %+v, want both of Gary's devices", res)
	}
	s.drainRepairs(s.queue)
	s.requireNoSecretAtRest("BLUEFALCON")
}

// hiddenMessageAt is the live row of the source at path holding the codename.
func (s *server) hiddenMessageAt(path string) string {
	s.t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT m.id::text FROM messages m JOIN sources src ON src.id=m.source_id
		WHERE src.path=$1 AND strpos(m.text,'BLUEFALCON')>0 AND NOT m.superseded`, path).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}
