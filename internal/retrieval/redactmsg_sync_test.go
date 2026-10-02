package retrieval_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// The conversation title is the first line of the first prompt: redacting
// that line masks the title too.
func TestRedactMasksTitle(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var addr string
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&addr)
	before := s.count(`SELECT count(*) FROM conversations WHERE strpos(title,'first line')>0`)
	t.Logf("titles holding line 1 before: %d", before)
	if before == 0 {
		t.Fatal("fixture title does not come from the prompt")
	}
	if n := s.count(`SELECT count(*) FROM conversation_activity WHERE strpos(digest::text,'first line')>0`); n == 0 {
		t.Fatal("fixture digest does not hold the prompt")
	}
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":1-1", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if s.rowsWith("first line") != 0 {
		t.Fatal("rows not masked")
	}
	if n := s.count(`SELECT count(*) FROM conversations WHERE strpos(title,'first line')>0`); n != 0 {
		t.Fatalf("%d conversation titles still hold the redacted line", n)
	}
	if n := s.count(`SELECT count(*) FROM conversation_activity WHERE strpos(digest::text,'first line')>0`); n != 0 {
		t.Fatalf("%d digests still hold the redacted line", n)
	}
}

// Another user's device holding the bytes of a rewritten chunk still
// syncs: its verified old body proves possession of the new chunk.
func TestRedactedChunkOtherUserSyncs(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	var addr string
	s.pool.QueryRow(ctx, `SELECT m.id::text FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&addr)
	if _, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: addr + ":2-2", AllCopies: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.ProcessDeletionJobs(ctx); err != nil {
		t.Fatal(err)
	}
	bob := s.member("bob@example.test")
	sy := s.syncerFor(bob)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := sy.Sync(cctx, specs[0]); err != nil {
		t.Fatalf("other user's sync: %v", err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN users u ON u.id=c.user_id WHERE u.email='bob@example.test'`); n == 0 {
		t.Fatal("bob's upload stored no rows")
	}
}

func (s *server) syncerFor(c client.HTTP) *devicesync.Syncer {
	s.t.Helper()
	st, err := devicesync.OpenStore(filepath.Join(s.t.TempDir(), "sync.db"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { st.Close() })
	spool, _ := devicesync.OpenSpool(filepath.Join(s.t.TempDir(), "spool"), 1<<30)
	sy, err := devicesync.NewSyncer(devicesync.Config{Chunk: devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}},
		st, spool, &syncproto.Client{Server: c.Server, Token: c.Token, HTTP: http.DefaultClient})
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(sy.Close)
	return sy
}
