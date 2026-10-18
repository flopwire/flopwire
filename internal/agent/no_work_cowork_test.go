package agent

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// A settled ordinary capture may never need another scheduler authorization.
// Periodic discovery must still classify its historical bytes when app metadata
// later proves that its native session belongs to Cowork.
func TestNoWorkLateCoworkSweepHoldsSettledOrdinaryCapture(t *testing.T) {
	f := newCoworkFixture(t)
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+f.store.Path()+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	st, err := devicesync.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := devicesync.OpenSpool(filepath.Join(f.home, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(synctest.New("synthetic-token"))
	defer server.Close()
	now := time.Now().Add(time.Hour)
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: f.cfg.Logger, Now: func() time.Time { return now }, SealAfter: time.Minute}, st, spool, &syncproto.Client{Server: server.URL, Token: "synthetic-token", HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	path := filepath.Join(f.cfg.ClaudeProjects, "ordinary-project", coworkNativeID+".jsonl")
	coworkWrite(t, path, coworkRecord(coworkNativeID, "ordinary-first", "preexisting ordinary bytes"))
	sp := devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, SessionKey: coworkNativeID, Parser: "claude@4.1"}
	if err := sy.Sync(ctx, sp); err != nil {
		t.Fatal(err)
	}
	var settled int
	if err := db.QueryRow(`SELECT count(*) FROM devsync_gens WHERE size>0 AND acked=entries AND tail_acked=1 AND tail_size=0 AND lost=0`).Scan(&settled); err != nil || settled != 1 {
		t.Fatalf("settled captures=%d err=%v", settled, err)
	}
	// No local extraction rows exist: classification must find raw sync evidence,
	// including a generation that has already been completely acknowledged.
	if n := f.count(`SELECT count(*) FROM sources`); n != 0 {
		t.Fatalf("unexpected indexed sources=%d", n)
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); localindex.IsCoworkPlacement(p.how) {
		t.Fatalf("ordinary source already classified: %+v", p)
	}
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	// Invoke the periodic sweep itself. No scheduler is run and no authorization
	// callback can discover or taint this old capture on the sweep's behalf.
	if err := f.a.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID})
	if p.how != localindex.PlacedByCoworkUnknown {
		t.Fatalf("sweep failed historical classification: %+v", p)
	}
	facts, err := f.store.CoworkHistoricalUnknownFacts(ctx)
	if err != nil || len(facts) != 1 || facts[0] != coworkNativeID {
		t.Fatalf("historical facts=%v err=%v", facts, err)
	}
	if f.a.allowUpload(sp) {
		t.Fatal("late-discovered historical capture allowed for upload")
	}
	f.restart()
	if err := f.a.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if f.a.allowUpload(sp) {
		t.Fatal("restart erased late-discovered historical hold")
	}
}

func TestNoWorkLateCoworkSweepEnforcesAndReconcilesWithoutAuthorization(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.UserRuleList = []string{"deny /host/secret"}
	policy := &coworkPolicyRecorder{}
	f.cfg.CoworkPolicy = policy
	f.restart()
	path := filepath.Join(f.cfg.ClaudeProjects, "ordinary-project", coworkNativeID+".jsonl")
	coworkWrite(t, path, coworkRecord(coworkNativeID, "ordinary-indexed", "ordinary indexed needle"))
	f.once()
	sp, ok := f.rec.spec(path)
	if !ok {
		t.Fatal("ordinary source not offered for upload")
	}
	id, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	bound, bounded := f.a.uploadBound(sp)
	if !bounded || bound != id.Size {
		t.Fatalf("production upload bound=%d bounded=%v size=%d", bound, bounded, id.Size)
	}
	if len(f.find("ordinary indexed needle", false)) != 1 {
		t.Fatal("ordinary source not indexed")
	}
	coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
	if err := f.a.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.find("ordinary indexed needle", false)) != 0 {
		t.Fatal("late Cowork deny did not purge indexed content")
	}
	if f.a.allowUpload(sp) {
		t.Fatal("late Cowork deny allowed upload")
	}
	if len(policy.requests) == 0 {
		t.Fatal("periodic sweep did not reconcile historical policy")
	}
	last := policy.requests[len(policy.requests)-1]
	if last.SessionID != coworkNativeID || last.EvidenceScope != syncproto.EvidenceUnmapped || last.ClientMode != syncproto.ClientModeDeny {
		t.Fatalf("reconciled scope=%+v", last)
	}
}
