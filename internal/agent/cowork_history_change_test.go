package agent

import (
	"database/sql"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCoworkHistoricalChangedSkipsRepeatedWritesAndIncludesNewAlias(t *testing.T) {
	f := newCoworkFixture(t)
	root := placeKey{transcript.AgentClaude, coworkNativeID}
	mark := func() (bool, error) {
		f.a.captureScopeMu.Lock()
		defer f.a.captureScopeMu.Unlock()
		return f.a.markCoworkHistoricalUnknownChanged(ctx, []placeKey{root})
	}
	if changed, err := mark(); err != nil || !changed {
		t.Fatalf("first historical mark: %v %v", changed, err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER reject_root_rewrite BEFORE INSERT ON placements WHEN NEW.session_id='` + coworkNativeID + `' BEGIN SELECT RAISE(FAIL,'repeated compatibility write'); END`); err != nil {
		t.Fatal(err)
	}
	if changed, err := mark(); err != nil || changed {
		t.Fatalf("repeated historical mark wrote placements: %v %v", changed, err)
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER reject_root_rewrite`); err != nil {
		t.Fatal(err)
	}
	f.a.mu.Lock()
	f.a.coworkFamilies = map[string]map[string]bool{coworkNativeID: {coworkNativeID: true, "agent-alias": true}}
	f.a.mu.Unlock()
	if changed, err := mark(); err != nil || !changed {
		t.Fatalf("new alias was skipped: %v %v", changed, err)
	}
	if have, err := f.store.CoworkHistoricalUnknown(ctx, "agent-alias"); err != nil || !have {
		t.Fatalf("alias not durable: %v %v", have, err)
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, "agent-alias"}); p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("alias historical origin not published")
	}
	if changed, err := mark(); err != nil || changed {
		t.Fatalf("expanded family not stable: %v %v", changed, err)
	}
}

func TestCoworkHistoricalChangedRetriesCompatibilityFailure(t *testing.T) {
	f := newCoworkFixture(t)
	if err := f.store.MarkCoworkHistoricalUnknown(ctx, []string{coworkNativeID}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TRIGGER reject_unknown BEFORE INSERT ON placements WHEN NEW.how='cowork-unknown' BEGIN SELECT RAISE(FAIL,'compatibility failure'); END`); err != nil {
		t.Fatal(err)
	}
	mark := func() (bool, error) {
		f.a.captureScopeMu.Lock()
		defer f.a.captureScopeMu.Unlock()
		return f.a.markCoworkHistoricalUnknownChanged(ctx, []placeKey{{transcript.AgentClaude, coworkNativeID}})
	}
	if changed, err := mark(); err == nil || !changed {
		t.Fatalf("first compatibility failure: %v %v", changed, err)
	}
	if p, _ := f.a.storedPlace(placeKey{transcript.AgentClaude, coworkNativeID}); p.how != localindex.PlacedByCoworkUnknown {
		t.Fatal("compatibility error weakened fact authority")
	}
	if changed, err := mark(); err == nil || !changed {
		t.Fatalf("compatibility failure incorrectly became no-op: %v %v", changed, err)
	}
	if err = f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER reject_unknown`); err != nil {
		t.Fatal(err)
	}
	if changed, err := mark(); err != nil || !changed {
		t.Fatalf("compatibility retry skipped: %v %v", changed, err)
	}
	if changed, err := mark(); err != nil || changed {
		t.Fatalf("successful retry not stable: %v %v", changed, err)
	}
}
