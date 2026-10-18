package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Exercise the actual CLI against the TLS server and two enrolled devices.
// Placement comes from real Git checkouts and uploaded synthetic transcripts.
func (h *harness) checkMessageThreads(t *testing.T, d1, d2 *device, r *result) (string, string, string) {
	t.Helper()
	const remote = "github.test/team/messages"
	devices := []*device{d1, d2}
	repos, sessions := make([]string, 2), make([]string, 2)
	for i, d := range devices {
		repos[i] = filepath.Join(d.home, "message-repo")
		for _, args := range [][]string{{"init", "--quiet", repos[i]}, {"-C", repos[i], "remote", "add", "origin", "https://" + remote + ".git"}} {
			if out, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput(); err != nil {
				t.Fatalf("synthetic repo: %v %s", err, out)
			}
		}
		sessions[i] = fmt.Sprintf("e2e-messages-%d-%s", i, h.nonce)
		path := filepath.Join(d.home, ".claude", "projects", "-e2e-messages", sessions[i]+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		row, _ := json.Marshal(map[string]any{"type": "user", "uuid": "message-fixture-" + sessions[i], "sessionId": sessions[i], "timestamp": now(), "cwd": repos[i], "message": map[string]any{"role": "user", "content": "canonical message fixture " + h.nonce}})
		writeFile(t, path, string(row)+"\n")
		h.waitSessionSynced(t, d, transcript.AgentClaude, path, sessions[i], time.Minute)
	}
	readConfig := func(path string) client.Config {
		t.Helper()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var cfg client.Config
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	bus := func(path string) client.Bus {
		cfg := readConfig(path)
		return client.Bus{Server: cfg.Server, Token: cfg.Token, HTTP: cfg.HTTPClient()}
	}
	send := func(d *device, from, to, body, reply string) busproto.SendResponse {
		t.Helper()
		out, err := bus(d.cfg).Send(t.Context(), busproto.SendRequest{FromSession: from, To: to, Body: body, ReplyTo: reply})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	root := send(d1, sessions[0], sessions[1], "Initial plan "+h.nonce, "")
	// Reply bodies stay complete when opening a thread; list rows are previews.
	replyBody := "Accepted plan " + h.nonce + "\nKeep the second line intact."
	reply := send(d2, sessions[1], sessions[0], replyBody, root.ID)
	second := send(d1, sessions[0], sessions[1], "Other thread "+h.nonce, "")
	get := func(args ...string) busproto.MessagesResponse {
		t.Helper()
		// No --repo: resolve the viewing device's current checkout, then match
		// the sender's other checkout through the recorded normalized remote.
		raw, err := h.runAt(repos[1], d2.env(), "", append([]string{"messages"}, args...)...)
		var out busproto.MessagesResponse
		if err != nil || json.Unmarshal([]byte(raw), &out) != nil {
			t.Fatalf("messages CLI: %v %s", err, raw)
		}
		return out
	}
	page := get("--limit", "1")
	if len(page.Threads) != 1 || page.Threads[0].ThreadID != second.ThreadID || !page.More || page.Next == "" {
		t.Fatalf("first page: %+v", page)
	}
	next := get("--limit", "1", "--cursor", page.Next)
	if len(next.Threads) != 1 || next.Threads[0].ID != reply.ID || next.Threads[0].Body != "Accepted plan "+h.nonce || next.More {
		t.Fatalf("second page/preview: %+v", next)
	}
	full := get("--thread", root.ThreadID, "--session", sessions[0], "--user", "gary@e2e.test")
	if len(full.Messages) != 2 || full.Messages[0].Body != replyBody || full.Messages[0].ReplyTo != root.ID || full.Messages[1].ID != root.ID {
		t.Fatalf("canonical full thread: %+v", full)
	}
	if got := get("--until", "2000-01-01T00:00:00Z"); len(got.Threads) != 0 {
		t.Fatalf("time filter: %+v", got)
	}
	text, err := h.runAt(repos[1], d2.env(), "", "messages", "--thread", root.ThreadID, "--text")
	if err != nil || !strings.Contains(text, root.ID) || !strings.Contains(text, "Keep the second line intact.") {
		t.Fatalf("readable full thread: %v %s", err, text)
	}

	// An external sender must remain invisible until accepted, and revoking
	// must remove the same row on a subsequent real CLI read.
	if _, err := h.run(h.adminEnv(), "", "enroll", "--name", "message-admin"); err != nil {
		t.Fatal(err)
	}
	adminBus := bus(h.admin)
	adminSession := "message-admin-" + h.nonce
	if _, err := adminBus.Poll(t.Context(), busproto.PollRequest{Sessions: []busproto.PresenceSession{{SessionID: adminSession, Agent: "codex", Repo: repos[0], Remote: remote, Main: repos[0], Busy: true}}}); err != nil {
		t.Fatal(err)
	}
	held, err := adminBus.Send(t.Context(), busproto.SendRequest{FromSession: adminSession, To: sessions[1], Body: "Held external message " + h.nonce})
	if err != nil || held.State != busproto.StateHeld {
		t.Fatalf("held fixture: %v %+v", err, held)
	}
	if got := get(); len(got.Threads) != 2 {
		t.Fatalf("held incoming leaked: %+v", got)
	}
	cfg := readConfig(d2.cfg)
	human := cfg.API(cfg.SessionToken)
	if err := human.JSON(t.Context(), "POST", busproto.PathAccepts, busproto.AcceptRequest{Sender: "admin@e2e.test", Password: "e2e member battery staple"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := get("--thread", held.ThreadID); len(got.Messages) != 1 || got.Messages[0].ID != held.ID {
		t.Fatalf("accepted incoming unavailable: %+v", got)
	}
	if err := human.JSON(t.Context(), "DELETE", busproto.PathAccepts+"/admin@e2e.test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := get(); len(got.Threads) != 2 {
		t.Fatalf("revocation left stale visibility: %+v", got)
	}
	raw, err := h.runAt(repos[1], d2.env(), "", "messages", "--thread", held.ThreadID)
	if err == nil || raw != "" || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("revoked thread discovery: %v %s", err, raw)
	}
	r.Notes = append(r.Notes, "actual CLI: cross-device remote identity, default repo, canonical thread/preview/text, filters/cursor, held/accept/revoke boundaries")
	return repos[0], root.ThreadID, replyBody
}
