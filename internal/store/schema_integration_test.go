package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type schemaFixture struct {
	pool                           *pgxpool.Pool
	user, device, source, conv     string
	otherUser, otherDevice, chunkA string
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPool(t, pgtest.NewDatabase(t))
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Fields(sql)[0]+" "+strings.Fields(sql)[2], err)
	}
}

func hash32(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func newSchemaFixture(t *testing.T) schemaFixture {
	f := schemaFixture{pool: migratedPool(t), user: uuid.NewString(), device: uuid.NewString(), source: uuid.NewString(), conv: uuid.NewString(), otherUser: uuid.NewString(), otherDevice: uuid.NewString()}
	now := time.Now().UTC()
	for _, u := range [][2]string{{f.user, "a@example.test"}, {f.otherUser, "b@example.test"}} {
		exec(t, f.pool, `INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,'n','member','human',$3)`, u[0], u[1], now)
	}
	exec(t, f.pool, `INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'mac','darwin',$3)`, f.device, f.user, now)
	exec(t, f.pool, `INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'box','linux',$3)`, f.otherDevice, f.otherUser, now)
	exec(t, f.pool, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'codex','/r.jsonl','1:2','jsonl_append','codex@1',$3)`, f.source, f.device, now)
	exec(t, f.pool, `INSERT INTO generations(source_id,generation,size,captured_at,complete) VALUES($1,0,150,$2,false)`, f.source, now)
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key,uploaded_by_device) VALUES($1,100,100,'chunks/a',$2)`, hash32("a"), f.device)
	exec(t, f.pool, `INSERT INTO manifest_entries(source_id,generation,ordinal,chunk_hash,byte_offset) VALUES($1,0,0,$2,0)`, f.source, hash32("a"))
	exec(t, f.pool, `INSERT INTO provisional_tails(source_id,generation,byte_offset,bytes,updated_at) VALUES($1,0,100,$2,$3)`, f.source, bytes.Repeat([]byte("x"), 50), now)
	exec(t, f.pool, `INSERT INTO chunks(hash,size,stored_size,object_key) VALUES($1,7,7,'chunks/unreferenced')`, hash32("b"))
	exec(t, f.pool, `INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id) VALUES($1,$2,'codex','sess-1',$3,$4)`, f.conv, f.source, f.device, f.user)
	return f
}

func (f schemaFixture) message(t *testing.T, native, text string, superseded bool, onPath *bool) string {
	t.Helper()
	id := uuid.NewString()
	exec(t, f.pool, `INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,text,text_len,content_sha,superseded,on_active_path,source_generation,parser,enrichment)
		VALUES($1,$2,$3,$4,$5,'assistant',$6,$7,$8,$9,$10,0,'codex@1','{"commands":[{"cmd":"go test","exit_code":1}]}')`,
		id, f.conv, f.source, native, time.Now().UnixNano(), text, len(text), hash32(text), superseded, onPath)
	return id
}

func TestSchemaMessagesSearchFiltersAndIntegrity(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	f.message(t, "m1", "The cobalt heron protocol retries twice", false, nil)
	f.message(t, "m2", "cobalt heron from an older rewrite", true, nil)
	f.message(t, "m3", "cobalt heron on an abandoned branch", false, func() *bool { b := false; return &b }())

	var ranked, defaults int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE tsv @@ websearch_to_tsquery('simple','cobalt heron')`).Scan(&ranked); err != nil || ranked != 3 {
		t.Fatalf("tsv hits=%d err=%v", ranked, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE tsv @@ websearch_to_tsquery('simple','cobalt heron') AND NOT superseded AND on_active_path IS NOT FALSE`).Scan(&defaults); err != nil || defaults != 1 {
		t.Fatalf("default-filter hits=%d err=%v", defaults, err)
	}
	var found string
	if err := f.pool.QueryRow(ctx, `SELECT native_id FROM messages WHERE text ILIKE '%heron protocol retr%'`).Scan(&found); err != nil || found != "m1" {
		t.Fatalf("trigram find=%q err=%v", found, err)
	}
	var err error

	// One live row per native record part; a superseded version may coexist.
	if _, err = f.pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,native_id,ordinal,kind,text,text_len,content_sha,source_generation,parser)
		VALUES($1,$2,'m1',9,'assistant','dup',3,$3,0,'codex@1')`, uuid.NewString(), f.conv, hash32("dup")); err == nil {
		t.Fatal("second live row for the same native id was accepted")
	}

	// A chunk that a manifest references cannot be removed.
	if _, err = f.pool.Exec(ctx, `DELETE FROM chunks WHERE hash=$1`, hash32("a")); err == nil {
		t.Fatal("referenced chunk row was deleted")
	}
	// Deletion-owned chunk states require an owning job, and only they may name one.
	if _, err = f.pool.Exec(ctx, `UPDATE chunks SET state='purging_delete' WHERE hash=$1`, hash32("b")); err == nil {
		t.Fatal("purging_delete without a deletion job was accepted")
	}

	// Deleting the conversation removes its message rows.
	exec(t, f.pool, `DELETE FROM conversations WHERE id=$1`, f.conv)
	var left int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("messages left=%d err=%v", left, err)
	}
}

func TestSchemaTombstoneIsUniquePerNaturalKey(t *testing.T) {
	f := newSchemaFixture(t)
	now := time.Now().UTC()
	exec(t, f.pool, `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at) VALUES($1,$4,$2,'codex','sess-1',$3,$4,$5)`, uuid.NewString(), f.device, f.conv, f.user, now)
	// Per user, whichever device: the second device's copy is the same session.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,requested_by,requested_at) VALUES($1,$4,$2,'codex','sess-1',$3,$4,$5)`, uuid.NewString(), f.otherDevice, uuid.NewString(), f.user, now); err == nil {
		t.Fatal("duplicate tombstone accepted")
	}
}

func TestPostgresUsageCountsChunksAndTailsPerUser(t *testing.T) {
	f := newSchemaFixture(t)
	p := NewPostgres(f.pool, nil, "")
	total, mine, err := p.Usage(context.Background(), f.user)
	if err != nil {
		t.Fatal(err)
	}
	// 100 (the user's device reserved it) + 7 (no uploader) + 50 (tail of
	// the user's source); the user is charged the first and the tail.
	if total != 157 || mine != 150 {
		t.Fatalf("total=%d mine=%d", total, mine)
	}
	_, other, err := p.Usage(context.Background(), f.otherUser)
	if err != nil || other != 0 {
		t.Fatalf("other=%d err=%v", other, err)
	}
	// V5: the counters follow every change: a tail that grows, an upload
	// taken over by another user's device, rows that go away.
	exec(t, f.pool, `UPDATE provisional_tails SET bytes=$2 WHERE source_id=$1`, f.source, bytes.Repeat([]byte("y"), 80))
	exec(t, f.pool, `UPDATE chunks SET uploaded_by_device=$2 WHERE hash=$1`, hash32("b"), f.otherDevice)
	if total, mine, _ = p.Usage(context.Background(), f.user); total != 187 || mine != 180 {
		t.Fatalf("after changes: total=%d mine=%d", total, mine)
	}
	if _, other, _ = p.Usage(context.Background(), f.otherUser); other != 7 {
		t.Fatalf("other after takeover=%d", other)
	}
	exec(t, f.pool, `DELETE FROM generations WHERE source_id=$1`, f.source) // cascades to the tail and manifest
	exec(t, f.pool, `DELETE FROM chunks WHERE hash=$1`, hash32("a"))
	if total, mine, _ = p.Usage(context.Background(), f.user); total != 7 || mine != 0 {
		t.Fatalf("after deletes: total=%d mine=%d", total, mine)
	}
}

// D14: one plain, uncapped text column, lz4 in TOAST with a low
// toast_tuple_target, feeding both the tsvector and the trigram index. A
// message far past the tsvector's 1MB limit still stores, and its end is
// findable.
func TestSchemaOneUncappedTextColumn(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	var cols []string
	rows, err := f.pool.Query(ctx, `SELECT a.attname||':'||format_type(a.atttypid,a.atttypmod)||':'||a.attcompression::text FROM pg_attribute a
		WHERE a.attrelid='messages'::regclass AND a.attnum>0 AND NOT a.attisdropped AND a.attname IN ('text','search_text')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) != 1 || cols[0] != "text:text:l" {
		t.Fatalf("text columns: %v", cols)
	}
	var opts []string
	if err := f.pool.QueryRow(ctx, `SELECT reloptions FROM pg_class WHERE oid='messages'::regclass`).Scan(&opts); err != nil || len(opts) != 1 || opts[0] != "toast_tuple_target=512" {
		t.Fatalf("reloptions %v %v", opts, err)
	}
	var words strings.Builder
	for i := 0; words.Len() < 3<<20; i++ {
		fmt.Fprintf(&words, "w%dx ", i)
	}
	big := words.String() + "zebra-tail-marker"
	f.message(t, "big", big, false, nil)
	var n, stored int
	if err := f.pool.QueryRow(ctx, `SELECT count(*),max(length(text)) FROM messages WHERE text ILIKE '%zebra-tail-marker%'`).Scan(&n, &stored); err != nil || n != 1 || stored != len(big) {
		t.Fatalf("uncapped find: %d rows, %d chars, %v", n, stored, err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE tsv @@ websearch_to_tsquery('simple','w1x')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tsv over a big text: %d %v", n, err)
	}
}
