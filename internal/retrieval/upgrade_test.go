package retrieval_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/redact/redacttest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// This constructs the schema and ledger of a pre-versioned deployment. The
// upgrade itself uses the production migration runner, not this fixture loader.
func preReparseSchema(ctx context.Context, pool *pgxpool.Pool) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE flopwire_schema_migrations(name text PRIMARY KEY,checksum text NOT NULL,applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		entries, err := migrations.Files.ReadDir(".")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() >= "008_reparse.sql" {
				continue
			}
			raw, err := migrations.Files.ReadFile(entry.Name())
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, string(raw)); err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			if _, err := tx.Exec(ctx, `INSERT INTO flopwire_schema_migrations(name,checksum) VALUES($1,$2)`, entry.Name(), hex.EncodeToString(sum[:])); err != nil {
				return err
			}
		}
		return nil
	})
}

func TestPopulatedArchiveReparseUpgrade(t *testing.T) {
	ctx := context.Background()
	s := newServerMigrating(t, devicesync.ChunkParams{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, nil, preReparseSchema)
	if s.count(`SELECT count(*) FROM flopwire_schema_migrations`) != 7 {
		t.Fatal("fixture is not pre-008")
	}
	secret := redacttest.Needles()["GITHUB_PAT"]
	sess, native, conv, live, history := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	raw := []byte(jline(map[string]any{"type": "user", "uuid": native, "sessionId": sess, "timestamp": "2026-09-30T12:00:00Z", "cwd": "/w/upgrade", "message": map[string]any{"role": "user", "content": "upgrade fixture " + secret}}))
	body, z := syncproto.EncodeBody(raw)
	hash := syncproto.Sum(raw)
	h := syncproto.FlushHeader{Version: syncproto.Version, Generation: 0, CapturedAt: time.Now().UTC(), Source: syncproto.Source{Path: "/historical/" + sess + ".jsonl", FileID: "old-device-file", Agent: "claude", StorageKind: "jsonl_append", Parser: "claude@2.0"}, Chunker: syncproto.ChunkerParams{Algorithm: devicesync.Algorithm, Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}, Entries: []syncproto.Entry{{Ordinal: 0, Hash: hash, Offset: 0, Size: int64(len(raw))}}, Bodies: []syncproto.Body{body}}
	upload := syncproto.Client{Server: s.client.Server, Token: s.client.Token, HTTP: &http.Client{Timeout: 10 * time.Second}}
	if _, err := upload.Flush(ctx, &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(z)}); err != nil {
		t.Fatal(err)
	}
	var source, device string
	if err := s.pool.QueryRow(ctx, `SELECT id::text,device_id::text FROM sources WHERE path=$1`, h.Source.Path).Scan(&source, &device); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	tailNative := uuid.NewString()
	tail := []byte(jline(map[string]any{"type": "assistant", "uuid": tailNative, "sessionId": sess, "timestamp": "2026-09-30T12:00:01Z", "message": map[string]any{"role": "assistant", "content": "tail fixture " + secret}}))
	exec(`INSERT INTO provisional_tails(source_id,generation,byte_offset,bytes,updated_at) VALUES($1,0,$2,$3,now())`, source, len(raw), tail)
	exec(`UPDATE source_parse_state SET generation=0,cursor_offset=$2,cursor_line=1,parsed_seq=requested_seq,parsed_at=now() WHERE source_id=$1`, source, len(raw))
	exec(`INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd,title) VALUES($1,$2,'claude',$3,$4,$5,'/w/upgrade',$6)`, conv, source, sess, device, s.userID, "old title "+secret)
	exec(`UPDATE conversation_activity SET digest=$2 WHERE conversation_id=$1`, conv, map[string]any{"summary": secret})
	for i, id := range []string{history, live} {
		text := fmt.Sprintf("old version %d %s", i, secret)
		sum := sha256.Sum256([]byte(text))
		exec(`INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,role,text,text_len,content_sha,version,superseded,enrichment,source_generation,line_no,byte_offset,byte_len,locator,parser) VALUES($1,$2,$3,$4,1,'user','user',$5,$6,$7,$8,$9,$10,0,1,0,$11,'line:1','claude@2.0')`, id, conv, source, claude.NativeID(native, 0), text, len(text), sum[:], i+1, i == 0, map[string]any{"persisted_output": secret}, len(raw))
	}
	var manifestBefore, ledgerBefore, tailBefore []byte
	if err := s.pool.QueryRow(ctx, `SELECT to_jsonb(t) FROM provisional_tails t WHERE source_id=$1`, source).Scan(&tailBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m)) FROM manifest_entries m WHERE source_id=$1`, source).Scan(&manifestBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY name) FROM flopwire_schema_migrations m`).Scan(&ledgerBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx, s.pool); err != nil {
		t.Fatal(err)
	}
	if s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_parser IS NULL AND applied_redaction_rules IS NULL`, source) != 1 || s.count(`SELECT count(*) FROM messages WHERE redaction_rules IS NULL`) != 2 {
		t.Fatal("upgrade lost rows or prematurely stamped fresh")
	}
	if err := store.Migrate(ctx, s.pool); err != nil {
		t.Fatalf("repeated migration: %v", err)
	}
	var ledgerAfter []byte
	if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY name) FROM flopwire_schema_migrations m WHERE name<'008_reparse.sql'`).Scan(&ledgerAfter); err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatalf("old ledger changed: %v", err)
	}
	// The upgraded process has no upload notifications in memory. Both stale
	// reads and the background worker use this fresh queue.
	s.queue = &ingest.Queue{Pool: s.pool, Objects: s.objects, Log: s.queue.Log}
	readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
	defer cancelRead()
	out, err := s.client.Read(readCtx, format.ReadQuery{Address: live}, format.Filters{})
	if err != nil || out.Focus != live || len(out.Messages) != 1 || out.Messages[0].ID != live {
		t.Fatalf("stale read: %v", err)
	}
	if s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND refresh_requested_at IS NOT NULL AND applied_parser IS NULL`, source) != 1 || s.count(`SELECT count(*) FROM messages WHERE parse_attempt=0`) != 2 {
		t.Fatal("read did not prioritize or waited for extraction")
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.queue.Run(runCtx) }()
	t.Cleanup(func() { stop(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND applied_parser=$2 AND applied_redaction_rules=$3 AND requested_seq=parsed_seq`, source, claude.ParserName, redact.RulesVersion) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("upgraded archive was not refreshed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done
	if s.count(`SELECT count(*) FROM source_parse_state WHERE source_id=$1 AND cursor_offset=$2 AND cursor_line=2`, source, len(raw)+len(tail)) != 1 {
		t.Fatal("refresh did not replay archived chunk and tail")
	}
	if s.count(`SELECT count(*) FROM messages WHERE NOT superseded AND native_id=ANY($1::text[]) AND parser=$2 AND parse_attempt>0`, []string{claude.NativeID(native, 0), claude.NativeID(tailNative, 0)}, claude.ParserName) != 2 {
		var rows []byte
		_ = s.pool.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_object('native_id',native_id,'parser',parser,'attempt',parse_attempt,'superseded',superseded,'kind',kind)) FROM messages`).Scan(&rows)
		t.Fatalf("refresh did not rederive live rows from both archived records: %s", rows)
	}
	if s.count(`SELECT count(*) FROM messages WHERE strpos(text,$1)>0 OR strpos(enrichment::text,$1)>0 OR redaction_rules IS DISTINCT FROM $2`, secret, redact.RulesVersion) != 0 {
		t.Fatal("upgrade left secret in stored row versions")
	}
	if s.count(`SELECT count(*) FROM messages WHERE id=$1 AND superseded`, history) != 1 {
		t.Fatal("history was discarded instead of masked")
	}
	if s.count(`SELECT count(*) FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id WHERE strpos(c.title,$1)>0 OR strpos(a.digest::text,$1)>0`, secret) != 0 {
		t.Fatal("upgrade left secret in summaries")
	}
	archived, err := s.objects.Get(ctx, ingest.ChunkKey(hash))
	if err != nil || !bytes.Equal(archived, z) {
		t.Fatalf("archive object changed: %v", err)
	}
	var manifestAfter []byte
	if err := s.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m)) FROM manifest_entries m WHERE source_id=$1`, source).Scan(&manifestAfter); err != nil || !bytes.Equal(manifestBefore, manifestAfter) {
		t.Fatalf("archive manifest changed: %v", err)
	}
	var tailAfter []byte
	if err := s.pool.QueryRow(ctx, `SELECT to_jsonb(t) FROM provisional_tails t WHERE source_id=$1`, source).Scan(&tailAfter); err != nil || !bytes.Equal(tailBefore, tailAfter) {
		t.Fatalf("archive tail changed: %v", err)
	}
	// Fresh process state must discover that this upgrade work is already done.
	var rowsBefore, rowsAfter []byte
	snapshot := `SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM messages m`
	if err := s.pool.QueryRow(ctx, snapshot).Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}
	restarted := &ingest.Queue{Pool: s.pool, Objects: s.objects, Log: s.queue.Log}
	if err := restarted.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, snapshot).Scan(&rowsAfter); err != nil || !bytes.Equal(rowsBefore, rowsAfter) {
		t.Fatalf("restart repeated completed work: %v", err)
	}
}
