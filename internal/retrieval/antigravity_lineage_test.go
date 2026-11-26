package retrieval

import (
	"context"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestAntigravityLineageVisibility(t *testing.T) {
	s, _ := perfCorpus(t, 12, 1)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE conversations SET agent='antigravity' WHERE id<>md5('c7')::uuid`,
		`UPDATE conversations SET device_id=md5('d1')::uuid,user_id=md5('u1')::uuid
		   WHERE id IN (md5('c2')::uuid,md5('c3')::uuid,md5('c4')::uuid,md5('c5')::uuid,md5('c7')::uuid,md5('c11')::uuid,md5('c12')::uuid)`,
		`UPDATE conversations SET parent_native_session_id=md5('s1'),depth=1
		   WHERE id IN (md5('c2')::uuid,md5('c3')::uuid,md5('c6')::uuid,md5('c7')::uuid)`,
		`UPDATE conversations SET parent_conversation_id=md5('c1')::uuid
		   WHERE id IN (md5('c3')::uuid,md5('c6')::uuid)`,
		`UPDATE conversations SET parent_conversation_id=md5('c2')::uuid,parent_native_session_id=md5('s2'),depth=2
		   WHERE id=md5('c4')::uuid`,
		`UPDATE conversations SET parent_native_session_id=md5('s3'),depth=2 WHERE id=md5('c5')::uuid`,
		`UPDATE conversations SET hidden_at=now() WHERE id=md5('c3')::uuid`,
		`UPDATE conversations SET session_id=md5('s1') WHERE id=md5('c8')::uuid`,
		`UPDATE conversations SET device_id=md5('d8')::uuid,user_id=md5('u8')::uuid,parent_native_session_id=md5('s1'),depth=1
		   WHERE id=md5('c9')::uuid`,
		`UPDATE conversations SET parent_native_session_id=md5('missing'),depth=1 WHERE id=md5('c10')::uuid`,
		`UPDATE conversations SET parent_native_session_id=md5('s7'),depth=1 WHERE id IN (md5('c11')::uuid,md5('c12')::uuid)`,
		`UPDATE conversations SET parent_conversation_id=md5('c7')::uuid WHERE id=md5('c11')::uuid`,
	} {
		if _, err := s.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	var root string
	if err := s.Pool.QueryRow(ctx, `SELECT md5('s1')`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	wantParents := map[int]int{2: 1, 4: 2, 9: 8}
	sessions, err := s.Sessions(ctx, "", "", format.Filters{Agent: "antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]format.ConversationInfo)
	for _, got := range sessions.Sessions {
		byID[got.ID] = got
	}
	for _, n := range []int{1, 2, 4, 5, 6, 8, 9, 10, 11, 12} {
		id := sessionAt(t, s.Pool, n)
		got, ok := byID[id]
		if !ok {
			t.Fatalf("missing visible conversation %d", n)
		}
		var parent format.ConversationInfo
		if want := wantParents[n]; want != 0 {
			parent = byID[sessionAt(t, s.Pool, want)]
		}
		if got.ParentConversationID != parent.ID || got.ParentSession != parent.Address {
			t.Errorf("conversation %d has parent %q %q, want %q %q", n, got.ParentConversationID, got.ParentSession, parent.ID, parent.Address)
		}
	}

	ids := func(ns ...int) []string {
		var out []string
		for _, n := range ns {
			out = append(out, sessionAt(t, s.Pool, n))
		}
		slices.Sort(out)
		return out
	}
	remaining, err := s.Sessions(ctx, "", "", format.Filters{ExcludeSession: root})
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, c := range remaining.Sessions {
		listed = append(listed, c.ID)
	}
	slices.Sort(listed)
	if want := ids(5, 6, 7, 10, 11, 12); !slices.Equal(listed, want) {
		t.Errorf("remaining sessions = %v, want %v", listed, want)
	}

	for _, tc := range []struct {
		name   string
		filter format.Filters
		want   []string
	}{
		{"include", format.Filters{Session: root}, ids(1, 2, 4, 8, 9)},
		{"exclude", format.Filters{ExcludeSession: root}, ids(5, 6, 7, 10, 11, 12)},
		{"device", format.Filters{Session: root, Device: "laptop8"}, ids(8, 9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grep, err := s.Grep(ctx, format.GrepQuery{Pattern: "step", Fixed: true}, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			search, err := s.Search(ctx, format.SearchQuery{Query: "step"}, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			for name, page := range map[string]*format.Page{"grep": grep, "search": search} {
				var got []string
				for _, h := range page.Hits {
					got = append(got, h.ConversationID)
				}
				slices.Sort(got)
				if !slices.Equal(got, tc.want) {
					t.Errorf("%s conversations = %v, want %v", name, got, tc.want)
				}
			}
		})
	}
}
