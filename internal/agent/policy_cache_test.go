package agent

import (
	"context"
	"errors"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAdminCacheValidityAndOwnership(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "admin.json")
	cfg := Config{PolicyAuthority: &PolicyAuthority{Server: "https://a.example", DeviceID: "device-a"}, AdminRulesCache: cache}
	cfg.defaults()
	a := &Agent{cfg: cfg, log: cfg.Logger}
	if got := a.readAdminCache(); got.state != adminCacheAbsent {
		t.Fatalf("missing cache = %v", got.state)
	}
	if err := a.writeAdminCache(AdminPolicy{Rules: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got := a.readAdminCache(); got.state != adminCacheValid {
		t.Fatalf("valid empty policy = %v: %v", got.state, got.err)
	}
	a.cfg.PolicyAuthority.Server = "https://b.example"
	if got := a.readAdminCache(); got.state == adminCacheValid {
		t.Fatal("A's policy accepted for B")
	}
	a.cfg.PolicyAuthority.Server = "https://a.example/"
	if got := a.readAdminCache(); got.state != adminCacheValid {
		t.Fatalf("normalized same authority = %v", got.state)
	}
	a.cfg.PolicyAuthority.DeviceID = "device-b"
	if got := a.readAdminCache(); got.state == adminCacheValid {
		t.Fatal("another device's policy accepted")
	}
	if err := os.WriteFile(a.adminCachePath(), []byte(`{"rules":`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := a.readAdminCache(); got.state != adminCacheUnreadable {
		t.Fatalf("corrupt cache = %v", got.state)
	}
}

func TestUnknownAdminPolicyPausesCollectionWithoutPurging(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "allow /tmp/**")
	f.once()
	before := f.count(`SELECT count(*) FROM conversations`)
	cache := filepath.Join(t.TempDir(), "policy.json")
	f.cfg.AdminRulesCache = cache
	f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "device-a"}
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{}, errors.New("offline") }
	ca := &Agent{cfg: f.cfg}
	path := ca.adminCachePath()
	if err := os.WriteFile(path, []byte(`{"rules":`), 0600); err != nil {
		t.Fatal(err)
	}
	f.rec = newRecorder()
	f.cfg.Sync = f.rec
	f.restart()
	if err := f.a.Once(ctx); !errors.Is(err, ErrPolicyUnknown) {
		t.Fatalf("unknown Once error = %v", err)
	}
	if got := f.count(`SELECT count(*) FROM conversations`); got != before {
		t.Fatalf("unknown policy changed existing conversations: %d -> %d", before, got)
	}
	if got := f.notified(func(devicesync.SourceSpec) bool { return true }); len(got) != 0 {
		t.Fatalf("unknown policy scheduled uploads: %v", got)
	}
	spec := devicesync.SourceSpec{Path: f.path(codexActive), Agent: transcript.AgentCodex}
	if f.a.allowUpload(spec) {
		t.Fatal("previously queued upload allowed with unknown rules")
	}
	if n, bounded := f.a.uploadBound(spec); !bounded || n != -1 {
		t.Fatalf("unknown upload bound = %d,%v", n, bounded)
	}
	// A fresh index must also collect nothing, including orphan stubs and stores.
	fresh := newFixture(t, "-")
	fresh.cfg.AdminRulesCache, fresh.cfg.PolicyAuthority, fresh.cfg.AdminRules = cache, f.cfg.PolicyAuthority, f.cfg.AdminRules
	fresh.restart()
	if err := fresh.a.Once(ctx); !errors.Is(err, ErrPolicyUnknown) {
		t.Fatalf("unknown Once error = %v", err)
	}
	// Exercise the continuous agent's sweep/workers too; Once can fail before them.
	sweepctx, stop := context.WithCancel(ctx)
	var workers sync.WaitGroup
	fresh.a.startWorkers(sweepctx, &workers)
	if err := fresh.a.sweep(sweepctx); err != nil {
		t.Fatal(err)
	}
	fresh.a.WaitIdle()
	stop()
	fresh.a.kick()
	workers.Wait()
	if err := fresh.a.Pass(ctx); !errors.Is(err, ErrPolicyUnknown) {
		t.Fatalf("unknown delegated pass error = %v", err)
	}
	if got := fresh.count(`SELECT count(*) FROM conversations`); got != 0 {
		t.Fatalf("unknown policy collected %d conversations", got)
	}
	// A successful empty answer is known policy and resumes the same agent.
	fresh.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{Rules: []string{}}, nil }
	fresh.a.cfg.AdminRules = fresh.cfg.AdminRules
	fresh.a.refreshPolicy(ctx, true)
	fresh.once()
	if got := fresh.count(`SELECT count(*) FROM conversations`); got == 0 {
		t.Fatal("valid empty policy did not resume collection")
	}
	if r := fresh.a.readAdminCache(); r.state != adminCacheValid {
		t.Fatalf("empty policy not durable: %+v", r)
	}
}

func TestAdminCacheAuthorityRetainedAndStandaloneLocal(t *testing.T) {
	cfg := Config{AdminRulesCache: filepath.Join(t.TempDir(), "policy.json"), PolicyAuthority: &PolicyAuthority{Server: "https://a.example", DeviceID: "dev"}}
	cfg.defaults()
	a := &Agent{cfg: cfg, log: cfg.Logger}
	if err := a.writeAdminCache(AdminPolicy{Rules: []string{"deny /tmp/**"}}); err != nil {
		t.Fatal(err)
	}
	a.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://b.example", DeviceID: "dev"}
	if err := a.writeAdminCache(AdminPolicy{Rules: []string{}}); err != nil {
		t.Fatal(err)
	}
	a.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "dev"}
	if r := a.readAdminCache(); r.state != adminCacheValid || len(r.policy.Rules) != 1 {
		t.Fatalf("A not retained: %+v", r)
	}
	a.cfg.PolicyAuthority = nil
	if !DeviceUploads(a.cfg, pathpolicy.Placement{Cwd: "/tmp/public"}) {
		t.Fatal("standalone inherited admin restriction")
	}
}

func TestAdminPolicyPersistenceFailureKeepsLiveRules(t *testing.T) {
	f, _, logs := rulesFixture(t, "-", "allow /tmp/**")
	f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "dev"}
	f.cfg.AdminRulesCache = filepath.Join(t.TempDir(), "missing", "cache.json")
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{Rules: []string{"local /tmp/**"}}, nil }
	f.restart()
	f.once()
	if f.a.policy().readiness != policyReady {
		t.Fatal("write failure discarded live policy")
	}
	if len(f.find("login test flake", false)) == 0 {
		t.Fatal("known local policy should collect")
	}
	if !strings.Contains(logs.String(), "cache could not be saved") {
		t.Fatal("cache write failure was not reported")
	}
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{}, errors.New("offline") }
	f.restart()
	if err := f.a.Once(ctx); !errors.Is(err, ErrPolicyUnknown) {
		t.Fatalf("unknown Once error = %v", err)
	}
	if f.a.policy().readiness != policyUnknown {
		t.Fatal("offline restart without durable policy did not pause")
	}
}

func TestUnknownPolicyBlocksAlreadyQueuedSchedulerUpload(t *testing.T) {
	f := newFixture(t, "-")
	dir := t.TempDir()
	remote := synctest.New("token")
	srv := httptest.NewServer(remote)
	defer srv.Close()
	queue, err := devicesync.OpenStore(filepath.Join(dir, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	spool, err := devicesync.OpenSpool(filepath.Join(dir, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := devicesync.NewSyncer(devicesync.Config{}, queue, spool, &syncproto.Client{Server: srv.URL, Token: "token", HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	sc := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{})
	sp := devicesync.SourceSpec{Path: f.path(codexActive), Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONLAppend, Parser: "codex@1"}
	// The job predates agent construction, as an offline backlog can.
	sc.Notify(sp)
	sc.Flush(sp)
	f.cfg.Sync = sc
	f.cfg.PolicyAuthority = &PolicyAuthority{Server: srv.URL, DeviceID: "device"}
	f.cfg.AdminRulesCache = filepath.Join(dir, "missing-policy.json")
	f.restart()
	checked := make(chan struct{})
	var once sync.Once
	sc.SetAdmission(func(sp devicesync.SourceSpec) error {
		err := f.a.admitUpload(sp)
		once.Do(func() { close(checked) })
		return err
	})
	runctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sc.Run(runctx) }()
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("queued job was not checked")
	}
	remote.Lock()
	requests := remote.FlushRequests
	remote.Unlock()
	if requests != 0 {
		cancel()
		<-done
		t.Fatalf("unknown policy uploaded %d requests", requests)
	}
	f.a.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
		return AdminPolicy{Rules: []string{}, Unplaceable: "upload"}, nil
	}
	f.a.refreshPolicy(ctx, true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		remote.Lock()
		requests = remote.FlushRequests
		remote.Unlock()
		if requests > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("queued job did not resume without a new notification")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestInvalidFetchedPolicyKeepsLastKnownFloor(t *testing.T) {
	f, _, _ := rulesFixture(t, "-", "allow /tmp/**")
	f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "dev"}
	f.cfg.AdminRulesCache = filepath.Join(t.TempDir(), "policy.json")
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{Rules: []string{"local /tmp/**"}}, nil }
	f.restart()
	f.once()
	old := f.a.policy().key
	f.a.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{Unplaceable: "garbage"}, nil }
	f.a.refreshPolicy(ctx, true)
	if f.a.policy().key != old || f.a.policy().readiness != policyReady {
		t.Fatal("invalid response replaced valid live rules")
	}
	if r := f.a.readAdminCache(); r.state != adminCacheValid || len(r.policy.Rules) != 1 {
		t.Fatalf("invalid response replaced saved rules: %+v", r)
	}
}

func TestLegacyCacheIsRetainedWithoutEstablishingAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-path-rules.json")
	legacy := []byte(`{"rules":["deny /tmp/**"]}`)
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{AdminRulesCache: path, PolicyAuthority: &PolicyAuthority{Server: "https://a.example", TokenFingerprint: "fingerprint-a"}}
	cfg.defaults()
	a := &Agent{cfg: cfg, log: cfg.Logger}
	if r := a.readAdminCache(); r.state != adminCacheAbsent {
		t.Fatalf("unowned legacy cache trusted: %+v", r)
	}
	if err := a.writeAdminCache(AdminPolicy{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(legacy) {
		t.Fatal("legacy cache changed")
	}
	first := a.adminCachePath()
	a.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", TokenFingerprint: "fingerprint-b"}
	if a.adminCachePath() == first {
		t.Fatal("legacy credential change retained authority")
	}
}

func TestPausedDaemonRecoversOnPeriodicPolicyFetch(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "dev"}
	f.cfg.AdminRulesCache = filepath.Join(t.TempDir(), "policy.json")
	f.cfg.AdminEvery = 10 * time.Millisecond
	f.cfg.FastLane = 10 * time.Millisecond
	f.cfg.Sweep = 10 * time.Millisecond
	var available atomic.Bool
	f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
		if available.Load() {
			return AdminPolicy{Rules: []string{}}, nil
		}
		return AdminPolicy{}, errors.New("offline")
	}
	f.restart()
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-f.a.discovered:
	case <-time.After(5 * time.Second):
		t.Fatal("paused daemon did not start")
	}
	if f.a.policy().readiness != policyUnknown {
		t.Fatal("daemon did not pause")
	}
	if got := f.count(`SELECT count(*) FROM conversations`); got != 0 {
		t.Fatalf("paused daemon collected %d conversations", got)
	}
	available.Store(true)
	f.a.polMu.Lock()
	f.a.adminAt = time.Now().Add(-2 * time.Minute)
	f.a.polMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for f.a.policy().readiness != policyReady || f.count(`SELECT count(*) FROM conversations`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("daemon did not recover after periodic policy fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
