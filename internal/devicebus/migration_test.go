package devicebus

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func populatedV4Inbox(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bus.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema, err := os.ReadFile("testdata/schema4.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	// Local and server rows retain delivery leases, receipts, bodies, sequence,
	// duplicate hashes and read state. Migration must not replay or discard them.
	if _, err := db.Exec(`INSERT INTO devbus_messages
 (id,origin,seq,to_session,to_agent,from_session,from_agent,thread_id,to_key,body_sha,envelope,state,reason,attempts,lease_until,created_at,expires_at,delivered_at,ack,listed,read_at,read_ack) VALUES
 ('local-queued','local',8,'recipient','claude','sender','codex','thread','recipient',X'010203','{"body":"synthetic local body"}','queued','',2,NULL,100,9999999999999,NULL,'',1,NULL,''),
 ('server-leased','server',9,'recipient','claude','remote','codex','thread','',NULL,'{"body":"synthetic leased body"}','leased','',3,9999999999000,101,9999999999999,NULL,'',1,102,'owed'),
 ('server-delivered','server',10,'recipient','claude','remote','codex','thread','',NULL,'{"body":"synthetic delivered body"}','delivered','',1,NULL,103,9999999999999,104,'owed',0,105,'done'),
 ('local-ended','local',11,'ended','claude','sender','codex','thread','ended',X'0405','{"body":"synthetic ended body"}','undelivered','session_ended',4,NULL,106,9999999999999,NULL,'report',1,NULL,'');
 INSERT INTO devbus_sessions VALUES('claude','ended','holder',10,11,12,13,'hook','holder');
 INSERT INTO devbus_instruct VALUES('recipient',NULL,9999999999000,3,14);
 INSERT INTO devbus_notices VALUES('sender-user',15);`); err != nil {
		t.Fatal(err)
	}
	return path
}

func inboxSnapshot(t *testing.T, db *sql.DB) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, table := range []string{"devbus_messages", "devbus_sessions", "devbus_instruct", "devbus_notices"} {
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			refs := make([]any, len(columns))
			for i := range refs {
				refs[i] = &values[i]
			}
			if err := rows.Scan(refs...); err != nil {
				t.Fatal(err)
			}
			for i, value := range values {
				if b, ok := value.([]byte); ok {
					values[i] = append([]byte(nil), b...)
				}
			}
			out[table] = append(out[table], values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return out
}

func TestInboxVersion4MigratesWithoutChangingDurableState(t *testing.T) {
	path := populatedV4Inbox(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	want := inboxSnapshot(t, db)
	db.Close()
	for i := 0; i < 2; i++ {
		st, err := openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(inboxSnapshot(t, st.db), want) {
			t.Fatal("migration/reopen changed durable inbox state")
		}
		var version, index, failures int
		if err := st.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='devbus_created'").Scan(&index); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRow("SELECT count(*) FROM devbus_failures").Scan(&failures); err != nil {
			t.Fatal(err)
		}
		if version != 5 || index != 1 || failures != 0 {
			t.Fatalf("migration metadata: version=%d index=%d failures=%d", version, index, failures)
		}
		st.db.Close()
	}
}

func TestInboxVersion4MigrationFailureIsAtomic(t *testing.T) {
	path := populatedV4Inbox(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	want := inboxSnapshot(t, db)
	// An incompatible additive table causes its index creation to fail after
	// the version-5 inbox index was created. Both must roll back together.
	if _, err := db.Exec("CREATE TABLE devbus_failures(id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err := openStore(path); err == nil {
		st.db.Close()
		t.Fatal("incompatible schema opened")
	}
	db, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !reflect.DeepEqual(inboxSnapshot(t, db), want) {
		t.Fatal("failed migration changed inbox state")
	}
	var version, index int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='devbus_created'").Scan(&index); err != nil {
		t.Fatal(err)
	}
	if version != 4 || index != 0 {
		t.Fatalf("failed migration partially committed: version=%d index=%d", version, index)
	}
}
