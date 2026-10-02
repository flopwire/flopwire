package ingest

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// conversation_activity.hidden follows conversations.hidden_at on insert
// and on every update that hides or restores, and a write that leaves
// hidden_at's nullness be (a hide's later time, a cold-column change, a
// no-op) leaves the activity row unwritten.
func TestActivityMirrorsHidden(t *testing.T) {
	e := newEnv(t)
	src := uuid.NewString()
	e.exec(`INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude','/p.jsonl','f','jsonl_append','test',now())`, src, e.deviceID)
	insert := func(hiddenAt *time.Time) string {
		id := uuid.NewString()
		e.exec(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,hidden_at,hidden_root) VALUES($1::uuid,$2,'claude',$1::text,$3,$4,$5,CASE WHEN $5::timestamptz IS NOT NULL THEN $1::uuid END)`,
			id, src, e.deviceID, e.userID, hiddenAt)
		return id
	}
	type state struct {
		hidden bool
		xmin   string
	}
	activity := func(id string) state {
		t.Helper()
		var s state
		if err := e.pool.QueryRow(e.ctx, `SELECT hidden,xmin::text FROM conversation_activity WHERE conversation_id=$1`, id).Scan(&s.hidden, &s.xmin); err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Now().UTC()
	visible, hidden := insert(nil), insert(&now)
	if activity(visible).hidden || !activity(hidden).hidden {
		t.Fatalf("inserted: visible %+v, hidden %+v", activity(visible), activity(hidden))
	}

	before := activity(visible)
	e.exec(`UPDATE conversations SET hidden_at=now(),hidden_root=id WHERE id=$1`, visible)
	if a := activity(visible); !a.hidden || a.xmin == before.xmin {
		t.Fatalf("hide: %+v (was %+v)", a, before)
	}
	for _, q := range []string{
		`UPDATE conversations SET hidden_at=hidden_at+interval '1 second' WHERE id=$1`,
		`UPDATE conversations SET title='renamed' WHERE id=$1`,
		`UPDATE conversations SET title=title WHERE id=$1`,
	} {
		before := activity(visible)
		e.exec(q, visible)
		if a := activity(visible); a != before {
			t.Fatalf("%s: activity row %+v, was %+v", q, a, before)
		}
	}
	e.exec(`UPDATE conversations SET hidden_at=NULL,hidden_root=NULL WHERE id=$1`, visible)
	if activity(visible).hidden {
		t.Fatal("restore left the activity row hidden")
	}
	// One statement over both, as a hide of a tree does.
	e.exec(`UPDATE conversations SET hidden_at=CASE WHEN hidden_at IS NULL THEN now() END WHERE id IN ($1,$2)`, visible, hidden)
	if !activity(visible).hidden || activity(hidden).hidden {
		t.Fatalf("swap: visible %+v, hidden %+v", activity(visible), activity(hidden))
	}
}
