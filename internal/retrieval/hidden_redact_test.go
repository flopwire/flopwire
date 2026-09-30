package retrieval_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/flopwire/flopwire/internal/ingest"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestRedactHiddenAddress(t *testing.T) {
	for _, actor := range []string{"owner", "admin"} {
		for _, kind := range []string{"uuid", "session", "ordinal", "prefix", "path"} {
			t.Run(actor+"/"+kind, func(t *testing.T) {
				s := newServer(t)
				specs, dv, export := s.writeRedactFixtures()
				s.syncRedact(s.sy, specs, dv, export)
				s.setRules("/w/redact")
				var id, session, path string
				var ordinal, line int
				err := s.pool.QueryRow(context.Background(), `SELECT m.id::text,c.session_id,m.ordinal,src.path,m.line_no FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN sources src ON src.id=m.source_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id, &session, &ordinal, &path, &line)
				if err != nil {
					t.Fatal(err)
				}
				address := id
				switch kind {
				case "session":
					address = session
				case "ordinal":
					address = fmt.Sprintf("%s/%d", session, ordinal)
				case "prefix":
					address = session[:35]
				case "path":
					address = fmt.Sprintf("%s:%d", path, line)
				}
				outsider := s.member("outsider@example.com")
				// A foreign hidden session sharing the prefix must not become
				// an ambiguity candidate for the owner.
				if actor == "owner" && kind == "prefix" {
					_, err := s.pool.Exec(context.Background(), `INSERT INTO conversations(id,agent,session_id,device_id,user_id,hidden_at) SELECT gen_random_uuid(),'claude',$1,d.id,d.user_id,now() FROM devices d JOIN users u ON u.id=d.user_id WHERE u.email='outsider@example.com'`, session[:35]+"2")
					if err != nil {
						t.Fatal(err)
					}
				}
				for _, c := range []client.HTTP{s.client, outsider} {
					_, err := c.Read(context.Background(), format.ReadQuery{Address: address}, format.Filters{})
					var apiErr *client.APIError
					if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
						t.Fatalf("hidden read: %v", err)
					}
				}
				_, err = s.redact(outsider, "/v1/redactions", format.RedactRequest{Address: address})
				var apiErr *client.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
					t.Fatalf("foreign hidden redaction: %v", err)
				}
				c := s.client
				endpoint := "/v1/redactions"
				if actor == "admin" {
					c = s.admin("admin@example.com")
					endpoint = "/v1/admin/redactions"
				}
				res, err := s.redact(c, endpoint, format.RedactRequest{Address: address, AllCopies: true})
				if err != nil || res.Messages != 3 {
					t.Fatalf("authorized hidden redaction: %+v %v", res, err)
				}
				if s.rowsWith("BLUEFALCON") != 0 || s.archiveHas("BLUEFALCON") {
					t.Fatal("redaction left secret in rows/archive")
				}
				if _, err := s.client.Read(context.Background(), format.ReadQuery{Address: id}, format.Filters{}); !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
					t.Fatalf("redaction exposed hidden session: %v", err)
				}
				s.setRules()
				result, err := s.client.Read(context.Background(), format.ReadQuery{Address: id}, format.Filters{})
				if err != nil || result == nil {
					t.Fatalf("restored read: %v", err)
				}
				if s.rowsWith("BLUEFALCON") != 0 {
					t.Fatal("restore recovered redacted text")
				}
			})
		}
	}
}

// A line-range redaction inside a hidden session changes just that copy.
func TestRedactHiddenLine(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	specs, dv, export := s.writeRedactFixtures()
	s.syncRedact(s.sy, specs, dv, export)
	s.setRules("/w/redact")
	var id, source string
	var generation int64
	if err := s.pool.QueryRow(ctx, `SELECT m.id::text,m.source_id::text,m.source_generation FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='claude' AND strpos(m.text,'BLUEFALCON')>0`).Scan(&id, &source, &generation); err != nil {
		t.Fatal(err)
	}
	result, err := s.redact(s.client, "/v1/redactions", format.RedactRequest{Address: id + ":2-2"})
	if err != nil || result.Messages != 1 {
		t.Fatalf("hidden line redaction: %+v %v", result, err)
	}
	var text string
	if err := s.pool.QueryRow(ctx, `SELECT text FROM messages WHERE id=$1`, id).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, "first line\n") || !strings.HasSuffix(text, "\nlast line") || strings.Contains(text, "BLUEFALCON") {
		t.Fatalf("neighboring lines or mask incorrect: %q", text)
	}
	if s.rowsWith("BLUEFALCON") != 2 {
		t.Fatal("redacted other copies without all_copies")
	}
	g, err := ingest.LoadGeneration(ctx, s.pool, source, generation)
	if err != nil {
		t.Fatal(err)
	}
	r := ingest.NewReader(ctx, s.objects, g)
	raw, err := io.ReadAll(io.NewSectionReader(r, 0, r.Size()))
	if err != nil || strings.Contains(string(raw), "BLUEFALCON") {
		t.Fatalf("hidden archive still holds secret: %v", err)
	}
}
