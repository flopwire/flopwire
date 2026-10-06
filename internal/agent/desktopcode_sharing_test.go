package agent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

type desktopCodeSyncFixture struct {
	*fixture
	db     *sql.DB
	store  *devicesync.Store
	spool  *devicesync.Spool
	sy     *devicesync.Syncer
	sched  *devicesync.Scheduler
	server *synctest.Server
	client *syncproto.Client
}

func newDesktopCodeSyncFixture(t *testing.T) *desktopCodeSyncFixture {
	t.Helper()
	f := newDesktopCodeFixture(t)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+f.store.Path()+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store, err := devicesync.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := devicesync.OpenSpool(filepath.Join(f.home, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	server := synctest.New("synthetic-token")
	h := httptest.NewServer(server)
	t.Cleanup(h.Close)
	x := &desktopCodeSyncFixture{fixture: f, db: db, store: store, spool: spool, server: server, client: &syncproto.Client{Server: h.URL, Token: "synthetic-token", HTTP: h.Client()}}
	x.restartSync(t)
	t.Cleanup(func() { x.sy.Close() })
	return x
}

func (x *desktopCodeSyncFixture) restartSync(t *testing.T) {
	t.Helper()
	if x.sy != nil {
		x.sy.Close()
	}
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: x.cfg.Logger, SealAfter: -1}, x.store, x.spool, x.client)
	if err != nil {
		t.Fatal(err)
	}
	x.sy, x.sched = sy, devicesync.NewScheduler(sy, devicesync.SchedulerConfig{})
	x.cfg.Sync = x.sched
	x.fixture.restart()
}

func (x *desktopCodeSyncFixture) sourceSpec(t *testing.T, path string) devicesync.SourceSpec {
	t.Helper()
	x.a.mu.Lock()
	target := x.a.targets[path]
	x.a.mu.Unlock()
	if target == nil {
		t.Fatal("missing native target")
	}
	return x.a.specOf(target)
}

func (x *desktopCodeSyncFixture) capture(t *testing.T, spec devicesync.SourceSpec) error {
	t.Helper()
	if err := x.fixture.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		return err
	}
	if auth == nil {
		return x.sy.Sync(ctx, spec)
	}
	defer auth.Release()
	return x.sy.SyncAuthorized(ctx, spec, auth)
}

func (x *desktopCodeSyncFixture) requests() int {
	x.server.Lock()
	defer x.server.Unlock()
	return x.server.Requests
}

func TestDesktopCodeScopedAuthorizationUploadsMatchedNativeFamily(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	main := desktopTranscript(t, x.fixture)
	sub, companion := coworkChildren(t, main)
	coworkWrite(t, sub, desktopRecord(x.fixture, "agent-cafe", "native-child", "scoped authorized child"))
	empty := filepath.Join(filepath.Dir(companion), "empty.txt")
	coworkWrite(t, empty, "")
	x.once()
	for _, path := range []string{main, sub, companion, empty} {
		spec := x.sourceSpec(t, path)
		if path == empty {
			if err := x.capture(t, spec); err != nil {
				t.Fatal(err)
			}
			var origin string
			if err := x.db.QueryRow(`SELECT protected_origin FROM devsync_sources WHERE path=?`, path).Scan(&origin); err != nil || origin != "desktop-code" {
				t.Fatal("empty companion lost source protection")
			}
			appendFile(t, empty, "later companion bytes")
			x.once()
		}
		if err := x.capture(t, spec); err != nil {
			t.Fatal(err)
		}
		id, err := transcript.StatIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		desc, gen, ok := x.server.Source(path, id.ID.String())
		if !ok || desc.SessionKey != spec.SessionKey {
			t.Fatal("native source identity lost")
		}
		got, err := x.server.Reconstruct(path, id.ID.String(), gen)
		want, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || !bytes.Equal(got, want) {
			t.Fatal("scoped native bytes did not reach synthetic server")
		}
		var origin, root string
		if err = x.db.QueryRow(`SELECT protected_origin,protected_root FROM devsync_sources WHERE path=?`, path).Scan(&origin, &root); err != nil || origin != "desktop-code" || root != x.cfg.DesktopCodeRoot {
			t.Fatal("missing durable Code boundary")
		}
	}
	if x.a.desktopCodeStatus().SharedHold != "" {
		t.Fatal("configured scoped authorization still advertised prerequisite hold")
	}
}

func TestDesktopCodeAuthorizationBoundsIndexedPrefixAndRejectsAppReclassification(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	main := desktopTranscript(t, x.fixture)
	appendFile(t, main, `{"unfinished":`)
	x.once()
	spec := x.sourceSpec(t, main)
	if err := x.fixture.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Proof.Offset >= auth.Proof.Identity.Size {
		auth.Release()
		t.Fatal("partial line was authorized")
	}
	bound := auth.Proof.Offset
	if err = x.sy.SyncAuthorized(ctx, spec, auth); err != nil {
		auth.Release()
		t.Fatal(err)
	}
	auth.Release()
	id, _ := transcript.StatIdentity(main)
	_, gen, _ := x.server.Source(main, id.ID.String())
	got, err := x.server.Reconstruct(main, id.ID.String(), gen)
	if err != nil || int64(len(got)) != bound {
		t.Fatal("unindexed tail shared")
	}
	before := x.requests()
	desktopMetadata(t, x.fixture, "claude-ai-chat")
	if err = x.capture(t, spec); err == nil {
		t.Fatal("ordinary chat reclassification authorized stale Code target")
	}
	if x.requests() != before {
		t.Fatal("reclassified source reached transport")
	}
}

func TestDesktopCodeAuthorizationRejectsSwappedCaptureAndRawRepair(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "repair"}[repair], func(t *testing.T) {
			x := newDesktopCodeSyncFixture(t)
			desktopMetadata(t, x.fixture, "")
			main := desktopTranscript(t, x.fixture)
			x.once()
			spec := x.sourceSpec(t, main)
			if repair {
				x.server.SetDown(true)
				if err := x.capture(t, spec); err == nil {
					t.Fatal("outage not captured")
				}
				var sid, gen int64
				if err := x.db.QueryRow(`SELECT id,generation FROM devsync_sources WHERE path=?`, main).Scan(&sid, &gen); err != nil {
					t.Fatal(err)
				}
				x.spool.DropTail(sid, gen)
				x.restartSync(t)
				x.server.SetDown(false)
				x.once()
			}
			if err := x.fixture.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			auth, err := x.a.authorizeDesktopCode(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			defer auth.Release()
			outside := filepath.Join(x.home, "outside.jsonl")
			coworkWrite(t, outside, desktopRecord(x.fixture, coworkNativeID, "outside", "outside protected sentinel"))
			if err = os.Remove(main); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(outside, main); err != nil {
				t.Fatal(err)
			}
			before := x.requests()
			if repair {
				err = x.sy.ResumeAuthorized(ctx, spec, auth)
			} else {
				err = x.sy.SyncAuthorized(ctx, spec, auth)
			}
			if err == nil {
				t.Fatal("swapped native source admitted")
			}
			if x.requests() != before {
				t.Fatal("swapped source reached transport")
			}
			if file, err := auth.Open(ctx, spec); err == nil {
				file.Close()
				t.Fatal("contained capture opener followed symlink")
			}
		})
	}
}

func TestDesktopCodeProtectedSourceSurvivesRootDisableAndProofGC(t *testing.T) {
	for _, disable := range []bool{true, false} {
		t.Run(map[bool]string{true: "disabled", false: "changed"}[disable], func(t *testing.T) {
			x := newDesktopCodeSyncFixture(t)
			desktopMetadata(t, x.fixture, "")
			main := desktopTranscript(t, x.fixture)
			x.once()
			spec := x.sourceSpec(t, main)
			if err := x.capture(t, spec); err != nil {
				t.Fatal(err)
			}
			// Retain an empty proof-less current generation after removing all
			// acknowledged historical proof rows, as after GC plus a crash.
			for _, stmt := range []string{`DELETE FROM devsync_manifest`, `UPDATE devsync_gens SET generation=1,size=0,entries=0,acked=0,closed=0,capture_proof=NULL`, `UPDATE devsync_sources SET generation=1,watermark=NULL`} {
				if _, err := x.db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			oldRoot := x.cfg.DesktopCodeRoot
			if disable {
				x.cfg.DesktopCodeRoot = "-"
			} else {
				x.cfg.DesktopCodeRoot = filepath.Join(x.home, "another-code-root")
			}
			if err := os.Remove(filepath.Join(oldRoot, "account", "org", desktopAppID+".json")); err != nil {
				t.Fatal(err)
			}
			x.restartSync(t)
			appendFile(t, main, desktopRecord(x.fixture, coworkNativeID, "after-root-change", "protected later bytes"))
			before := x.requests()
			if err := x.capture(t, spec); !errors.Is(err, devicesync.ErrUnprovenCapture) {
				t.Fatalf("durable Code protection lost: %v", err)
			}
			if x.requests() != before {
				t.Fatal("disabled Code root permitted ordinary capture")
			}
			var origin, root string
			if err := x.db.QueryRow(`SELECT protected_origin,protected_root FROM devsync_sources WHERE path=?`, main).Scan(&origin, &root); err != nil || origin != "desktop-code" || root != oldRoot {
				t.Fatal("GC/reset weakened source origin")
			}
		})
	}
}

func TestDesktopCodeLeaseRejectsFolderRuleChangeBeforeTransport(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	main := desktopTranscript(t, x.fixture)
	x.once()
	if err := x.fixture.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	spec := x.sourceSpec(t, main)
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Release()
	x.a.cfg.UserRules = filepath.Join(x.home, "rules")
	coworkWrite(t, x.a.cfg.UserRules, "deny "+filepath.Join(x.home, "repo")+"\n")
	before := x.requests()
	if err = x.sy.SyncAuthorized(context.Background(), spec, auth); err == nil {
		t.Fatal("changed folder rules accepted stale lease")
	}
	if x.requests() != before {
		t.Fatal("stale policy reached transport")
	}
}

func TestDesktopCodeAuthorizationRejectsUnprovenHistoricalGenerations(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "acknowledged"}[pending], func(t *testing.T) {
			x := newDesktopCodeSyncFixture(t)
			desktopMetadata(t, x.fixture, "")
			main := desktopTranscript(t, x.fixture)
			x.once()
			if err := x.fixture.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			spec := x.sourceSpec(t, main)
			// Synthetic legacy capture deliberately bypasses the agent's new
			// lease to establish a generation with no indexed authorization.
			x.server.SetDown(pending)
			if err := x.sy.Sync(ctx, spec); (err != nil) != pending {
				t.Fatalf("legacy fixture capture: %v", err)
			}
			x.server.SetDown(false)
			before := x.requests()
			if err := x.capture(t, spec); !errors.Is(err, devicesync.ErrUnprovenCapture) {
				t.Fatalf("legacy generation accepted: %v", err)
			}
			if x.requests() != before {
				t.Fatal("unproven historical bytes reached transport")
			}
		})
	}
}

func TestDesktopCodeAuthorizationRejectsAncestorSymlinkSwap(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	main := desktopTranscript(t, x.fixture)
	x.once()
	if err := x.fixture.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	spec := x.sourceSpec(t, main)
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Release()
	project := filepath.Dir(main)
	retained := filepath.Join(x.home, "retained-project")
	if err = os.Rename(project, retained); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(x.home, "outside-project")
	coworkWrite(t, filepath.Join(outside, filepath.Base(main)), desktopRecord(x.fixture, coworkNativeID, "outside", "ancestor swap sentinel"))
	if err = os.Symlink(outside, project); err != nil {
		t.Fatal(err)
	}
	before := x.requests()
	if err = x.sy.SyncAuthorized(ctx, spec, auth); err == nil {
		t.Fatal("ancestor symlink granted scoped lease")
	}
	if x.requests() != before {
		t.Fatal("ancestor swap reached transport")
	}
	if file, err := auth.Open(ctx, spec); err == nil {
		file.Close()
		t.Fatal("contained source opener followed ancestor symlink")
	}
}

func TestDesktopCodeDispatcherRechecksHistoricalReadFailureForOrdinaryCLI(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	path := desktopCLI(t, x.fixture)
	x.once()
	spec := x.sourceSpec(t, path)
	if !x.a.allowUpload(spec) {
		t.Fatal("ordinary CLI fixture must initially pass scheduler filter")
	}
	if err := x.fixture.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := x.db.Exec(`CREATE TABLE cowork_history(unrelated_column TEXT)`); err != nil {
		t.Fatal(err)
	}
	before := x.requests()
	if err := x.capture(t, spec); !errors.Is(err, errDesktopCodeHeld) {
		t.Fatalf("fresh historical read failure must hold queued ordinary source: %v", err)
	}
	if x.requests() != before {
		t.Fatal("historical read failure reached transport")
	}
}

func TestDesktopCodeAuthorizationRequiresCurrentIndexedParser(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	path := desktopTranscript(t, x.fixture)
	x.once()
	spec := x.sourceSpec(t, path)
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	x.a.mu.Lock()
	target := x.a.targets[path]
	target.indexedWith = "claude@0.1"
	x.a.mu.Unlock()
	before := x.requests()
	if err := x.sy.SyncAuthorized(ctx, spec, auth); !errors.Is(err, errDesktopCodeHeld) {
		t.Fatalf("retained old indexed parser must invalidate existing lease: %v", err)
	}
	auth.Release()
	if _, err := x.a.authorizeDesktopCode(ctx, spec); !errors.Is(err, errDesktopCodeHeld) {
		t.Fatalf("retained old indexed parser must reject fresh authorization: %v", err)
	}
	if x.requests() != before {
		t.Fatal("stale extraction reached transport")
	}
}

func TestDesktopCodeActiveLeaseBlocksPolicyPublication(t *testing.T) {
	x := newDesktopCodeSyncFixture(t)
	desktopMetadata(t, x.fixture, "")
	path := desktopTranscript(t, x.fixture)
	x.once()
	spec := x.sourceSpec(t, path)
	auth, err := x.a.authorizeDesktopCode(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	fetched, done := make(chan struct{}), make(chan struct{})
	x.a.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
		close(fetched)
		return AdminPolicy{Unplaceable: "exclude"}, nil
	}
	prior := x.a.policy().key
	go func() {
		x.a.refreshPolicy(ctx, true)
		close(done)
	}()
	<-fetched
	select {
	case <-done:
		auth.Release()
		t.Fatal("rule publication completed while capture lease was active")
	case <-time.After(30 * time.Millisecond):
	}
	if x.a.policy().key != prior {
		auth.Release()
		t.Fatal("rule snapshot changed during active capture lease")
	}
	auth.Release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rule publication did not resume after lease release")
	}
	if x.a.policy().key == prior {
		t.Fatal("admin rule change was not published")
	}
}
