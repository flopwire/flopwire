package retrieval

import (
	"context"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestSharedRecordedRepositoryLabels(t *testing.T) {
	s, _ := perfCorpus(t, 2, 2)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, `UPDATE conversations SET repo_root='/tmp/app-ux-audit' WHERE id=md5('c1')::uuid`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE sources SET remote='github.com/other/unrelated' WHERE id=md5('f2')::uuid`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET source_id=md5('f2')::uuid WHERE conversation_id=md5('c1')::uuid`); err != nil {
		t.Fatal(err)
	}
	var session, derivedSource string
	if err := s.Pool.QueryRow(ctx, `SELECT session_id,md5('f2')::uuid::text FROM conversations WHERE id=md5('c1')::uuid`).Scan(&session, &derivedSource); err != nil {
		t.Fatal(err)
	}
	f := format.Filters{Session: session}
	for _, tc := range []struct{ remote, main, identity string }{
		{"github.com/acme/app", "/src/.app.git", "github.com/acme/app"},
		{"github.com/other/app", "/src/.app.git", "github.com/other/app"},
		{"", "/src/.app.git", "/src/.app.git"},
		{"", "", ""},
	} {
		if _, err := s.Pool.Exec(ctx, `UPDATE sources SET remote=$1,checkout=$2 WHERE id=md5('f1')::uuid`, tc.remote, tc.main); err != nil {
			t.Fatal(err)
		}
		worktree := "/tmp/app-ux-audit"
		if tc.identity == "" {
			worktree = ""
		}
		check := func(c format.ConversationInfo) {
			t.Helper()
			if c.RepoIdentity != tc.identity || c.Worktree != worktree || c.Repo != "/tmp/app-ux-audit" || c.SessionID != session {
				t.Fatalf("conversation labels: %+v", c)
			}
		}
		list, err := s.Sessions(ctx, session, "", f)
		if err != nil || len(list.Sessions) != 1 {
			t.Fatalf("sessions: %+v %v", list, err)
		}
		check(list.Sessions[0])
		cx, err := s.Read(ctx, "", format.ReadQuery{Address: session}, f)
		if err != nil {
			t.Fatal(err)
		}
		check(cx.Conversation)
		g, err := s.Grep(ctx, format.GrepQuery{Pattern: "step", Fixed: true}, f)
		if err != nil {
			t.Fatal(err)
		}
		q, err := s.Search(ctx, format.SearchQuery{Query: "step"}, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, page := range []*format.Page{g, q} {
			if len(page.Hits) != 2 {
				t.Fatalf("hits: %+v", page)
			}
			for _, h := range page.Hits {
				if h.RepoIdentity != tc.identity || h.Worktree != worktree || h.Repo != "/tmp/app-ux-audit" || !strings.HasPrefix(h.Address, session[:8]) {
					t.Fatalf("hit labels or address: %+v", h)
				}
				if h.Provenance.SourceID != derivedSource {
					t.Fatalf("derived provenance changed: %+v", h.Provenance)
				}
			}
		}
	}
}
