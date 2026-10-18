package agent

import (
	"database/sql"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func markHistoryForTest(t *testing.T, f *fixture) error {
	t.Helper()
	f.a.captureScopeMu.Lock()
	defer f.a.captureScopeMu.Unlock()
	return f.a.markCoworkHistoryUnknown(ctx, []placeKey{{transcript.AgentClaude, coworkNativeID}})
}

func TestCoworkHistoricalMarkEnforcesOnlyNewFloor(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	coworkChildren(t, path)
	f.once()
	if n := f.count(`SELECT count(*) FROM conversations`); n == 0 {
		t.Fatal("missing indexed family")
	}
	before := f.a.policy().gen
	if err := markHistoryForTest(t, f); err != nil {
		t.Fatal(err)
	}
	if f.a.policy().gen <= before {
		t.Fatal("new historical floor did not enforce")
	}
	if n := f.count(`SELECT count(*) FROM conversations`); n != 0 {
		t.Fatalf("denied family not purged: %d", n)
	}
	stable := f.a.policy().gen
	if err := markHistoryForTest(t, f); err != nil {
		t.Fatal(err)
	}
	if f.a.policy().gen != stable {
		t.Fatal("already committed historical floor invalidated all targets again")
	}
}

func TestCoworkHistoricalMarkRetriesFailedPurgeAfterRestart(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.Unplaceable = "exclude"
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	coworkTranscript(t, f)
	f.once()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER fail_historical_purge BEFORE DELETE ON conversations BEGIN SELECT RAISE(FAIL,'synthetic purge failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := markHistoryForTest(t, f); err == nil {
		t.Fatal("purge failure hidden")
	}
	if n := f.count(`SELECT count(*) FROM conversations`); n == 0 {
		t.Fatal("failed purge unexpectedly erased evidence")
	}
	if known, err := f.store.CoworkHistoricalUnknown(ctx, coworkNativeID); err != nil || !known {
		t.Fatalf("fact not durable: %v %v", known, err)
	}
	// Startup policy enforcement must reconstruct unfinished work from retained
	// rows and durable placements, without persisting a second permission state.
	f.restart()
	f.a.refreshPolicy(ctx, false)
	f.a.mu.Lock()
	pending := f.a.policyEnforcementErr
	f.a.mu.Unlock()
	if pending == nil {
		t.Fatal("startup lost unfinished historical purge")
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER fail_historical_purge`); err != nil {
		t.Fatal(err)
	}
	if err := markHistoryForTest(t, f); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM conversations`); n != 0 {
		t.Fatalf("retry did not purge retained denied history: %d", n)
	}
	f.a.mu.Lock()
	pending = f.a.policyEnforcementErr
	f.a.mu.Unlock()
	if pending != nil {
		t.Fatal("successful retry retained unfinished enforcement")
	}
	stable := f.a.policy().gen
	if err := markHistoryForTest(t, f); err != nil {
		t.Fatal(err)
	}
	if f.a.policy().gen != stable {
		t.Fatal("completed retry continued global enforcement")
	}
}
