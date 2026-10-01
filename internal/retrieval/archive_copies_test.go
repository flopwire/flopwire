package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
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
