package devicebus

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// These synthetic store controls distinguish identity and import timing. They
// do not reproduce the historical IDE miss or exercise harness context entry.
func TestHookBoundaryIdentityAndDelayedImportControls(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	now := time.Now().UTC()
	e := busproto.Envelope{ID: "synthetic-message", ThreadID: "synthetic-message", ToSession: "sessionA", ToAgent: "codex", From: "sender", Body: "synthetic marker", Sent: now, ExpiresAt: now.Add(time.Hour)}
	take := func(session string) []busproto.Envelope {
		t.Helper()
		_, got, err := st.take(ctx, session, "codex", now, Limit{}, LeaseFor, 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// Before local import, a correct identity cannot retrieve an absent row.
	if has, err := st.has(ctx, e.ID); err != nil || has {
		t.Fatalf("preimport row=%v err=%v", has, err)
	}
	if got := take("sessionA"); len(got) != 0 {
		t.Fatal("delivered before import")
	}
	if err := st.addClaimed(ctx, e); err != nil {
		t.Fatal(err)
	}
	// A wrong-session boundary leaves the intended recipient's row queued.
	if got := take("sessionB"); len(got) != 0 {
		t.Fatal("wrong session received message")
	}
	var state string
	if err := st.db.QueryRowContext(ctx, `SELECT state FROM devbus_messages WHERE id=?`, e.ID).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("wrong session state=%q err=%v", state, err)
	}
	// The next correct boundary sees the newly imported message exactly once.
	got := take("sessionA")
	if len(got) != 1 || got[0].ID != e.ID {
		t.Fatalf("next boundary: %+v", got)
	}
	if _, err := st.confirm(ctx, "sessionA", []string{e.ID}, now); err != nil {
		t.Fatal(err)
	}
	if got := take("sessionA"); len(got) != 0 {
		t.Fatal("confirmed message repeated")
	}
}
