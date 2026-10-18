package agent

import (
	"database/sql"

	"github.com/flopwire/flopwire/internal/transcript"
	"path/filepath"
	"testing"
)

// Force both legal worker orders without relying on scheduler timing.
func TestCoworkCompanionOwnerInsertionOrder(t *testing.T) {
	for _, companionFirst := range []bool{true, false} {
		name := "owner-first"
		if companionFirst {
			name = "companion-first"
		}
		t.Run(name, func(t *testing.T) {
			f := newCoworkFixture(t)
			rules := filepath.Join(f.home, "rules")
			writeRules(t, rules, "deny /host/secret")
			f.cfg.UserRules = rules
			f.restart()
			coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
			path := coworkTranscript(t, f)
			sub, companion := coworkChildren(t, path)
			if err := f.a.load(ctx); err != nil {
				t.Fatal(err)
			}
			f.a.refreshAdmin(ctx)
			// Discovery enqueues all three targets, but no workers are started. Execute
			// their actual production index methods in a chosen legal worker order.
			if err := f.a.sweep(ctx); err != nil {
				t.Fatal(err)
			}
			companionTarget := f.a.lookup(companion, "")
			mainTarget := f.a.lookup(path, "")
			childTarget := f.a.lookup(sub, "")
			if companionTarget == nil || mainTarget == nil || childTarget == nil {
				t.Fatal("missing discovered targets")
			}
			indexCompanion := func() {
				if err := f.a.indexCompanion(ctx, companionTarget); err != nil {
					t.Fatal(err)
				}
			}
			indexOwners := func() {
				for _, target := range []*target{mainTarget, childTarget} {
					if _, err := f.a.indexTranscript(ctx, target); err != nil {
						t.Fatal(err)
					}
				}
			}
			if companionFirst {
				indexCompanion()
				indexOwners()
			} else {
				indexOwners()
				indexCompanion()
			}
			if err := f.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if n := f.count(`SELECT count(*) FROM companions`); n != 1 {
				t.Fatalf("initial companions=%d, want 1", n)
			}
			coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
			// Use Once as the original test does. It drains the queued workers and
			// commits the same metadata-policy transition before the assertions.
			f.once()
			if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
				t.Fatalf("denial left %d messages", n)
			}
			if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
				t.Fatalf("denial companions=%d want 0", n)
			}
			if n := f.count(`SELECT count(*) FROM companions WHERE conversation_id IS NULL`); n != 0 {
				t.Fatalf("denial NULL companions=%d want 0", n)
			}
			// A later purge cannot discover the orphan after its owner is gone.
			if err := f.a.purgeDenied(ctx); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
				t.Fatalf("repeat purge companions=%d want 0", n)
			}
			f.restart()
			f.once()
			if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
				t.Fatalf("restart restored denied companions=%d", n)
			}
			writeRules(t, rules, "local /host/secret")
			f.once()
			if len(f.find("cowork main needle", false)) != 1 || len(f.find("cowork child needle", false)) != 1 {
				t.Fatal("loosened rule did not restore transcripts")
			}
			if n := f.count(`SELECT count(*) FROM companions p JOIN conversations c ON c.id=p.conversation_id WHERE p.path=? AND c.session_id=?`, companion, coworkNativeID); n != 1 {
				t.Fatalf("restored companion owner links=%d", n)
			}

		})
	}
}

// Simulate a pre-fix index whose transcript has already been purged. The
// companion can have neither a conversation nor a source, so only fresh
// native discovery can supply its owner.
func TestCoworkLegacyUnlinkedCompanionDeniedOnRestart(t *testing.T) {
	f := newCoworkFixture(t)
	f.cfg.UserRuleList = []string{"deny /host/secret"}
	f.restart()
	coworkMetadata(t, f, []string{"/host/public"}, nil, nil)
	path := coworkTranscript(t, f)
	_, companion := coworkChildren(t, path)
	f.once()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", f.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`UPDATE companions SET conversation_id=NULL, source_id=NULL WHERE path=?`, companion); err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{coworkNativeID, "agent-cafe"} {
		if err := f.store.PurgeConversation(ctx, transcript.AgentClaude, session); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 1 {
		t.Fatalf("legacy companions=%d", n)
	}
	coworkMetadata(t, f, []string{"/host/secret"}, nil, nil)
	f.restart()
	if err := f.a.load(ctx); err != nil {
		t.Fatal(err)
	}
	f.a.refreshAdmin(ctx)
	if err := f.a.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	target := f.a.lookup(companion, "")
	if target == nil {
		t.Fatal("companion was not rediscovered")
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_companion_purge BEFORE DELETE ON companions BEGIN SELECT RAISE(FAIL,'synthetic deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.a.indexCompanion(ctx, target); err == nil {
		t.Fatal("companion deletion failure ignored")
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 1 {
		t.Fatalf("failed purge removed proof: %d", n)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_companion_purge`); err != nil {
		t.Fatal(err)
	}
	if err := f.a.indexCompanion(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("legacy denied companions=%d", n)
	}
	if n := f.count(`SELECT count(*) FROM messages`); n != 0 {
		t.Fatalf("denied messages=%d", n)
	}
	f.restart()
	f.once()
	if n := f.count(`SELECT count(*) FROM companions`); n != 0 {
		t.Fatalf("repeat restart companions=%d", n)
	}
}
