package localindex

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// Walking ListConversations by keyset returns every conversation once, in
// order, both ways: ties on last activity broken by id, undated ones last.
func TestListConversationsKeysetWalk(t *testing.T) {
	s := openTest(t, DetailColumn)
	ctx := context.Background()
	for i := 1; i <= 23; i++ {
		var at any = int64(1_788_000_000_000 + (i/3)*1000)
		if i%5 == 0 {
			at = nil
		}
		if _, err := s.wdb.Exec(`INSERT INTO conversations(id,agent,session_id,device_id,last_activity_at) VALUES(?,'claude',?,'d',?)`,
			i, "s"+string(rune('a'+i)), at); err != nil {
			t.Fatal(err)
		}
	}
	for _, oldest := range []bool{false, true} {
		var all []int64
		rows, _ := s.ListConversations(ctx, ListOptions{Limit: 100, Oldest: oldest})
		for _, r := range rows {
			all = append(all, r.ID)
		}
		if len(all) != 23 {
			t.Fatalf("listed %d", len(all))
		}
		for _, limit := range []int{1, 4} {
			var got []int64
			o := ListOptions{Limit: limit, Oldest: oldest}
			for range 50 {
				rows, err := s.ListConversations(ctx, o)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					got = append(got, r.ID)
				}
				if len(rows) < limit {
					break
				}
				last := rows[len(rows)-1]
				var at sql.NullInt64
				if err := s.rdb.QueryRow(`SELECT last_activity_at FROM conversations WHERE id=?`, last.ID).Scan(&at); err != nil {
					t.Fatal(err)
				}
				o.After = &ListKey{At: at.Int64, Undated: !at.Valid, ID: last.ID}
			}
			if !slices.Equal(got, all) {
				t.Fatalf("oldest=%v limit %d: walk %v, want %v", oldest, limit, got, all)
			}
		}
	}
}
