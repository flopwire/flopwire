package e2e

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
)

// Exercise the installed agent, its real scheduler, upload transport, server
// extraction and cross-device retrieval. A no-op authorization callback or a
// disabled collector cannot satisfy both the content and operation assertions.
func (h *harness) checkUnrelatedDesktopCapture(t *testing.T, d, reader *device, r *result) {
	d.stop(false)
	defer func() { d.stop(false); d.start() }()
	cowork := filepath.Join(d.dir, "unrelated-cowork")
	for i := range 64 {
		metadata := fmt.Sprintf(`{"sessionId":"session-%d","cliSessionId":"11111111-2222-4333-8444-%012d","userSelectedFolders":["/synthetic/selected"]}`, i, i)
		writeFile(t, filepath.Join(cowork, "account", "workspace", fmt.Sprintf("session-%d.json", i)), metadata)
	}
	// Only Codex changes in this phase. Startup/sweep discovery still observes
	// the unrelated desktop sessions; there is no Claude capture to refresh them.
	d.start("--claude-projects", filepath.Join(d.dir, "empty-claude"),
		"--cowork-root", cowork, "--desktop-code-root", "-", "--devin-db", "-", "--opencode-db", "-", "--sweep", "1h")
	read := func() (*agent.DiagnosticsSnapshot, error) {
		response, err := agent.Call(t.Context(), d.sock, agent.Request{Op: "diagnostics"})
		if err != nil {
			return nil, err
		}
		if !response.OK || response.Diagnostics == nil {
			return nil, fmt.Errorf("agent diagnostics unavailable: %+v", response)
		}
		return response.Diagnostics, nil
	}
	var before *agent.DiagnosticsSnapshot
	eventually(t, 30*time.Second, 100*time.Millisecond, "restarted agent diagnostics", func() (bool, error) {
		var err error
		before, err = read()
		return before != nil, err
	})
	for i := range 3 {
		needle := fmt.Sprintf("unrelated desktop codex capture %s %d", h.nonce, i)
		appendFile(t, d.codexPath, d.codexMsg("user", needle))
		eventually(t, 30*time.Second, 100*time.Millisecond, "cross-device Codex append", func() (bool, error) {
			hits, err := reader.serverFind(needle, "--agent", "codex", "--device", d.name)
			return len(inSession(hitsOn(hits, d), d.codexSID)) == 1, err
		})
	}
	after, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if before.PID != after.PID {
		t.Fatal("agent restarted during capture measurement")
	}
	calls := func(d *agent.DiagnosticsSnapshot, name string) uint64 {
		for _, op := range d.Operations {
			if op.Name == name {
				return op.Calls
			}
		}
		t.Fatalf("missing diagnostic operation %s", name)
		return 0
	}
	authorizations := calls(after, "authorize_capture") - calls(before, "authorize_capture")
	refreshes := calls(after, "refresh_from_authorization") - calls(before, "refresh_from_authorization")
	if authorizations < 3 || refreshes != 0 {
		t.Fatalf("3 cross-device appends: authorizations=%d desktop refreshes=%d, want >=3 and 0", authorizations, refreshes)
	}
	r.Notes = append(r.Notes, fmt.Sprintf("64 unrelated desktop sessions; %d capture authorizations; 0 authorization-triggered desktop refreshes; all 3 appends retrieved by the second device", authorizations))
}
