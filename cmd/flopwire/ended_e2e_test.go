package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// endedWindow is how soon an ended session's end shows: the agent reads
// presence (and so the registries) on its 2 s tick, and peers caches
// presence for a second. Asserted with slack for a loaded machine.
const endedWindow = 10 * time.Second

// Through a real device agent with no server: B's Claude session file
// names a running process; A sends to B; the process is killed (its file
// stays, naming a dead pid). Within the window the sender's inbox shows
// the message undelivered (session_ended), peers no longer lists B, and
// B's hooks get nothing (#67, #82).
func TestHookEndToEndSessionEnded(t *testing.T) {
	sock, home := hookE2EHome(t)
	proc := exec.Command("sleep", "120")
	if err := proc.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	t.Cleanup(func() { proc.Process.Kill(); proc.Wait() })
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", proc.Process.Pid)),
		fmt.Appendf(nil, `{"pid":%d,"sessionId":%q,"status":"idle","kind":"interactive","entrypoint":"sdk-cli"}`, proc.Process.Pid, e2eB), 0o600); err != nil {
		t.Fatal(err)
	}
	waitPeer(t, sock, e2eA, e2eB) // presence read: B is held
	out, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "for a session that is killed")
	if err != nil || !strings.Contains(out, "idle, arrives with its human's next prompt") {
		t.Fatalf("send: %q %v", out, err)
	}
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	proc.Wait()
	killed := time.Now()
	for {
		inbox, _ := busCLI(t, sock, e2eA, "", "inbox", "--sent")
		peers, _ := busCLI(t, sock, e2eA, "", "peers")
		if strings.Contains(inbox, "inform  undelivered (session_ended)\n") && !strings.Contains(peers, "e2e0bbbb") {
			t.Logf("ended after %s", time.Since(killed).Round(10*time.Millisecond))
			break
		}
		if time.Since(killed) > endedWindow {
			t.Fatalf("not ended %s after the kill:\ninbox:\n%s\npeers:\n%s", endedWindow, inbox, peers)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if o := runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0]; strings.Contains(o, "for a session that is killed") {
		t.Fatalf("delivered after the end: %q", o)
	}
	// Sent to the ended session now: the receipt says it is not running.
	if out, err := busCLI(t, sock, e2eA, "", "send", e2eB, "--", "after the end"); err != nil || !strings.Contains(out, "not running, arrives only if it resumes") {
		t.Fatalf("send after the end: %q %v", out, err)
	}
}
