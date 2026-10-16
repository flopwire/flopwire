package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
)

// A held message can outlive the session it addressed. Acceptance releases
// the hold, but does not make the ended session receive the message.
func TestRetryEndToEndAfterAcceptanceAndSessionEnded(t *testing.T) {
	s := newAcceptServer(t)
	ctx := t.Context()
	enrolled := apiCall(t, s.url, s.alex, "POST", "/v1/devices", map[string]string{"name": "alex-lifecycle", "platform": "darwin"})
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	if err := client.Save(client.Config{Server: s.url, Token: enrolled["token"].(string), SessionToken: s.alex, DeviceID: enrolled["device"].(map[string]any)["id"].(string)}); err != nil {
		t.Fatal(err)
	}
	// The API fixture does not ingest transcript uploads. Record the same
	// ownership row ingestion creates so messages still route to this
	// device after the session leaves live presence.
	if _, err := s.pool.Exec(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id) VALUES(gen_random_uuid(),'claude',$1,$2,$3)`, e2eB, enrolled["device"].(map[string]any)["id"].(string), s.alexID); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	writeClaudeSession(t, filepath.Join(home, ".claude", "projects"), e2eB, "/tmp/e2e-api", "review pagination")
	proc := exec.Command("sleep", "120")
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.Process.Kill(); proc.Wait() })
	sessions := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", proc.Process.Pid)), fmt.Appendf(nil, `{"pid":%d,"sessionId":%q,"status":"idle","kind":"interactive","entrypoint":"sdk-cli"}`, proc.Process.Pid, e2eB), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortSockDir(t), "alex.sock")
	startAgent(t, home, sock)
	api := client.HTTP{Server: s.url, Token: s.garyDevice}
	peers := func() busproto.PeersResponse {
		t.Helper()
		var out busproto.PeersResponse
		if err := api.JSON(ctx, "GET", busproto.PathPeers+"?session=gary-1111", nil, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	e2eWait(t, "recipient process live", func() bool {
		for _, p := range peers().Peers {
			if p.Session == e2eB {
				return true
			}
		}
		return false
	})
	const firstBody = "held until after the recipient exits"
	first := s.send(t, e2eB, firstBody)
	if first.State != busproto.StateHeld || !first.To.Live {
		t.Fatalf("send while live: %+v", first)
	}
	t.Logf("send state=%s live=%t", first.State, first.To.Live)
	if err := proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	proc.Wait()
	e2eWait(t, "recipient process ended", func() bool {
		for _, p := range peers().Peers {
			if p.Session == e2eB {
				return false
			}
		}
		return true
	})
	out, _, err := acceptRun(t, true, mustLoad(t), nil, adminPassword+"\n", "accept", "--text", "gary@example.test")
	if err != nil || !strings.Contains(out, "1 held message released") {
		t.Fatalf("accept: %q %v", out, err)
	}
	t.Logf("accept stdout: %s", strings.TrimSpace(out))
	e2eWait(t, "terminal session-ended receipt", func() bool {
		var in busproto.InboxResponse
		if err := api.JSON(ctx, "GET", busproto.PathInbox+"?session=gary-1111&sent=true", nil, &in); err != nil {
			t.Fatal(err)
		}
		for _, m := range in.Messages {
			if m.ID == first.ID {
				return m.State == busproto.StateUndelivered && m.Reason == busproto.ReasonSessionEnded
			}
		}
		return false
	})
	t.Log("sender inbox state=undelivered reason=session_ended")
	// A later hook resumes the session. A terminal failed message stays
	// failed, rather than being delivered as a side effect of the resume.
	hook := func() string {
		t.Helper()
		var stdout, stderr strings.Builder
		hookCmd(ctx, []string{"--socket", sock}, strings.NewReader(hookFor(e2eB, evPostToolUse)), &stdout, &stderr, func(string) string { return "" })
		var output hookOutput
		if stdout.Len() != 0 {
			if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
				t.Fatal(err)
			}
		}
		return output.HookSpecificOutput.AdditionalContext
	}
	for range 2 {
		if got := hook(); strings.Contains(got, first.ID) {
			t.Fatalf("terminal message delivered after resume: %s", got)
		}
	}
	resumed := exec.Command("sleep", "120")
	if err := resumed.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resumed.Process.Kill(); resumed.Wait() })
	if err := os.Remove(filepath.Join(sessions, fmt.Sprintf("%d.json", proc.Process.Pid))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, fmt.Sprintf("%d.json", resumed.Process.Pid)), fmt.Appendf(nil, `{"pid":%d,"sessionId":%q,"status":"idle","kind":"interactive","entrypoint":"sdk-cli"}`, resumed.Process.Pid, e2eB), 0o600); err != nil {
		t.Fatal(err)
	}
	e2eWait(t, "resumed session in peers", func() bool {
		for _, p := range peers().Peers {
			if p.Session == e2eB {
				return true
			}
		}
		return false
	})
	var duplicate busproto.SendResponse
	err = api.JSON(ctx, "POST", busproto.PathSend, busproto.SendRequest{FromSession: "gary-1111", To: e2eB, Body: firstBody}, &duplicate)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != busproto.CodeDuplicate {
		t.Fatalf("identical resend within duplicate window: %+v %v", duplicate, err)
	}
	t.Logf("identical resend refused: status=%d code=%s duplicate_window=%s", apiErr.StatusCode, apiErr.Code, busproto.DuplicateWindow)
	if got := hook(); strings.Contains(got, "<flopwire-message ") {
		t.Fatalf("message delivered after refused duplicate: %s", got)
	}
	request := busproto.SendRequest{FromSession: "gary-1111", To: e2eB, Body: firstBody, RetryOf: first.ID}
	var retry busproto.SendResponse
	if err := api.JSON(ctx, "POST", busproto.PathRetry, request, &retry); err != nil {
		t.Fatal(err)
	}
	if retry.ID == first.ID || retry.RetryOf != first.ID || retry.ThreadID != first.ThreadID || retry.State != busproto.StateQueued || retry.Replayed {
		t.Fatalf("retry receipt: %+v", retry)
	}
	var delivered string
	e2eWait(t, "explicit retry delivered", func() bool {
		delivered = hook()
		return strings.Contains(delivered, retry.ID)
	})
	if strings.Count(delivered, "<flopwire-message ") != 1 || !strings.Contains(delivered, firstBody) || !strings.Contains(delivered, `retry-of="`+first.ID+`"`) {
		t.Fatalf("retry context: %s", delivered)
	}
	e2eWait(t, "retry delivery receipt", func() bool {
		var in busproto.InboxResponse
		if err := api.JSON(ctx, "GET", busproto.PathInbox+"?session=gary-1111&sent=true", nil, &in); err != nil {
			t.Fatal(err)
		}
		originalFailed, retryDelivered := false, false
		for _, m := range in.Messages {
			originalFailed = originalFailed || m.ID == first.ID && m.State == busproto.StateUndelivered && m.Reason == busproto.ReasonSessionEnded
			retryDelivered = retryDelivered || m.ID == retry.ID && m.State == busproto.StateDelivered && m.RetryOf == first.ID
		}
		return originalFailed && retryDelivered
	})
	var replay busproto.SendResponse
	if err := api.JSON(ctx, "POST", busproto.PathRetry, request, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != retry.ID || !replay.Replayed || replay.State != busproto.StateDelivered || arrival(replay) != arriveRecorded || sendNext(replay) != "" {
		t.Fatalf("replay receipt: %+v", replay)
	}
	if got := hook(); strings.Contains(got, "<flopwire-message ") {
		t.Fatalf("retry delivered twice: %s", got)
	}
	t.Log("explicit retry delivered once; repeated retry returned existing delivered successor")

}
