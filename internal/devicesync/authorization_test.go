package devicesync

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type authTransport struct {
	syncproto.Transport
	has, flush int
	onHas      func()
	onFlush    func()
	fail       bool
}

func (t *authTransport) Has(ctx context.Context, h []syncproto.Hash) ([]syncproto.Hash, error) {
	t.has++
	if t.onHas != nil {
		t.onHas()
	}
	if t.fail {
		return nil, errors.New("synthetic outage")
	}
	return t.Transport.Has(ctx, h)
}
func (t *authTransport) Flush(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
	t.flush++
	if t.onFlush != nil {
		t.onFlush()
	}
	if t.fail {
		return nil, errors.New("synthetic outage")
	}
	return t.Transport.Flush(ctx, r)
}

func authorizedSpec(e *env, name string, kind transcript.StorageKind) SourceSpec {
	sp := e.spec(name, kind)
	sp.Agent, sp.Parser = transcript.AgentClaude, "claude@4.1"
	return sp
}

func fileAuthorization(t *testing.T, sp SourceSpec, bound int64) *CaptureAuthorization {
	t.Helper()
	id, err := transcript.StatIdentity(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	return &CaptureAuthorization{Origin: "cowork", Root: filepath.Dir(sp.Path), Proof: CaptureProof{PolicyRequestDigest: strings.Repeat("a", 64), Identity: id, Offset: bound}, Open: func(ctx context.Context, s SourceSpec) (*os.File, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fi, err := os.Lstat(s.Path)
		if err != nil {
			return nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, errors.New("source is not a contained regular file")
		}
		return os.Open(s.Path)
	}, Check: func(context.Context) error { return nil }, Release: func() {}}
}

func TestAuthorizedCaptureRejectsLegacyPendingDespiteLowerBound(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "legacy.jsonl", transcript.StorageJSONLAppend)
	sp.SessionKey = "legacy-session"
	data := jsonlLines(80, 80, 300)
	appendFile(t, sp.Path, data)
	tr := &authTransport{Transport: e.client, fail: true}
	e.sy.tr = tr
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("expected initial outage")
	}
	before := tr.flush + tr.has
	a := fileAuthorization(t, sp, 0)
	err := e.sy.SyncAuthorized(context.Background(), sp, a)
	var unproven *UnprovenCaptureError
	if !errors.As(err, &unproven) || !errors.Is(err, ErrUnprovenCapture) || unproven.Source.SessionKey != "legacy-session" || unproven.SourceID <= 0 || !unproven.Captured {
		t.Fatalf("legacy pending error=%v", err)
	}
	if tr.flush+tr.has != before {
		t.Fatal("legacy bytes reached transport through bound-noop")
	}
}

func TestAuthorizedCapturePersistsProofAcrossRestart(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "controlled.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(81, 80, 300)
	appendFile(t, sp.Path, data)
	tr := &authTransport{Transport: e.client, fail: true}
	e.sy.tr = tr
	a := fileAuthorization(t, sp, int64(len(data)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected outage")
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.store.gen(context.Background(), src.ID, src.Gen)
	if err != nil || g.Proof == nil || g.Proof.Offset != int64(len(data)) {
		t.Fatalf("persisted proof=%+v,%v", g, err)
	}
	e.sy.Close()
	sy, err := NewSyncer(e.sy.cfg, e.store, e.spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	if err := sy.Resume(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("proof without lease resumed: %v", err)
	}
	// The strictest full union may have a newer digest than the original capture.
	a.Proof.PolicyRequestDigest = strings.Repeat("b", 64)
	if err := sy.ResumeAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, fileID(a.Proof.Identity), src.Gen, data)
}

func TestAuthorizedCaptureAlwaysBoundsPrefix(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "prefix.jsonl", transcript.StorageJSONLAppend)
	first := jsonlLines(82, 30, 100)
	all := append(append([]byte{}, first...), jsonlLines(83, 30, 100)...)
	appendFile(t, sp.Path, all)
	a := fileAuthorization(t, sp, int64(len(first)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, fileID(a.Proof.Identity), 0, first)
}

func TestAuthorizedCaptureRejectsSourceSwapAndRawRepairSwap(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture", true: "repair"}[repair], func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "source.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(84, 80, 300)
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			tr := &authTransport{Transport: e.client, fail: true}
			e.sy.tr = tr
			if repair {
				if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
					t.Fatal("expected outage")
				}
			}
			other := e.path("outside.jsonl")
			appendFile(t, other, data)
			if err := os.Remove(sp.Path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, sp.Path); err != nil {
				t.Fatal(err)
			}
			before := tr.has + tr.flush
			tr.fail = false
			var err error
			if repair {
				err = e.sy.ResumeAuthorized(context.Background(), sp, a)
			} else {
				err = e.sy.SyncAuthorized(context.Background(), sp, a)
			}
			if err == nil {
				t.Fatal("source swap accepted")
			}
			if tr.has+tr.flush != before {
				t.Fatal("swapped raw source reached transport")
			}
		})
	}
}

func TestAuthorizedCaptureRejectsWriteBeforeCaptureCommit(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "race.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(85, 50, 300)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	checks := 0
	a.Check = func(context.Context) error {
		checks++
		if checks == 3 {
			appendFile(t, sp.Path, []byte("mutated\n"))
		}
		return nil
	}
	tr := &authTransport{Transport: e.client}
	e.sy.tr = tr
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("capture commit race error=%v checks=%d", err, checks)
	}
	if tr.has+tr.flush != 0 {
		t.Fatal("racy captured bytes uploaded")
	}
	var gens int
	if err := e.store.db.QueryRow(`SELECT count(*) FROM devsync_gens`).Scan(&gens); err != nil || gens != 0 {
		t.Fatalf("racy generation committed=%d,%v", gens, err)
	}
}

func TestAuthorizedCompanionRequiresRawDigest(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "mismatch"}[wrong], func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "result.txt", transcript.StorageCompanion)
			data := []byte("synthetic companion bytes")
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			if wrong {
				sum := sha256.Sum256([]byte("wrong bytes"))
				a.Proof.ContentSHA = sum[:]
			}
			tr := &authTransport{Transport: e.client}
			e.sy.tr = tr
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
				t.Fatal("companion without correct raw hash accepted")
			}
			if tr.has+tr.flush != 0 {
				t.Fatal("unverified companion reached transport")
			}
		})
	}
}

func TestAuthorizedCaptureRechecksBeforeNetworkHandover(t *testing.T) {
	e := newEnv(t, Config{HasThreshold: 1}, 1<<20)
	sp := authorizedSpec(e, "network.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(86, 80, 300)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	changed := false
	policyErr := errors.New("synthetic policy union changed")
	a.Check = func(context.Context) error {
		if changed {
			return policyErr
		}
		return nil
	}
	tr := &authTransport{Transport: e.client, onHas: func() { changed = true }}
	e.sy.tr = tr
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, policyErr) {
		t.Fatalf("policy change error=%v", err)
	}
	if tr.has != 1 || tr.flush != 0 {
		t.Fatalf("network handovers: has=%d flush=%d", tr.has, tr.flush)
	}
}

func TestSchedulerAuthorizationReleasesEveryPath(t *testing.T) {
	for _, kind := range []string{"acquire error", "check error", "network error", "export rejection", "success"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "lease.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(87, 20, 100)
			appendFile(t, sp.Path, data)
			sc := NewScheduler(e.sy, SchedulerConfig{})
			a := fileAuthorization(t, sp, int64(len(data)))
			released := 0
			notified := 0
			a.OnError = func(_ context.Context, err error) {
				notified++
				if released != 0 {
					t.Error("error callback ran after lease release")
				}
				e.sy.mu.Lock()
				e.sy.mu.Unlock()
			}
			a.Release = func() { released++ }
			var acquireErr error
			switch kind {
			case "acquire error":
				acquireErr = errors.New("synthetic acquire error")
			case "check error":
				a.Check = func(context.Context) error { return errors.New("synthetic check error") }
			case "network error":
				e.sy.tr = &authTransport{Transport: e.client, fail: true}
			case "export rejection":
				sp.Export = true
			}
			fn := func(context.Context, SourceSpec) (*CaptureAuthorization, error) { return a, acquireErr }
			err := sc.syncJob(context.Background(), &job{spec: sp}, 0, false, fn)
			if kind == "success" && err != nil {
				t.Fatal(err)
			}
			if kind != "success" && err == nil {
				t.Fatal("expected branch error")
			}
			if kind == "success" && notified != 0 || kind != "success" && notified != 1 {
				t.Errorf("error notifications=%d", notified)
			}
			if released != 1 {
				t.Fatalf("lease releases=%d", released)
			}
		})
	}
}

func TestAuthorizedCompanionRawDigestPersistsAndUploads(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "verified.txt", transcript.StorageCompanion)
	data := []byte("synthetic companion verified bytes")
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	sum := sha256.Sum256(data)
	a.Proof.ContentSHA = sum[:]
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.store.gen(context.Background(), src.ID, src.Gen)
	if err != nil || g.Proof == nil || string(g.Proof.ContentSHA) != string(sum[:]) {
		t.Fatalf("companion proof missing: %+v,%v", g, err)
	}
	e.requireServerHas(sp.Path, fileID(a.Proof.Identity), 0, data)
}

func TestAuthorizedCaptureRejectsWriteAfterRawMaterialization(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "handover.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(90, 50, 300)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	checks := 0
	a.Check = func(context.Context) error {
		checks++
		if checks == 6 {
			appendFile(t, sp.Path, []byte("changed before network\n"))
		}
		return nil
	}
	tr := &authTransport{Transport: e.client}
	e.sy.tr = tr
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("materialization race error=%v checks=%d", err, checks)
	}
	if tr.has+tr.flush != 0 {
		t.Fatal("changed descriptor handed to network")
	}
}

func TestAuthorizedCaptureChecksEveryPendingGeneration(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "generations.jsonl", transcript.StorageJSONLAppend)
	first := jsonlLines(91, 50, 300)
	appendFile(t, sp.Path, first)
	tr := &authTransport{Transport: e.client, fail: true}
	e.sy.tr = tr
	a := fileAuthorization(t, sp, int64(len(first)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected outage")
	}
	second := jsonlLines(92, 60, 300)
	if err := os.WriteFile(sp.Path, second, 0600); err != nil {
		t.Fatal(err)
	}
	a = fileAuthorization(t, sp, int64(len(second)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected second outage")
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.Gen != 1 {
		t.Fatalf("latest generation=%d", src.Gen)
	}
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=NULL WHERE source_id=? AND generation=0`, src.ID); err != nil {
		t.Fatal(err)
	}
	before := tr.has + tr.flush
	// The latest generation proof and a zero fresh bound cannot qualify gen 0.
	a.Proof.Offset = 0
	var unproven *UnprovenCaptureError
	err = e.sy.SyncAuthorized(context.Background(), sp, a)
	if !errors.As(err, &unproven) || unproven.Generation != 0 {
		t.Fatalf("old generation not independently checked: %v", err)
	}
	if tr.has+tr.flush != before {
		t.Fatal("old unproven generation sent under latest proof")
	}
}

func TestCaptureProofMigrationPreservesLegacyHold(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "migration.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(93, 20, 100)
	appendFile(t, sp.Path, data)
	e.sy.tr = &authTransport{Transport: e.client, fail: true}
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("expected outage")
	}
	if _, err := e.store.db.Exec(`ALTER TABLE devsync_gens DROP COLUMN capture_proof`); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewStore(e.store.db)
	if err != nil {
		t.Fatal(err)
	}
	e.sy.store = migrated
	src, err := migrated.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := migrated.gen(context.Background(), src.ID, src.Gen)
	if err != nil || g == nil || g.Proof != nil || g.Size == 0 {
		t.Fatalf("migration lost legacy evidence: %+v,%v", g, err)
	}
	a := fileAuthorization(t, sp, 0)
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("migration grandfathered old evidence: %v", err)
	}
}

func TestNewAuthorizationFailureDoesNotClaimHistoricalCapture(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "new.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(94, 5, 100))
	a := fileAuthorization(t, sp, 0)
	a.Proof.PolicyRequestDigest = ""
	var unproven *UnprovenCaptureError
	err := e.sy.SyncAuthorized(context.Background(), sp, a)
	if !errors.As(err, &unproven) || unproven.Captured {
		t.Fatalf("new authorization error became historical taint: %v", err)
	}
	var gens int
	if err := e.store.db.QueryRow(`SELECT count(*) FROM devsync_gens`).Scan(&gens); err != nil || gens != 0 {
		t.Fatalf("invalid new authorization committed captures: %d,%v", gens, err)
	}
}

func TestCapturedProofCannotFallBackToUnauthorisedRewrite(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "protected.jsonl", transcript.StorageJSONLAppend)
	first := jsonlLines(95, 20, 100)
	appendFile(t, sp.Path, first)
	a := fileAuthorization(t, sp, int64(len(first)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sp.Path, jsonlLines(96, 30, 200), 0600); err != nil {
		t.Fatal(err)
	}
	tr := &authTransport{Transport: e.client}
	e.sy.tr = tr
	if err := e.sy.Sync(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("fully acknowledged source lost authorization on rewrite: %v", err)
	}
	if tr.has+tr.flush != 0 {
		t.Fatal("protected rewrite used ordinary collector path")
	}
	var gens int
	if err := e.store.db.QueryRow(`SELECT count(*) FROM devsync_gens`).Scan(&gens); err != nil || gens != 1 {
		t.Fatalf("unauthorized rewrite committed: %d,%v", gens, err)
	}
}

func TestSchedulerWithoutAuthorizerCannotResumeProtectedPendingAfterRestart(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "restart-protected.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(104, 30, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	e.sy.tr = &authTransport{Transport: e.client, fail: true}
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected outage")
	}
	e.sy.Close()
	tr := &authTransport{Transport: e.client}
	sy, err := NewSyncer(e.sy.cfg, e.store, e.spool, tr)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	sc := NewScheduler(sy, SchedulerConfig{}) // no authorizer installed after restart/root change
	// Make any raw repair unsafe as well; persisted proof must block before it.
	other := e.path("outside-restart.jsonl")
	appendFile(t, other, data)
	if err := os.Remove(sp.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, sp.Path); err != nil {
		t.Fatal(err)
	}
	err = sc.syncJob(context.Background(), &job{spec: sp}, 0, false, nil)
	if !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("restart without authorizer error=%v", err)
	}
	if tr.has+tr.flush != 0 {
		t.Fatal("restart/root change uploaded protected pending bytes")
	}
}

func TestSourceProtectionSurvivesAcknowledgedGenerationGCAndEmptyFailure(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "durable-origin.jsonl", transcript.StorageJSONLAppend)
	first := jsonlLines(105, 20, 100)
	appendFile(t, sp.Path, first)
	a := fileAuthorization(t, sp, int64(len(first)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	// A next empty generation closes the acknowledged generation. Simulate an
	// old failed-materialization crash state with no proof on that empty row.
	if err := os.WriteFile(sp.Path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	a = fileAuthorization(t, sp, 0)
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src.Gen != 1 {
		t.Fatalf("empty next generation=%d", src.Gen)
	}
	if _, err := e.store.db.Exec(`DELETE FROM devsync_manifest WHERE source_id=? AND generation=0`, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`DELETE FROM devsync_gens WHERE source_id=? AND generation=0 AND closed=1 AND acked=entries AND tail_acked=1`, src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=NULL WHERE source_id=? AND generation=1`, src.ID); err != nil {
		t.Fatal(err)
	}
	var proofs int
	if err := e.store.db.QueryRow(`SELECT count(*) FROM devsync_gens WHERE capture_proof IS NOT NULL`).Scan(&proofs); err != nil || proofs != 0 {
		t.Fatalf("expected GC removed all proof rows: %d,%v", proofs, err)
	}
	// A failed attempt to capture new bytes cannot weaken the durable origin.
	later := jsonlLines(106, 30, 200)
	appendFile(t, sp.Path, later)
	a = fileAuthorization(t, sp, int64(len(later)))
	a.Open = func(context.Context, SourceSpec) (*os.File, error) { return nil, os.ErrNotExist }
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected new capture failure")
	}
	e.sy.Close()
	tr := &authTransport{Transport: e.client}
	sy, err := NewSyncer(e.sy.cfg, e.store, e.spool, tr)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	sc := NewScheduler(sy, SchedulerConfig{})
	sp.Checkout = "/new/spec/value" // Updating SourceSpec must preserve protection.
	err = sc.syncJob(context.Background(), &job{spec: sp}, 0, false, nil)
	if !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("GC/emptyfailure lost durable source protection: %v", err)
	}
	if tr.has+tr.flush != 0 {
		t.Fatal("unbounded ordinary capture escaped durable source marker")
	}
	src, err = e.store.source(context.Background(), sp.Path, nil)
	if err != nil || src.ProtectedOrigin != "cowork" || src.ProtectedRoot != filepath.Dir(sp.Path) {
		t.Fatalf("durable source marker=%+v,%v", src, err)
	}
}

func TestSourceProtectionRejectsOriginOrRootReplacement(t *testing.T) {
	for _, kind := range []string{"origin", "root"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "boundary.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(107, 20, 100)
			appendFile(t, sp.Path, data)
			a := fileAuthorization(t, sp, int64(len(data)))
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
				t.Fatal(err)
			}
			a = fileAuthorization(t, sp, int64(len(data)))
			if kind == "origin" {
				a.Origin = "desktop-code"
			} else {
				a.Root = t.TempDir()
			}
			opened := false
			a.Open = func(context.Context, SourceSpec) (*os.File, error) { opened = true; return os.Open(sp.Path) }
			tr := &authTransport{Transport: e.client}
			e.sy.tr = tr
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, ErrProtectionMismatch) {
				t.Fatalf("protection replacement accepted: %v", err)
			}
			if opened || tr.has+tr.flush != 0 {
				t.Fatal("replacement boundary reached capture/network")
			}
		})
	}
}

func TestSourceProtectionPersistsBeforeFirstCaptureFailure(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "never-captured.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(108, 10, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	a.Open = func(context.Context, SourceSpec) (*os.File, error) { return nil, os.ErrNotExist }
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected contained open failure")
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil || src.Gen != -1 || src.ProtectedOrigin != "cowork" {
		t.Fatalf("source marker not durable before failed capture: %+v,%v", src, err)
	}
	if err := e.sy.Sync(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("first capture failure escaped ordinary next attempt: %v", err)
	}
}

func TestSourceProtectionMigrationMarksExistingQualifiedCaptures(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "old-qualified.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(109, 10, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`ALTER TABLE devsync_sources DROP COLUMN protected_origin`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`ALTER TABLE devsync_sources DROP COLUMN protected_root`); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewStore(e.store.db)
	if err != nil {
		t.Fatal(err)
	}
	src, err := migrated.source(context.Background(), sp.Path, nil)
	if err != nil || src.ProtectedOrigin != a.Origin || src.ProtectedRoot != a.Root {
		t.Fatalf("qualified-source migration marker=%+v,%v", src, err)
	}
	// Proofs predating root fields receive an unknown protected origin, not
	// an ordinary source permission.
	if _, err := e.store.db.Exec(`UPDATE devsync_sources SET protected_origin='',protected_root=''`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET capture_proof=json_remove(capture_proof,'$.Origin','$.Root')`); err != nil {
		t.Fatal(err)
	}
	migrated, err = NewStore(e.store.db)
	if err != nil {
		t.Fatal(err)
	}
	e.sy.store = migrated
	src, err = migrated.source(context.Background(), sp.Path, nil)
	if err != nil || src.ProtectedOrigin != "legacy-qualified" {
		t.Fatalf("old-proof missing-root migration escaped protection: %+v,%v", src, err)
	}
	if err := e.sy.Sync(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
		t.Fatalf("unknown migrated root uploaded ordinarily: %v", err)
	}
}

func TestProtectedSourceCannotRebindCapturedNativeAssociation(t *testing.T) {
	for _, kind := range []string{"session", "agent", "parent", "storage", "export", "parser namespace", "empty parser", "invalid parser", "cass parser"} {
		for _, resume := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/capture", true: "/resume"}[resume], func(t *testing.T) {
				e := newEnv(t, Config{}, 1<<20)
				sp := authorizedSpec(e, "immutable-association.jsonl", transcript.StorageJSONLAppend)
				sp.Agent, sp.Parser = transcript.AgentClaude, "claude@4.1"
				sp.SessionKey = "original-session"
				data := jsonlLines(110, 30, 100)
				appendFile(t, sp.Path, data)
				a := fileAuthorization(t, sp, int64(len(data)))
				tr := &authTransport{Transport: e.client, fail: true}
				e.sy.tr = tr
				if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
					t.Fatal("expected outage")
				}
				changed := sp
				switch kind {
				case "session":
					changed.SessionKey = "different-session"
				case "agent":
					changed.Agent = transcript.AgentCodex
				case "parent":
					changed.Parent = e.path("different-parent.jsonl")
				case "storage":
					changed.StorageKind = transcript.StorageJSONDoc
				case "export":
					changed.Export = true
				case "parser namespace":
					changed.Parser = "codex@1"
				case "empty parser":
					changed.Parser = ""
				case "invalid parser":
					changed.Parser = "claude@0.1"
				case "cass parser":
					changed.Parser = "cass@1"
				}
				before := tr.has + tr.flush
				var err error
				if resume {
					err = e.sy.ResumeAuthorized(context.Background(), changed, a)
				} else {
					err = e.sy.SyncAuthorized(context.Background(), changed, a)
				}
				if !errors.Is(err, ErrProtectionMismatch) {
					t.Fatalf("native reassociation accepted: %v", err)
				}
				src, loadErr := e.store.source(context.Background(), sp.Path, nil)
				if loadErr != nil || src.Spec != sp {
					t.Fatalf("protected association overwritten: %+v,%v", src, loadErr)
				}
				if tr.has+tr.flush != before {
					t.Fatal("reassociated bytes reached transport")
				}
			})
		}
	}
}

func TestProtectedSourceAssociationSurvivesProofGC(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "association-after-gc.jsonl", transcript.StorageJSONLAppend)
	sp.Agent, sp.Parser = transcript.AgentClaude, "claude@4.1"
	sp.SessionKey = "original-session"
	data := jsonlLines(111, 20, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`DELETE FROM devsync_manifest`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.db.Exec(`DELETE FROM devsync_gens`); err != nil {
		t.Fatal(err)
	}
	e.sy.Close()
	sy, err := NewSyncer(e.sy.cfg, e.store, e.spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	changed := sp
	changed.SessionKey = "new-session"
	if err := sy.SyncAuthorized(context.Background(), changed, a); !errors.Is(err, ErrProtectionMismatch) {
		t.Fatalf("GC allowed native association replacement: %v", err)
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil || src.Spec.SessionKey != "original-session" {
		t.Fatalf("GC lost protected native association: %+v,%v", src, err)
	}
}

func TestProtectedSourceAllowsRepositoryMetadataWithoutIdentityChanges(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "repo-update.jsonl", transcript.StorageJSONLAppend)
	sp.Agent, sp.Parser = transcript.AgentClaude, "claude@4.1"
	data := jsonlLines(112, 20, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	sp.Checkout = "/synthetic/main-repository"
	sp.Remote = "github.com/synthetic/repository"
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
		t.Fatal(err)
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil || src.Spec.Checkout != sp.Checkout || src.Spec.Remote != sp.Remote {
		t.Fatalf("repository metadata not updated: %+v,%v", src, err)
	}
}

func TestProtectedEmptyKeyCannotBeRecertifiedAsNativeGrant(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "empty-key.jsonl", transcript.StorageJSONLAppend)
	sp.Agent, sp.Parser = transcript.AgentClaude, "claude@4.1"
	data := jsonlLines(113, 10, 100)
	appendFile(t, sp.Path, data)
	a := fileAuthorization(t, sp, int64(len(data)))
	e.sy.tr = &authTransport{Transport: e.client, fail: true}
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); err == nil {
		t.Fatal("expected outage")
	}
	sp.SessionKey = "new-native-key"
	if err := e.sy.SyncAuthorized(context.Background(), sp, a); !errors.Is(err, ErrProtectionMismatch) {
		t.Fatalf("empty-key bytes recertified under native key: %v", err)
	}
}

func TestProtectedSourceAllowsNativeParserVersionUpgrade(t *testing.T) {
	for _, parser := range []string{"claude@4.2", "claude@5.0"} {
		t.Run(parser, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "parser-upgrade.jsonl", transcript.StorageJSONLAppend)
			sp.Agent, sp.Parser, sp.SessionKey = transcript.AgentClaude, "claude@4.1", "same-owner"
			data := jsonlLines(114, 20, 100)
			appendFile(t, sp.Path, data)
			tr := &authTransport{Transport: e.client, fail: true}
			e.sy.tr = tr
			if err := e.sy.SyncAuthorized(context.Background(), sp, fileAuthorization(t, sp, int64(len(data)))); err == nil {
				t.Fatal("expected outage")
			}
			src, err := e.store.source(context.Background(), sp.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			prior, err := e.store.gen(context.Background(), src.ID, src.Gen)
			if err != nil {
				t.Fatal(err)
			}
			sp.Parser = parser
			tr.fail = false
			if err := e.sy.ResumeAuthorized(context.Background(), sp, fileAuthorization(t, sp, int64(len(data)))); err != nil {
				t.Fatal(err)
			}
			src, err = e.store.source(context.Background(), sp.Path, nil)
			if err != nil || src.Spec.Parser != parser || src.Spec.SessionKey != "same-owner" {
				t.Fatalf("upgrade association: %+v %v", src, err)
			}
			current, err := e.store.gen(context.Background(), src.ID, prior.Gen)
			if err != nil {
				t.Fatal(err)
			}
			if current != nil && (current.FileID != prior.FileID || current.Proof.PolicyRequestDigest != prior.Proof.PolicyRequestDigest) {
				t.Fatal("old capture ownership changed")
			}
		})
	}
}

func TestInitialAuthorizationRejectsNonNativeSources(t *testing.T) {
	for _, kind := range []string{"agent", "empty", "cass", "family", "version", "storage", "export"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "initial.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(115, 10, 100)
			appendFile(t, sp.Path, data)
			switch kind {
			case "agent":
				sp.Agent = transcript.AgentCodex
			case "empty":
				sp.Parser = ""
			case "cass":
				sp.Parser = "cass@1"
			case "family":
				sp.Parser = "codex@1"
			case "version":
				sp.Parser = "claude@invalid"
			case "storage":
				sp.StorageKind = transcript.StorageJSONDoc
			case "export":
				sp.Export = true
			}
			tr := &authTransport{Transport: e.client}
			e.sy.tr = tr
			err := e.sy.SyncAuthorized(context.Background(), sp, fileAuthorization(t, sp, int64(len(data))))
			var unproven *UnprovenCaptureError
			if !errors.As(err, &unproven) || unproven.Captured {
				t.Fatalf("invalid first authorization: %v", err)
			}
			src, err := e.store.source(context.Background(), sp.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if src.ProtectedOrigin != "" || tr.has+tr.flush != 0 {
				t.Fatal("invalid native source acquired protection or sent bytes")
			}
		})
	}
}

func TestAuthorizedCaptureQualifiesOrdinaryEmptyCurrentGeneration(t *testing.T) {
	for _, appendBytes := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged empty", true: "append"}[appendBytes], func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			sp := authorizedSpec(e, "empty-then-native.jsonl", transcript.StorageJSONLAppend)
			sp.SessionKey = "ordinary-empty-owner"
			appendFile(t, sp.Path, nil)
			if err := e.sy.Sync(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			src, err := e.store.source(context.Background(), sp.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			old, err := e.store.gen(context.Background(), src.ID, src.Gen)
			if err != nil {
				t.Fatal(err)
			}
			if old == nil || old.Size != 0 || old.Proof != nil {
				t.Fatalf("ordinary empty fixture: %+v", old)
			}
			var data []byte
			if appendBytes {
				data = jsonlLines(117, 20, 100)
				appendFile(t, sp.Path, data)
			}
			a := fileAuthorization(t, sp, int64(len(data)))
			tr := &authTransport{Transport: e.client}
			e.sy.tr = tr
			// Resume must hold until capture qualifies the empty generation, and must
			// not label this zero-byte history as captured evidence requiring taint.
			err = e.sy.ResumeAuthorized(context.Background(), sp, a)
			var hold *UnprovenCaptureError
			if !errors.As(err, &hold) || hold.Captured || tr.has+tr.flush != 0 {
				t.Fatalf("empty resume bypass: %v transport=%d", err, tr.has+tr.flush)
			}
			if err := e.sy.Sync(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
				t.Fatalf("protected ordinary fallback: %v", err)
			}
			if err := e.sy.SyncAuthorized(context.Background(), sp, a); err != nil {
				t.Fatal(err)
			}
			src, err = e.store.source(context.Background(), sp.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			current, err := e.store.gen(context.Background(), src.ID, src.Gen)
			if err != nil {
				t.Fatal(err)
			}
			if current == nil || current.Gen <= old.Gen || !generationProofValid(sp, current) || current.Size != int64(len(data)) {
				t.Fatalf("new proved capture: %+v", current)
			}
			if err := e.sy.ResumeAuthorized(context.Background(), sp, a); err != nil {
				t.Fatal(err)
			}
			if err := e.sy.Sync(context.Background(), sp); !errors.Is(err, ErrUnprovenCapture) {
				t.Fatalf("proved source lost ordinary hold: %v", err)
			}
		})
	}
}

func TestAuthorizedEmptyCurrentAllowanceDoesNotQualifyNonemptyPendingHistory(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := authorizedSpec(e, "nonempty-history.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(118, 20, 100)
	appendFile(t, sp.Path, data)
	tr := &authTransport{Transport: e.client, fail: true}
	e.sy.tr = tr
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("expected pending outage")
	}
	src, err := e.store.source(context.Background(), sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Synthesize an empty current generation while an older generation retains
	// pending bytes: the current can be empty, but its history is not.
	oldGeneration := src.Gen
	if err = e.store.saveCapture(context.Background(), src, &genRow{SourceID: src.ID, Gen: src.Gen + 1, FileID: "empty-current", TailAcked: true}, nil, src.Watermark, nil); err != nil {
		t.Fatal(err)
	}
	before := tr.has + tr.flush
	err = e.sy.SyncAuthorized(context.Background(), sp, fileAuthorization(t, sp, int64(len(data))))
	var hold *UnprovenCaptureError
	if !errors.As(err, &hold) || !hold.Captured || hold.Generation != oldGeneration || tr.has+tr.flush != before {
		t.Fatalf("pending legacy bytes recertified: %v", err)
	}
}
