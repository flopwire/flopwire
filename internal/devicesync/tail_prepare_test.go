package devicesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type tailPreparationFixture struct {
	dbPath string
	store  *Store
	spool  *Spool
}

func newTailPreparationFixture(t *testing.T) tailPreparationFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	spool := publicationSpool(t, 4096)
	if _, err := store.db.Exec(`CREATE TABLE untouched(value TEXT); INSERT INTO untouched VALUES ('literal retained state')`); err != nil {
		t.Fatal(err)
	}
	return tailPreparationFixture{path, store, spool}
}

func (f tailPreparationFixture) add(t *testing.T, kind transcript.StorageKind, export bool, body []byte, state []byte, acked bool) (int64, syncproto.Hash) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "synthetic-source")
	src, err := f.store.source(ctx, path, &SourceSpec{Path: path, StorageKind: kind, Agent: transcript.AgentClaude, SessionKey: "literal-fixture", Parser: "claude:v1", Export: export})
	if err != nil {
		t.Fatal(err)
	}
	hash := syncproto.Sum(body)
	g := &genRow{SourceID: src.ID, Gen: 0, FileID: "literal-inode", Size: int64(len(body)), Tail: syncproto.Tail{Size: int64(len(body)), Hash: hash}, TailAcked: acked}
	wm := &transcript.Watermark{Offset: int64(len(body))}
	if err := f.store.saveCapture(ctx, src, g, nil, wm, state); err != nil {
		t.Fatal(err)
	}
	return src.ID, hash
}

func tailPreparationReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestPrepareLegacyTailsVerifiedConversionAndReadOnlyState(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("independently specified committed export bytes\n")
	sid, hash := f.add(t, transcript.StorageSQLite, true, body, []byte("resume-state"), true)
	old := []byte("previous committed export bytes\n")
	if err := f.spool.PutTail(sid, 0, old); err != nil {
		t.Fatal(err)
	}
	if err := f.spool.PutTailVersion(sid, 0, hash, body); err != nil {
		t.Fatal(err)
	}
	if err := f.spool.PutTailVersion(sid, 0, syncproto.Sum(old), old); err != nil {
		t.Fatal(err)
	}
	chunk := []byte("unrelated retained chunk\n")
	if err := f.spool.PutChunk(syncproto.Sum(chunk), chunk); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(f.spool.dir, "tails", "do-not-guess.tmp")
	if err := os.WriteFile(unknown, []byte("untouched temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db := tailPreparationReadOnly(t, f.dbPath)
	result, err := PrepareLegacyTails(t.Context(), db, f.spool.dir)
	if err != nil || result.Needed != 1 || result.Converted != 1 || result.RemovedVersions != 1 {
		t.Fatalf("conversion: %+v %v", result, err)
	}
	// Reopen accounting and use exactly the old reader's canonical Tail API.
	spool, err := OpenSpool(f.spool.dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	actual, found, err := spool.Tail(sid, 0)
	if err != nil || !found || !bytes.Equal(actual, body) || spool.Used() != int64(len(body)+len(chunk)+len("untouched temporary")) {
		t.Fatalf("old canonical reader/accounting: found=%v used=%d err=%v", found, spool.Used(), err)
	}
	if actual, found, err := spool.Chunk(syncproto.Sum(chunk)); err != nil || !found || !bytes.Equal(actual, chunk) {
		t.Fatal("conversion changed unrelated chunks")
	}
	if _, err := os.Stat(unknown); err != nil {
		t.Fatal("conversion swept unrecognized files")
	}
	again, err := PrepareLegacyTails(t.Context(), db, f.spool.dir)
	if err != nil || again.Canonical != 1 || again.Converted != 0 || again.RemovedVersions != 0 {
		t.Fatalf("idempotence: %+v %v", again, err)
	}
	var sentinel string
	if err := db.QueryRow(`SELECT value FROM untouched`).Scan(&sentinel); err != nil || sentinel != "literal retained state" {
		t.Fatal("unrelated state changed")
	}
	var tailHash []byte
	var gen, size int64
	if err := db.QueryRow(`SELECT generation,size,tail_hash FROM devsync_gens WHERE source_id=?`, sid).Scan(&gen, &size, &tailHash); err != nil || gen != 0 || size != int64(len(body)) || !bytes.Equal(tailHash, hash[:]) {
		t.Fatal("committed generation changed")
	}
	if _, err := db.Exec(`DELETE FROM untouched`); err == nil {
		t.Fatal("fixture handle was not read-only")
	}
	db.Close()
	after, err := os.ReadFile(f.dbPath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("preparation changed live SQLite bytes")
	}
}

func TestPrepareLegacyTailsValidatesAllNeededBeforeMutation(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal complete provisional bytes\n")
	one, hash := f.add(t, transcript.StorageJSONDoc, false, body, nil, false)
	two, _ := f.add(t, transcript.StorageSQLite, true, body, nil, false)
	if err := f.spool.PutTailVersion(one, 0, hash, body); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir); err == nil {
		t.Fatal("missing required later candidate did not stop preparation")
	}
	if _, found, err := f.spool.Tail(one, 0); err != nil || found {
		t.Fatal("earlier conversion occurred before all needed validation")
	}
	if actual, found, err := f.spool.TailVersion(one, 0, hash); err != nil || !found || !bytes.Equal(actual, body) {
		t.Fatal("failed preparation removed committed immutable bytes")
	}
	if err := f.spool.PutTail(two, 0, []byte("mismatching legacy")); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir); err == nil {
		t.Fatal("existing mismatching required tail was exempted")
	}
}

func TestPrepareLegacyTailsUnspooledClassification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   transcript.StorageKind
		export bool
		state  []byte
		change string
		wantOK bool
	}{
		{name: "current native", kind: transcript.StorageJSONLAppend, wantOK: true},
		{name: "current companion", kind: transcript.StorageCompanion, wantOK: true},
		{name: "rewrite document", kind: transcript.StorageJSONDoc},
		{name: "export", kind: transcript.StorageSQLite, export: true},
		{name: "current resume tail", kind: transcript.StorageSQLite, export: true, state: []byte("resume")},
		{name: "closed native", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_gens SET closed=1`},
		{name: "noncurrent native", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET generation=1`},
		{name: "vanished native", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET watermark=NULL`},
		{name: "null watermark", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET watermark='null'`},
		{name: "empty watermark", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET watermark='{}'`},
		{name: "wrong watermark", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET watermark='{"Offset":1}'`},
		{name: "broken tail range", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_gens SET tail_offset=1`},
		{name: "protected orphan", kind: transcript.StorageJSONLAppend, change: `UPDATE devsync_sources SET protected_origin='desktop-code',watermark=NULL`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTailPreparationFixture(t)
			f.add(t, tc.kind, tc.export, []byte("literal tail bytes\n"), tc.state, tc.state != nil)
			if tc.change != "" {
				if _, err := f.store.db.Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			got, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir)
			if (err == nil) != tc.wantOK || tc.wantOK && got.Unspooled != 1 {
				t.Fatalf("classification: %+v %v", got, err)
			}
		})
	}
}

func TestPrepareLegacyTailsNativeMismatchNotUnspooled(t *testing.T) {
	for _, immutable := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "immutable"}[immutable], func(t *testing.T) {
			f := newTailPreparationFixture(t)
			sid, _ := f.add(t, transcript.StorageJSONLAppend, false, []byte("committed\n"), nil, false)
			wrong := []byte("other committed version\n")
			var err error
			if immutable {
				err = f.spool.PutTailVersion(sid, 0, syncproto.Sum(wrong), wrong)
			} else {
				err = f.spool.PutTail(sid, 0, wrong)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir); err == nil {
				t.Fatal("native mismatch exempted as intentionally unspooled")
			}
		})
	}
}

func TestPrepareLegacyTailsInterruptedAndResumable(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal resumable tail\n")
	var ids []int64
	for range 2 {
		sid, hash := f.add(t, transcript.StorageSQLite, true, body, nil, false)
		ids = append(ids, sid)
		if err := f.spool.PutTailVersion(sid, 0, hash, body); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	first, err := prepareLegacyTails(ctx, f.store.db, f.spool.dir, func() error { cancel(); return ctx.Err() })
	if !errors.Is(err, context.Canceled) || first.Converted != 1 {
		t.Fatalf("actual rename interruption: %+v %v", first, err)
	}
	if got, found, err := f.spool.Tail(ids[0], 0); err != nil || !found || !bytes.Equal(got, body) {
		t.Fatal("interrupted conversion did not leave verified canonical")
	}
	second, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir)
	if err != nil || second.Canonical != 1 || second.Converted != 1 {
		t.Fatalf("resume: %+v %v", second, err)
	}
	third, err := PrepareLegacyTails(t.Context(), f.store.db, f.spool.dir)
	if err != nil || third.Canonical != 2 || third.Converted != 0 {
		t.Fatalf("idempotent resume: %+v %v", third, err)
	}
}

func TestPrepareLegacyTailsProcessDeathAfterRename(t *testing.T) {
	const fixtureEnv = "FLOPWIRE_TEST_TAIL_PREPARE_DEATH"
	if raw := os.Getenv(fixtureEnv); raw != "" {
		var fixture struct{ DB, Spool string }
		if json.Unmarshal([]byte(raw), &fixture) != nil {
			os.Exit(21)
		}
		db := tailPreparationReadOnly(t, fixture.DB)
		_, err := prepareLegacyTails(t.Context(), db, fixture.Spool, func() error { os.Exit(23); return nil })
		if err != nil {
			os.Exit(22)
		}
		os.Exit(24) // barrier not reached
	}
	f := newTailPreparationFixture(t)
	body := []byte("literal bytes preserved across process death\n")
	sid, hash := f.add(t, transcript.StorageSQLite, true, body, nil, false)
	if err := f.spool.PutTailVersion(sid, 0, hash, body); err != nil {
		t.Fatal(err)
	}
	f.store.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct{ DB, Spool string }{f.dbPath, f.spool.dir})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPrepareLegacyTailsProcessDeathAfterRename$")
	cmd.Env = append(os.Environ(), fixtureEnv+"="+string(raw))
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("real rename/death barrier not reached: %v", err)
	}
	db := tailPreparationReadOnly(t, f.dbPath)
	result, err := PrepareLegacyTails(t.Context(), db, f.spool.dir)
	if err != nil || result.Canonical != 1 || result.Converted != 0 {
		t.Fatalf("restart after actual process death: %+v %v", result, err)
	}
	spool, err := OpenSpool(f.spool.dir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := spool.Tail(sid, 0); err != nil || !found || !bytes.Equal(got, body) {
		t.Fatal("process death changed committed tail bytes")
	}
}
