package ingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// This regression drives explicit real Agent.Once sweeps and the background
// scheduler through authenticated API, PostgreSQL and MinIO. It does not run
// the installed CLI, watch the Mac UI, or migrate an external history index.
func TestDesktopCodeAgentCLIOverlapOutageRestartCatchup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := newChainEnv(t)
	h := newChainAPI(t, e, false) // Always use the real policy/capability response.
	id, token := e.credential(t, "desktop-code-overlap-catchup")
	d := newChainDevice(t, id, token, h)
	d.config.CoworkRoot = "-"
	d.config.DesktopCodeRoot = filepath.Join(d.home, "desktop-code")
	const app = "local_aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	metadata := filepath.Join(d.config.DesktopCodeRoot, "account", "org", app+".json")
	md, err := json.Marshal(map[string]any{"sessionId": app, "cliSessionId": chainNativeID, "cwd": d.selected, "originCwd": d.selected})
	if err != nil {
		t.Fatal(err)
	}
	chainWrite(t, metadata, md)
	native := filepath.Join(d.config.DesktopCodeRoot, "account", "org", app, ".claude", "projects", "-host-repo", chainNativeID+".jsonl")
	cli := filepath.Join(d.config.ClaudeProjects, "configured-project", chainNativeID+".jsonl")
	paths := []string{native, cli}
	rows := []desktopChainRow{
		{native: "desktop-baseline-user", role: "user", text: "desktop overlap baseline user marker"},
		{native: "desktop-baseline-answer", parent: "desktop-baseline-user", role: "assistant", text: "desktop overlap baseline answer marker"},
		{native: "desktop-outage-user", parent: "desktop-baseline-answer", role: "user", text: "desktop overlap outage user marker"},
		{native: "desktop-outage-answer", parent: "desktop-outage-user", role: "assistant", text: "desktop overlap outage answer marker"},
	}
	var body []byte
	for i := range rows {
		rows[i].offset = int64(len(body))
		rows[i].line = int64(i + 1)
		record := desktopChainRecord(d.selected, rows[i])
		rows[i].length = int64(len(record))
		body = append(body, record...)
	}
	baseline := body[:rows[2].offset]
	for _, path := range paths {
		chainWrite(t, path, baseline)
	}
	identities := map[string]string{}
	for _, path := range paths {
		identity, err := transcript.StatIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		identities[path] = identity.ID.String()
	}
	if identities[native] == identities[cli] {
		t.Fatal("overlap copies must be distinct native file identities")
	}
	outage := &desktopChainFlushOutage{base: h.Client().Transport, token: token}
	if outage.base == nil {
		outage.base = http.DefaultTransport
	}
	d.client.HTTP = &http.Client{Transport: outage, Timeout: 15 * time.Second}
	d.start(t)
	d.once(t)
	chainEventually(t, "both baseline native archives and durable ACKs", func() error {
		if err := desktopChainCaptures(ctx, d, paths, native, int64(len(baseline)), true); err != nil {
			return err
		}
		if err := desktopChainRaw(ctx, e, d, id, paths, baseline); err != nil {
			return err
		}
		return desktopChainRows(ctx, e, d, id, paths, identities, rows[:2], true)
	})
	// Inject only this enrolled device's Flush transport. Policy, capabilities,
	// Has, and authenticated retrieval keep their actual routes throughout.
	outage.deny.Store(true)
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.Write(body[len(baseline):])
		syncErr, closeErr := f.Sync(), f.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if syncErr != nil {
			t.Fatal(syncErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	d.once(t)
	chainEventually(t, "real outage attempt and both durable pending new extents", func() error {
		if outage.denied.Load() == 0 {
			return fmt.Errorf("no real Flush denial yet")
		}
		if err := desktopChainCaptures(ctx, d, paths, native, int64(len(body)), false); err != nil {
			return err
		}
		if err := desktopChainRows(ctx, e, d, id, paths, identities, rows, false); err != nil {
			return err
		}
		return desktopChainServerUnchanged(ctx, e, id, rows[:2], rows[2:])
	})
	// Reopen every index/store/spool handle, then Syncer/Scheduler/Agent, on the same durable paths
	// while transport remains down. Restart must neither drop the queue nor
	// publish the new markers before a real successful upload.
	d.halt(t)
	d.sy.Close()
	pendingBefore, err := desktopChainPendingState(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := d.index.Path()
	if err := d.index.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.syncStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.index.Close(); err != nil {
		t.Fatal(err)
	}
	d.index, err = localindex.Open(indexPath, localindex.Options{DeviceID: id, DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	reopenedIndex := d.index
	t.Cleanup(func() { _ = reopenedIndex.Close() })
	d.syncDB, err = sql.Open("sqlite", "file:"+indexPath+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	d.syncDB.SetMaxOpenConns(1)
	reopenedDB := d.syncDB
	t.Cleanup(func() { _ = reopenedDB.Close() })
	d.syncStore, err = devicesync.NewStore(d.syncDB)
	if err != nil {
		t.Fatal(err)
	}
	d.spool, err = devicesync.OpenSpool(filepath.Join(d.home, "spool"), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	pendingAfter, err := desktopChainPendingState(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pendingBefore, pendingAfter) {
		t.Fatal("durable pending identities, manifest/tail ACKs, watermarks or capture proofs changed across handle reopen")
	}
	before := outage.denied.Load()
	d.sy, err = devicesync.NewSyncer(devicesync.Config{Logger: d.config.Logger, SealAfter: -1, Chunk: devicesync.ChunkParams{Min: 1024, Avg: 4096, Max: 16384}}, d.syncStore, d.spool, d.client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.sy.Close)
	d.scheduler = devicesync.NewScheduler(d.sy, devicesync.SchedulerConfig{Append: devicesync.Cadence{Debounce: time.Millisecond, MaxWait: time.Millisecond}})
	d.config.Sync = d.scheduler
	d.start(t)
	d.once(t)
	chainEventually(t, "restarted durable queue actually retries while Flush remains denied", func() error {
		if outage.denied.Load() <= before {
			return fmt.Errorf("restarted scheduler has not attempted real denied Flush")
		}
		if err := desktopChainCaptures(ctx, d, paths, native, int64(len(body)), false); err != nil {
			return err
		}
		if err := desktopChainRows(ctx, e, d, id, paths, identities, rows, false); err != nil {
			return err
		}
		return desktopChainServerUnchanged(ctx, e, id, rows[:2], rows[2:])
	})
	outage.deny.Store(false)
	chainEventually(t, "restarted backlog catches up with exact raw evidence and unique rows", func() error {
		if err := desktopChainCaptures(ctx, d, paths, native, int64(len(body)), true); err != nil {
			return err
		}
		if err := desktopChainRaw(ctx, e, d, id, paths, body); err != nil {
			return err
		}
		return desktopChainRows(ctx, e, d, id, paths, identities, rows, true)
	})
	d.once(t)
	chainEventually(t, "unchanged sweep retains exact single conversation and message heads", func() error {
		if err := desktopChainCaptures(ctx, d, paths, native, int64(len(body)), true); err != nil {
			return err
		}
		if err := desktopChainRaw(ctx, e, d, id, paths, body); err != nil {
			return err
		}
		return desktopChainRows(ctx, e, d, id, paths, identities, rows, true)
	})
	d.halt(t)
}

type desktopChainFlushOutage struct {
	base   http.RoundTripper
	token  string
	deny   atomic.Bool
	denied atomic.Int64
}

func (o *desktopChainFlushOutage) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == syncproto.PathFlush && r.Header.Get("Authorization") == "Bearer "+o.token && o.deny.Load() {
		o.denied.Add(1)
		return nil, fmt.Errorf("synthetic device-only Flush outage")
	}
	return o.base.RoundTrip(r)
}

type desktopChainRow struct {
	native, parent, role, text string
	offset, length, line       int64
}

func desktopChainRecord(cwd string, row desktopChainRow) []byte {
	var parent any
	if row.parent != "" {
		parent = row.parent
	}
	content := any(row.text)
	if row.role == "assistant" {
		content = []map[string]string{{"type": "text", "text": row.text}}
	}
	record := map[string]any{"parentUuid": parent, "cwd": cwd, "sessionId": chainNativeID, "type": row.role, "message": map[string]any{"role": row.role, "content": content}, "uuid": row.native, "timestamp": "2026-10-10T12:00:00.000Z"}
	raw, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func desktopChainCaptures(ctx context.Context, d *chainDevice, paths []string, native string, size int64, acked bool) error {
	for _, path := range paths {
		var generation, gotSize, entries, ack, tailSize, srvOffset, srvSize, count int64
		var tailAck, lost bool
		var proof []byte
		if err := d.syncDB.QueryRowContext(ctx, `SELECT g.generation,g.size,g.entries,g.acked,g.tail_size,g.tail_acked,g.lost,g.srv_tail_off,g.srv_tail_size,g.capture_proof FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id AND g.generation=s.generation WHERE s.path=?`, path).Scan(&generation, &gotSize, &entries, &ack, &tailSize, &tailAck, &lost, &srvOffset, &srvSize, &proof); err != nil {
			return err
		}
		if err := d.syncDB.QueryRowContext(ctx, `SELECT count(*) FROM devsync_gens g JOIN devsync_sources s ON s.id=g.source_id WHERE s.path=?`, path).Scan(&count); err != nil {
			return err
		}
		if generation != 0 || count != 1 || gotSize != size || lost {
			return fmt.Errorf("capture %s generation=%d count=%d size=%d lost=%v", path, generation, count, gotSize, lost)
		}
		if path == native {
			var p devicesync.CaptureProof
			if json.Unmarshal(proof, &p) != nil || p.Origin != "desktop-code" || p.Root != d.config.DesktopCodeRoot || p.Offset != size || p.Identity.Size != size || len(p.PolicyRequestDigest) != 64 {
				return fmt.Errorf("Desktop Code capture lacks exact current protected proof")
			}
		}
		done := ack == entries && tailAck
		if done != acked {
			return fmt.Errorf("capture %s ACK complete=%v, want %v", path, done, acked)
		}
		if acked && tailSize > 0 && srvOffset+srvSize != size {
			return fmt.Errorf("capture %s server tail ACK extent=%d, want %d", path, srvOffset+srvSize, size)
		}
		if !acked && srvOffset+srvSize >= size {
			return fmt.Errorf("capture %s outage ACK already covers new extent", path)
		}
	}
	return nil
}
func desktopChainRaw(ctx context.Context, e *chainEnv, d *chainDevice, id string, paths []string, want []byte) error {
	if err := e.queue.Drain(ctx); err != nil {
		return err
	}
	c := client.HTTP{Server: d.client.Server, Token: d.client.Token, Client: d.client.HTTP}
	for _, path := range paths {
		var source string
		var generation, size, count int64
		if err := e.pool.QueryRow(ctx, `SELECT s.id::text,g.generation,g.size FROM sources s JOIN generations g ON g.source_id=s.id WHERE s.device_id=$1 AND s.path=$2 ORDER BY g.generation DESC LIMIT 1`, id, path).Scan(&source, &generation, &size); err != nil {
			return err
		}
		if generation != 0 || size != int64(len(want)) {
			return fmt.Errorf("archive extent for %s: %d/%d", path, generation, size)
		}
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM sources s JOIN generations g ON g.source_id=s.id WHERE s.device_id=$1 AND s.path=$2`, id, path).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("archive %s has %d source generations", path, count)
		}
		got, err := c.Raw(ctx, source, generation, 0, size)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("archive bytes/hash differ for %s: got=%x want=%x", path, sha256.Sum256(got), sha256.Sum256(want))
		}
	}
	return nil
}

func desktopChainRows(ctx context.Context, e *chainEnv, d *chainDevice, id string, paths []string, identities map[string]string, want []desktopChainRow, server bool) error {
	var conversations, count int
	if err := d.index.DB().QueryRowContext(ctx, `SELECT count(*) FROM conversations WHERE agent='claude' AND session_id=?`, chainNativeID).Scan(&conversations); err != nil {
		return err
	}
	if err := d.index.DB().QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.session_id=? AND NOT m.superseded`, chainNativeID).Scan(&count); err != nil {
		return err
	}
	if conversations != 1 || count != len(want) {
		return fmt.Errorf("local conversations/heads=%d/%d want=1/%d", conversations, count, len(want))
	}
	if server {
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE device_id=$1 AND agent='claude' AND session_id=$2 AND hidden_at IS NULL`, id, chainNativeID).Scan(&conversations); err != nil {
			return err
		}
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.device_id=$1 AND c.session_id=$2 AND NOT m.superseded`, id, chainNativeID).Scan(&count); err != nil {
			return err
		}
		if conversations != 1 || count != len(want) {
			return fmt.Errorf("server conversations/heads=%d/%d want=1/%d", conversations, count, len(want))
		}
	}
	for _, row := range want {
		var path, file, hash, parser, role, parent string
		var generation, offset, length, line, ordinal int64
		if err := d.index.DB().QueryRowContext(ctx, `SELECT s.path,s.file_id,lower(hex(m.content_sha)),m.parser,m.role,COALESCE(m.parent_native_id,''),m.source_generation,m.byte_offset,m.byte_len,m.line_no,m.ordinal FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN sources s ON s.id=m.source_id WHERE c.session_id=? AND m.native_id=? AND NOT m.superseded`, chainNativeID, row.native+"#0").Scan(&path, &file, &hash, &parser, &role, &parent, &generation, &offset, &length, &line, &ordinal); err != nil {
			return err
		}
		if err := desktopChainProvenance(row, paths, identities, path, file, hash, parser, role, parent, generation, offset, length, line, ordinal, 1); err != nil {
			return err
		}
		hits, err := d.index.Find(ctx, row.text, localindex.FindOptions{CaseSensitive: true})
		if err != nil {
			return err
		}
		if len(hits) != 1 || hits[0].Text != row.text {
			return fmt.Errorf("local literal marker %q not exactly retained", row.text)
		}
		if server {
			var text string
			var exists bool
			if err := e.pool.QueryRow(ctx, `SELECT s.path,s.file_id,encode(m.content_sha,'hex'),m.parser,m.role,COALESCE(m.parent_native_id,''),m.source_generation,m.byte_offset,m.byte_len,m.line_no,m.ordinal,m.text,g.source_id IS NOT NULL FROM messages m JOIN conversations c ON c.id=m.conversation_id JOIN sources s ON s.id=m.source_id LEFT JOIN generations g ON g.source_id=m.source_id AND g.generation=m.source_generation WHERE c.device_id=$1 AND c.session_id=$2 AND m.native_id=$3 AND NOT m.superseded`, id, chainNativeID, row.native+"#0").Scan(&path, &file, &hash, &parser, &role, &parent, &generation, &offset, &length, &line, &ordinal, &text, &exists); err != nil {
				return err
			}
			if text != row.text || !exists {
				return fmt.Errorf("server literal marker or generation missing")
			}
			if err := desktopChainProvenance(row, paths, identities, path, file, hash, parser, role, parent, generation, offset, length, line, ordinal, 0); err != nil {
				return err
			}
		}
	}
	return nil
}
func desktopChainProvenance(row desktopChainRow, paths []string, identities map[string]string, path, file, hash, parser, role, parent string, generation, offset, length, line, ordinal, expectedGeneration int64) error {
	if (path != paths[0] && path != paths[1]) || file != identities[path] || hash != fmt.Sprintf("%x", sha256.Sum256([]byte(row.text))) || parser != "claude@4.1" || role != row.role || parent != row.parent || generation != expectedGeneration || offset != row.offset || length != row.length || line != row.line || ordinal != row.offset*4096 {
		return fmt.Errorf("literal marker %.80q has wrong source/hash/native-chain extent: path=%.240q want-one-of=[%.240q,%.240q] file=%.80q want=%.80q generation=%d want=%d parser=%.80q want=claude@4.1 role=%.40q want=%.40q parent=%.80q want=%.80q hash=%.64q want=%.64q offset=%d want=%d length=%d want=%d line=%d want=%d ordinal=%d want=%d", row.native, path, paths[0], paths[1], file, identities[path], generation, expectedGeneration, parser, role, row.role, parent, row.parent, hash, fmt.Sprintf("%x", sha256.Sum256([]byte(row.text))), offset, row.offset, length, row.length, line, row.line, ordinal, row.offset*4096)
	}
	return nil
}
func desktopChainServerUnchanged(ctx context.Context, e *chainEnv, id string, baseline, pending []desktopChainRow) error {
	if err := e.queue.Drain(ctx); err != nil {
		return err
	}
	var count int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.device_id=$1 AND NOT m.superseded`, id).Scan(&count); err != nil {
		return err
	}
	if count != len(baseline) {
		return fmt.Errorf("server changed during outage: heads=%d want=%d", count, len(baseline))
	}
	for _, row := range pending {
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.device_id=$1 AND (m.native_id=$2 OR m.text=$3) AND NOT m.superseded`, id, row.native+"#0", row.text).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("pending marker reached server during Flush outage")
		}
	}
	return nil
}

// The comparison excludes retry scheduling hints. Every persisted source,
// generation, watermark, proof and manifest remains an exact durable oracle.
func desktopChainPendingState(ctx context.Context, d *chainDevice) ([]byte, error) {
	rows, err := d.syncDB.QueryContext(ctx, `SELECT s.id,s.path,s.spec,s.generation,s.watermark,s.protected_origin,s.protected_root,g.file_id,g.closed,g.generation,g.size,g.entries,g.acked,g.tail_offset,g.tail_size,hex(g.tail_hash),g.tail_acked,g.lost,g.srv_tail_off,g.srv_tail_size,COALESCE(g.capture_proof,'') FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id ORDER BY s.path,g.generation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshot [][]any
	for rows.Next() {
		values := make([]any, 21)
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		snapshot = append(snapshot, values)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(snapshot) != 2 {
		return nil, fmt.Errorf("pending snapshot has %d generations, want two", len(snapshot))
	}
	rows.Close()
	manifest, err := d.syncDB.QueryContext(ctx, `SELECT s.path,m.generation,m.ordinal,hex(m.hash),m.offset,m.size FROM devsync_manifest m JOIN devsync_sources s ON s.id=m.source_id ORDER BY s.path,m.generation,m.ordinal`)
	if err != nil {
		return nil, err
	}
	defer manifest.Close()
	for manifest.Next() {
		values := make([]any, 6)
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := manifest.Scan(dest...); err != nil {
			return nil, err
		}
		snapshot = append(snapshot, values)
	}
	if err := manifest.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(snapshot)
}
