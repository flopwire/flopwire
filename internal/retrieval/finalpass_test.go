package retrieval_test

// Cross-feature tests from the final pass over integ/full: message
// redaction x zstd chunks x digests x hidden sessions x purge x quota x
// minted-token scopes. Each exercises a pair no single PR's tests saw.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/google/uuid"
)

// setRules stores admin path rules the way a policy update does.
func (s *server) setRules(rules ...string) {
	s.t.Helper()
	ctx := context.Background()
	var cred string
	if err := s.pool.QueryRow(ctx, `SELECT id::text FROM credentials WHERE user_id=$1 LIMIT 1`, s.userID).Scan(&cred); err != nil {
		s.t.Fatal(err)
	}
	if rules == nil {
		rules = []string{}
	}
	if err := s.store.UpdatePolicyWithAudit(ctx, cred, domain.Policy{PathRules: rules, UpdatedBy: s.userID},
		domain.AuditEvent{ID: uuid.NewString(), ActorID: s.userID, Action: "policy.update", TargetType: "policy", CreatedAt: time.Now().UTC()}); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.queue.EnforceRules(ctx); err != nil {
		s.t.Fatal(err)
	}
}

// redactFixturesSplit is writeRedactFixtures with the Codex rollout in
// another directory (/w/open), so a rule on /w/redact hides the Claude
// session and the Devin export but not the Codex copy.
func (s *server) redactFixturesSplit() {
	s.t.Helper()
	specs, dv, export := s.writeRedactFixtures()
	b, err := os.ReadFile(specs[1].Path)
	if err != nil {
		s.t.Fatal(err)
	}
	os.WriteFile(specs[1].Path, bytes.ReplaceAll(b, []byte(`"/w/redact"`), []byte(`"/w/open"`)), 0o600)
	s.syncRedact(s.sy, specs, dv, export)
}

func (s *server) codexCopy() string {
	var id string
	s.pool.QueryRow(context.Background(), `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.agent='codex' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id)
	if id == "" {
		s.t.Fatal("no codex copy")
	}
	return id
}

// Redaction x hidden sessions x zstd chunks x restore x purge: an owner's
// all-copies redaction from a visible copy masks the copies in sessions a
// rule change hid (rows, titles, digests and their compressed chunks); a
// restore brings them back masked; the rewritten chunks are zstd with a
// true stored size; and a later purge of the hidden sessions and the
// redaction's own purge both finish.
func TestFinalRedactHiddenCopiesRestorePurge(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.redactFixturesSplit()
	s.setRules("/w/redact")
	if n := s.count(`SELECT count(*) FROM conversations WHERE hidden_at IS NOT NULL`); n != 2 {
		t.Fatalf("%d hidden conversations, want 2 (claude, devin)", n)
	}
	res, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.codexCopy() + ":2-2", AllCopies: true})
	if err != nil || res.Messages != 3 {
		t.Fatalf("redaction: %+v %v", res, err)
	}
	if n := s.rowsWith("BLUEFALCON"); n != 0 {
		t.Fatalf("%d rows (hidden included) hold the codename", n)
	}
	if n := s.count(`SELECT count(*) FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE strpos(COALESCE(a.digest::text,''),'BLUEFALCON')>0 OR strpos(COALESCE(c.title,''),'BLUEFALCON')>0`); n != 0 {
		t.Fatalf("%d titles or digests hold the codename", n)
	}
	if s.archiveHas("BLUEFALCON") {
		t.Fatal("archive still holds the codename")
	}
	// Every live chunk object is a zstd frame of its recorded stored size.
	rows, _ := s.pool.Query(ctx, `SELECT hash,size,object_key,stored_size,encoding FROM chunks WHERE state='committed'`)
	type ch struct {
		hash       []byte
		size       int64
		key, enc   string
		storedSize int
	}
	var all []ch
	for rows.Next() {
		var c ch
		rows.Scan(&c.hash, &c.size, &c.key, &c.storedSize, &c.enc)
		all = append(all, c)
	}
	rows.Close()
	for _, c := range all {
		z, err := s.objects.Get(ctx, c.key)
		if err != nil {
			t.Fatal(err)
		}
		if len(z) != c.storedSize || c.enc != syncproto.Encoding {
			t.Fatalf("chunk %s: object %d bytes, stored_size %d, encoding %s", c.key, len(z), c.storedSize, c.enc)
		}
		if _, err := ingest.GetChunk(ctx, s.objects, ingest.Chunk{Hash: syncproto.Hash(c.hash), Size: c.size, Key: c.key}); err != nil {
			t.Fatalf("chunk %s does not decode: %v", c.key, err)
		}
	}
	// Restore: the rule is removed; the sessions come back masked.
	s.setRules()
	if n := s.count(`SELECT count(*) FROM conversations WHERE hidden_at IS NOT NULL`); n != 0 {
		t.Fatalf("%d still hidden after the rule was removed", n)
	}
	if s.rowsWith("BLUEFALCON") != 0 {
		t.Fatal("restore brought the codename back")
	}
	// Hide again and purge; then run the queued jobs (redaction purge and
	// the hidden purge) in one pass.
	s.setRules("/w/redact")
	purged, _, err := s.queue.PurgeHidden(ctx, s.userID, "", "")
	if err != nil || purged != 2 {
		t.Fatalf("purge hidden: %d %v", purged, err)
	}
	for range 3 {
		if _, err := s.store.ProcessDeletionJobs(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count(`SELECT count(*) FROM deletion_jobs WHERE state<>'complete'`); n != 0 {
		t.Fatalf("%d deletion jobs not completed", n)
	}
	if n := s.count(`SELECT count(*) FROM conversations WHERE agent IN ('claude','devin')`); n != 0 {
		t.Fatalf("%d purged conversations remain", n)
	}
	if n := s.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='codex'`); n == 0 {
		t.Fatal("the purge took the visible codex session")
	}
}

// Redaction x quota: the rewritten chunk is charged to the device that
// uploaded the chunk it replaces, so a redaction does not move a user's
// stored bytes out of their quota.
func TestFinalRedactionKeepsQuotaOwner(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	usage := func() int64 {
		var n int64
		s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT bytes FROM storage_usage WHERE owner=$1),0)`, s.userID).Scan(&n)
		return n
	}
	before := usage()
	if before == 0 {
		t.Fatal("no usage recorded")
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: s.codexCopy() + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM chunks WHERE uploaded_by_device IS NULL`); n != 0 {
		t.Fatalf("%d chunks charged to nobody", n)
	}
	after := usage()
	// Masks keep lengths; compressed sizes move a little.
	if after < before*9/10 {
		t.Fatalf("user usage %d -> %d after a redaction", before, after)
	}
}

// Redaction x digest x rewrite x re-parse: after the first prompt's first
// line (the title) is redacted, a rewrite of the transcript (new chunk
// boundaries, a new generation) and a full re-parse rebuild the rows,
// title and digest from bytes the device re-uploads unmasked; none of them
// may bring the line back.
func TestFinalDigestTitleAfterRedactionRewriteReparse(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var addr string
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&addr)
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":1-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	leaks := func(when string) {
		t.Helper()
		for _, needle := range []string{"BLUEFALCON", "first line"} {
			if n := s.rowsWith(needle); n != 0 {
				t.Fatalf("%s: %d rows hold %q", when, n, needle)
			}
			if n := s.count(`SELECT count(*) FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE strpos(COALESCE(a.digest::text,''),$1)>0 OR strpos(COALESCE(c.title,''),$1)>0`, needle); n != 0 {
				t.Fatalf("%s: %d titles or digests hold %q", when, n, needle)
			}
		}
	}
	leaks("after redaction")
	fresh := s.syncer()
	b, _ := os.ReadFile(specs[0].Path)
	pre := jline(map[string]any{"type": "summary", "summary": strings.Repeat("new head ", 40)})
	os.WriteFile(specs[0].Path+".new", append([]byte(pre), b...), 0o600)
	os.Rename(specs[0].Path+".new", specs[0].Path)
	s.syncRedact(fresh, specs[:1], dv, export)
	leaks("after rewrite")
	// A full re-parse of every source (what a parser change or a rule
	// change's recheck requests: requestParse with reparse set).
	if _, err := s.pool.Exec(ctx, `UPDATE source_parse_state SET reparse=true,requested_seq=requested_seq+1,requested_at=now()`); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	leaks("after re-parse")
}

// Minted tokens x destructive routes: a read-only token minted from a
// member's device or an admin's login session reads, but cannot redact,
// withhold, delete, purge hidden sessions or mint; each refusal is
// audited.
func TestFinalMintedReadTokenCannotMutate(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	mint := func(from client.HTTP) client.HTTP {
		var out struct{ Token string }
		if err := from.JSON(ctx, "POST", "/v1/tokens", map[string]any{"scopes": []string{"read"}, "label": "sandbox"}, &out); err != nil || out.Token == "" {
			t.Fatalf("mint: %v", err)
		}
		c := from
		c.Token = out.Token
		return c
	}
	member := mint(s.client)
	admin := mint(s.admin("root@example.test"))
	addr := s.codexCopy()
	var conv string
	s.pool.QueryRow(ctx, `SELECT conversation_id::text FROM messages WHERE id=$1`, addr).Scan(&conv)
	if _, err := find(t, member, "BLUEFALCON", false, false, format.Filters{}); err != nil {
		t.Fatalf("a read token cannot read: %v", err)
	}
	before := s.count(`SELECT count(*) FROM audit_events WHERE action='authorization.failed'`)
	for _, c := range []struct {
		name         string
		c            client.HTTP
		method, path string
		body         any
	}{
		{"member redact", member, "POST", "/v1/redactions", format.RedactRequest{Address: addr, AllCopies: true}},
		{"member withhold", member, "POST", "/v1/conversations/withhold", map[string]any{"agent": "codex", "session_id": "30000000-0000-4000-8000-0000000000c1", "rule": "x"}},
		{"member delete", member, "DELETE", "/v1/conversations/" + conv, nil},
		{"member mint", member, "POST", "/v1/tokens", map[string]any{"scopes": []string{"read"}, "label": "again"}},
		{"admin redact", admin, "POST", "/v1/admin/redactions", format.RedactRequest{Address: addr}},
		{"admin purge", admin, "POST", "/v1/admin/policy/hidden/purge", map[string]any{"confirm": true}},
		{"admin delete", admin, "DELETE", "/v1/admin/conversations/" + conv, nil},
		{"admin policy", admin, "PUT", "/v1/admin/policy", map[string]any{"path_rules": []string{"/"}}},
	} {
		err := c.c.JSON(ctx, c.method, c.path, c.body, nil)
		var ae *client.APIError
		if err == nil || !errors.As(err, &ae) || ae.StatusCode != 403 {
			t.Errorf("%s: %v, want 403", c.name, err)
		}
	}
	if s.rowsWith("BLUEFALCON") != 3 || s.count(`SELECT count(*) FROM conversations`) != 3 {
		t.Fatal("a refused request changed stored data")
	}
	if n := s.count(`SELECT count(*) FROM audit_events WHERE action='authorization.failed'`) - before; n != 8 {
		t.Fatalf("%d refusals audited, want 8", n)
	}
}

// Strict placement x hidden sessions: a device withholding a session an
// admin rule already hid deletes it (a later rule removal must not
// restore what the device withheld).
func TestFinalWithholdHiddenSession(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	s.redactFixturesSplit()
	s.setRules("/w/redact")
	const sess = "30000000-0000-4000-8000-000000000010"
	if n := s.count(`SELECT count(*) FROM conversations WHERE session_id=$1 AND hidden_at IS NOT NULL`, sess); n != 1 {
		t.Fatalf("claude session hidden: %d", n)
	}
	if err := s.client.JSON(ctx, "POST", "/v1/conversations/withhold", map[string]any{"agent": "claude", "session_id": sess, "mode": "deny", "rule": "deny /w/redact"}, nil); err != nil {
		t.Fatalf("withhold: %v", err)
	}
	if _, err := s.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	s.setRules()
	if n := s.count(`SELECT count(*) FROM conversations WHERE session_id=$1`, sess); n != 0 {
		t.Fatalf("the withheld session came back on restore: %d", n)
	}
	if n := s.count(`SELECT count(*) FROM conversations WHERE agent='devin' AND hidden_at IS NULL`); n != 1 {
		t.Fatalf("the other hidden session was not restored: %d", n)
	}
}
