package ingest

import (
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
)

func claudeLineAt(sid, uuid, cwd, text string) string {
	return `{"type":"user","cwd":"` + cwd + `","uuid":"` + uuid + `","sessionId":"` + sid + `","timestamp":"2026-09-20T00:01:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
}

// A stored session that names a denied directory in a later line (an
// agent that uploads it anyway) is hidden at parse time, with the
// directory recorded, and stays hidden through the rule sweep; the hide
// then runs the confirm/7-day purge path like any other.
func TestLaterCwdHidesStoredSession(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	const sid = "0b7e2c1a-0000-4000-8000-00000000d001"
	sp := claudeAt(t, home, "-work-app", sid, "/work/app")
	e.setRules("", "/work/secret")
	e.enforce()
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	e.drain()
	if e.visible(sid) != 1 {
		t.Fatal("allowed session not stored")
	}

	appendFile(t, sp.Path, claudeLineAt(sid, "late-1", "/work/secret/sub", "moved into the secret checkout"))
	sync1(t, sy, sp)
	e.drain()
	if c, _ := e.stored(sid); c != 1 {
		t.Fatalf("session refused rather than hidden: %d conversations", c)
	}
	if e.visible(sid) != 0 {
		t.Fatal("a session that named a denied directory is visible")
	}
	var others []string
	var rule string
	if err := e.pool.QueryRow(e.ctx, `SELECT other_cwds,hidden_rule FROM conversations WHERE session_id=$1`, sid).Scan(&others, &rule); err != nil {
		t.Fatal(err)
	}
	if len(others) != 1 || others[0] != "/work/secret/sub" || rule != "/work/secret" {
		t.Fatalf("other_cwds %v rule %q", others, rule)
	}
	// A rule change re-checks with every directory: still hidden, not
	// restored for its first directory.
	e.setRules("", "/work/secret", "/nothing")
	if n := e.enforce(); n.Restored != 0 {
		t.Fatalf("sweep restored it: %+v", n)
	}
	// An append that repeats only the first directory keeps it hidden.
	appendFile(t, sp.Path, claudeLineAt(sid, "late-2", "/work/app", "back home"))
	sync1(t, sy, sp)
	e.drain()
	if e.visible(sid) != 0 {
		t.Fatal("an append naming only the first directory made it visible")
	}
	// Seven days on, the sweep purges it.
	e.exec(`UPDATE conversations SET hidden_at=now()-interval '7 days 1 minute' WHERE session_id=$1`, sid)
	if n := e.enforce(); n.Purged != 1 {
		t.Fatalf("expired hide not purged: %+v", n)
	}
	if c, _ := e.stored(sid); c != 0 {
		t.Fatal("purged session still stored")
	}
}

// Directories that only resemble a denied one keep the session visible.
func TestLaterCwdNearMissStaysVisible(t *testing.T) {
	e := newEnv(t)
	const sid = "0b7e2c1a-0000-4000-8000-00000000d002"
	sp := claudeAt(t, t.TempDir(), "-work-app", sid, "/work/app")
	e.setRules("", "/work/secret")
	e.enforce()
	sy := e.syncer(devicesync.Config{SealAfter: -1})
	sync1(t, sy, sp)
	appendFile(t, sp.Path, claudeLineAt(sid, "nm-1", "/work/secretive", "a")+claudeLineAt(sid, "nm-2", "/work/secret-2/x", "b")+
		claudeLineAt(sid, "nm-3", "work/secret", "c"))
	sync1(t, sy, sp)
	e.drain()
	if e.visible(sid) != 1 {
		t.Fatal("near-miss directories hid the session")
	}
	e.setRules("", "/work/secret", "/nothing")
	if n := e.enforce(); n.Hidden != 0 {
		t.Fatalf("sweep hid a near miss: %+v", n)
	}
}
