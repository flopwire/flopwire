package devicesync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func devinExport() SourceSpec {
	return SourceSpec{Path: "devin:sessions.db#s1", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin@1", Export: true}
}

// S1: an intermediate version of a rewritten source whose prefix chunks the
// server already holds survives another rewrite while the server is down.
// Salvage used to treat those (unspooled, known) chunks as lost and cut the
// generation to nothing.
func TestSalvageKeepsChunksTheServerHolds(t *testing.T) {
	e := newEnv(t, Config{}, 64<<20)
	sp := devinExport()
	ctx := context.Background()
	rows := jsonlLines(13, 600, 150)
	if err := e.sy.SyncExport(ctx, sp, rows[:40000]); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	v2 := bytes.Clone(rows[:80000])
	v2[39990] = 'Z' // in-place edit near the end of v1: a rewrite sharing its prefix
	if err := e.sy.SyncExport(ctx, sp, v2); err == nil {
		t.Fatal("want error while down")
	}
	v3 := rows[1000:50000] // another rewrite before the server returns
	if err := e.sy.SyncExport(ctx, sp, v3); err == nil {
		t.Fatal("want error while down")
	}
	e.srv.SetDown(false)
	if err := e.sy.Resume(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.logs.String(), "lost before upload") {
		t.Fatalf("salvage recorded a gap:\n%s", e.logs)
	}
	e.requireServerHas(sp.Path, "", 1, v2)
	e.requireServerHas(sp.Path, "", 2, v3)
	if e.spool.Used() != 0 {
		t.Fatalf("spool not released: %d", e.spool.Used())
	}
}

// S1: if the server later reports missing a chunk salvage relied on, an
// older generation records the gap there instead of failing forever.
func TestOldGenerationMissingKnownChunkRecordsGap(t *testing.T) {
	e := newEnv(t, Config{}, 64<<20)
	sp := devinExport()
	ctx := context.Background()
	rows := jsonlLines(15, 600, 150)
	v1 := rows[:40000]
	if err := e.sy.SyncExport(ctx, sp, v1); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	v2 := bytes.Clone(rows[:80000])
	v2[39990] = 'Z'
	e.sy.SyncExport(ctx, sp, v2)
	e.sy.SyncExport(ctx, sp, rows[1000:50000])
	// The server loses the first chunk v1 and v2 share.
	ents, _ := e.srv.Manifest(sp.Path, "", 0)
	e.srv.DeleteChunk(ents[0].Hash)
	e.srv.SetDown(false)
	for range 3 { // the missing report, the gap, then the rest
		if err := e.sy.Resume(ctx, sp); err == nil {
			break
		}
	}
	if err := e.sy.Resume(ctx, sp); err != nil {
		t.Fatalf("resume after gap: %v\n%s", err, e.logs)
	}
	if !strings.Contains(e.logs.String(), "lost before upload") {
		t.Fatalf("gap not logged:\n%s", e.logs)
	}
	e.requireServerHas(sp.Path, "", 2, rows[1000:50000])
}

// S5: spooled chunks of entries a gap cut away are released.
func TestGapCutReleasesSpool(t *testing.T) {
	e := newEnv(t, Config{}, 64<<20)
	sp := devinExport()
	ctx := context.Background()
	rows := jsonlLines(16, 600, 150)
	e.srv.SetDown(true)
	e.sy.SyncExport(ctx, sp, rows[:60000])
	ents, err := e.store.entries(ctx, 1, 0, 0, 1000)
	if err != nil || len(ents) < 4 {
		t.Fatalf("entries: %v %d", err, len(ents))
	}
	e.spool.DropChunk(ents[1].Hash) // an unrecoverable chunk mid-generation
	e.sy.SyncExport(ctx, sp, rows[5000:70000])
	e.srv.SetDown(false)
	if err := e.sy.Resume(ctx, sp); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.logs.String(), "lost before upload") {
		t.Fatalf("gap not logged:\n%s", e.logs)
	}
	e.requireServerHas(sp.Path, "", 1, rows[5000:70000])
	if e.spool.Used() != 0 {
		t.Fatalf("spool holds %d bytes after draining", e.spool.Used())
	}
}

// S6: a new Syncer drops spool files nothing pending needs (a crash between
// spooling and committing the capture), and keeps the ones it needs.
func TestSpoolSweptAtStart(t *testing.T) {
	e := newEnv(t, Config{}, 64<<20)
	sp := devinExport()
	ctx := context.Background()
	data := jsonlLines(17, 200, 150)
	e.srv.SetDown(true)
	e.sy.SyncExport(ctx, sp, data)
	pending := e.spool.Used()
	if pending == 0 {
		t.Fatal("nothing spooled")
	}
	orphan := []byte("orphan chunk from a crashed capture")
	e.spool.PutChunk(syncproto.Sum(orphan), orphan)
	e.spool.PutTail(99, 3, []byte("orphan tail"))
	os.WriteFile(filepath.Join(e.dir, "spool", "chunks", "deadbeef.tmp"), []byte("partial"), 0o600)
	e.sy.Close()

	spool, err := OpenSpool(filepath.Join(e.dir, "spool"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := NewSyncer(Config{Chunk: small}, e.store, spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	if spool.Used() != pending {
		t.Fatalf("spool holds %d bytes after the sweep, want %d", spool.Used(), pending)
	}
	if _, ok, _ := spool.Chunk(syncproto.Sum(orphan)); ok {
		t.Fatal("orphan chunk kept")
	}
	e.srv.SetDown(false)
	if err := sy.Resume(ctx, sp); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, "", 0, data)
	if spool.Used() != 0 {
		t.Fatalf("spool not released: %d", spool.Used())
	}
}

// S8: a companion links to its parent as it was at capture, even when the
// parent is replaced before the upload runs.
func TestCompanionParentFromCapture(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	parent := e.spec("sess.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, parent.Path, jsonlLines(18, 50, 100))
	parentID := fileIDOf(t, parent.Path)
	comp := SourceSpec{Path: e.path("sess-note.txt"), Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion, Parent: parent.Path}
	if err := os.WriteFile(comp.Path, []byte("companion bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	if err := e.sy.Sync(context.Background(), comp); err == nil {
		t.Fatal("want error while down")
	}
	os.WriteFile(parent.Path+".n", []byte("{}\n"), 0o600)
	os.Rename(parent.Path+".n", parent.Path) // a new parent file
	e.srv.SetDown(false)
	if err := e.sy.Resume(context.Background(), comp); err != nil {
		t.Fatal(err)
	}
	desc, _, _ := e.srv.Source(comp.Path, fileIDOf(t, comp.Path))
	if desc.Parent == nil || desc.Parent.FileID != parentID {
		t.Fatalf("parent link %+v, want file id %s", desc.Parent, parentID)
	}
}

// D20: Codex archives a rollout by renaming it into archived_sessions/. The
// new path links the old one as Previous and sends no chunk bodies again.
func TestCodexArchiveRenameLinksPrevious(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	name := "rollout-2026-09-29T10-00-00-0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.jsonl"
	os.MkdirAll(e.path("sessions/2026/09/29"), 0o700)
	os.MkdirAll(e.path("archived_sessions"), 0o700)
	old := SourceSpec{Path: e.path("sessions/2026/09/29/" + name), Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend,
		SessionKey: "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", Parser: "codex@1"}
	data := jsonlLines(19, 400, 200)
	appendFile(t, old.Path, data)
	fid := fileIDOf(t, old.Path)
	e.sync(old)
	e.srv.Lock()
	before := e.srv.BodyBytes
	e.srv.Unlock()

	nu := old
	nu.Path = e.path("archived_sessions/" + name)
	if err := os.Rename(old.Path, nu.Path); err != nil {
		t.Fatal(err)
	}
	e.sync(nu) // the new path can be seen before the old one's vanish
	e.sync(old)
	desc, _, ok := e.srv.Source(nu.Path, fid)
	if !ok {
		t.Fatal("archived source not uploaded")
	}
	if desc.Previous == nil || desc.Previous.Path != old.Path || desc.Previous.FileID != fid {
		t.Fatalf("previous %+v, want %s", desc.Previous, old.Path)
	}
	e.requireServerHas(nu.Path, fid, 0, data)
	e.srv.Lock()
	sent := e.srv.BodyBytes - before
	e.srv.Unlock()
	if sent != 0 {
		t.Fatalf("re-sent %d chunk bytes for a rename", sent)
	}

	// A different session at a new path is not linked.
	other := nu
	other.Path, other.SessionKey = e.path("archived_sessions/other.jsonl"), "other"
	appendFile(t, other.Path, jsonlLines(20, 10, 100))
	e.sync(other)
	if desc, _, _ := e.srv.Source(other.Path, fileIDOf(t, other.Path)); desc.Previous != nil {
		t.Fatalf("unrelated source linked to %+v", desc.Previous)
	}
}

// S1: a vanished source's pending generation stays current (no capture
// will supersede it). If the server loses a chunk salvage skipped because
// it was known, the upload must record the gap instead of retrying forever.
func TestVanishedSourceMissingKnownChunkRecordsGap(t *testing.T) {
	e := newEnv(t, Config{}, 64<<20)
	ctx := context.Background()
	data := jsonlLines(21, 300, 150)
	a := e.spec("a.jsonl", transcript.StorageJSONLAppend)
	if err := os.WriteFile(a.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(a) // the server holds every chunk of data
	b := e.spec("b.jsonl", transcript.StorageJSONLAppend)
	if err := os.WriteFile(b.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	if err := e.sy.Sync(ctx, b); err == nil {
		t.Fatal("want error while down")
	}
	os.Remove(b.Path)
	if err := e.sy.Sync(ctx, b); err == nil { // vanish: salvage skips known chunks
		t.Fatal("want error while down")
	}
	ents, _ := e.srv.Manifest(a.Path, fileIDOf(t, a.Path), 0)
	e.srv.DeleteChunk(ents[0].Hash)
	e.srv.SetDown(false)
	var err error
	for range 5 {
		if err = e.sy.Sync(ctx, b); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("vanished source never completes: %v\nlogs:\n%s", err, e.logs)
	}
	if !strings.Contains(e.logs.String(), "lost before upload") {
		t.Fatalf("want a recorded gap:\n%s", e.logs)
	}
	if gens, _ := e.store.pendingGens(ctx, mustSource(t, e, b).ID); len(gens) != 0 {
		t.Fatalf("pending generations left: %d", len(gens))
	}
}

func mustSource(t *testing.T, e *env, sp SourceSpec) *sourceRow {
	t.Helper()
	src, err := e.store.source(context.Background(), sp.Path, &sp)
	if err != nil {
		t.Fatal(err)
	}
	return src
}
