package local

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestLiveSessions(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	write := func(p, s string, mtime time.Time) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	claude := filepath.Join(home, ".claude")
	// Running and writing: live. Running but idle for two hours: not.
	// Exited: not.
	write(filepath.Join(claude, "sessions", "101.json"), fmt.Sprintf(`{"pid":101,"sessionId":"c-live","updatedAt":%d}`, now.Add(-2*time.Hour).UnixMilli()), now)
	write(filepath.Join(claude, "projects", "-x", "c-live.jsonl"), "{}", now.Add(-5*time.Minute))
	write(filepath.Join(claude, "sessions", "102.json"), fmt.Sprintf(`{"pid":102,"sessionId":"c-idle","updatedAt":%d}`, now.Add(-2*time.Hour).UnixMilli()), now)
	write(filepath.Join(claude, "projects", "-x", "c-idle.jsonl"), "{}", now.Add(-2*time.Hour))
	write(filepath.Join(claude, "sessions", "103.json"), `{"pid":103,"sessionId":"c-dead"}`, now)
	write(filepath.Join(claude, "sessions", "101.abc.key"), "x", now)
	devin := filepath.Join(home, ".local", "share", "devin", "cli")
	write(filepath.Join(devin, "session_locks", "able-kangaroo.lock"), "101\n", now)
	write(filepath.Join(devin, "session_locks", "old-lock.lock"), "103", now)
	codexHome := filepath.Join(home, ".codex")
	rollout := filepath.Join(codexHome, "sessions", "2026", "09", "30", "rollout-2026-09-30T10-00-00-019a0000-0000-7000-8000-0000000000aa.jsonl")
	write(rollout, "{}", now.Add(-time.Minute))
	d := &Detector{Getenv: func(string) string { return "" }, Home: home,
		PidAlive: func(pid int) bool { return pid == 101 || pid == 102 },
		CodexFiles: func() []string {
			return []string{rollout, "/elsewhere/rollout-x-019a0000-0000-7000-8000-0000000000bb.jsonl"}
		}}
	live := d.Live(true)
	if _, ok := live["c-live"]; !ok || len(live) != 3 {
		t.Fatalf("live: %v", live)
	}
	if _, ok := live["able-kangaroo"]; !ok {
		t.Fatalf("devin lock: %v", live)
	}
	if _, ok := live["019a0000-0000-7000-8000-0000000000aa"]; !ok {
		t.Fatalf("codex rollout: %v", live)
	}
	if got := d.Live(false); len(got) != 2 {
		t.Fatalf("without codex: %v", got)
	}

	// MarkLive: recent activity, or held open and written within the cap.
	ago := func(d time.Duration) *time.Time { x := now.Add(-d); return &x }
	for _, tc := range []struct {
		c    format.ConversationInfo
		want bool
	}{
		{format.ConversationInfo{SessionID: "other", LastActivityAt: ago(time.Minute)}, true},
		{format.ConversationInfo{SessionID: "other", LastActivityAt: ago(time.Hour)}, false},
		{format.ConversationInfo{SessionID: "c-live", LastActivityAt: ago(30 * time.Minute)}, true},
		{format.ConversationInfo{SessionID: "able-kangaroo", LastActivityAt: ago(30 * time.Minute)}, true},
		{format.ConversationInfo{SessionID: "able-kangaroo", LastActivityAt: ago(3 * time.Hour)}, false},
		{format.ConversationInfo{SessionID: "x", Live: true, LastActivityAt: ago(3 * time.Hour)}, true}, // the server said so
	} {
		c := tc.c
		MarkLive(&c, live, now)
		if c.Live != tc.want {
			t.Errorf("%s last %v: live %v, want %v", c.SessionID, c.LastActivityAt, c.Live, tc.want)
		}
	}
	rep := LiveReporter(d, time.Minute, 2)
	if got := rep(); len(got) != 2 || got[0] > got[1] {
		t.Fatalf("reporter: %v", got)
	}
}
