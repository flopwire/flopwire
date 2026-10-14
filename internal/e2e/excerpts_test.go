package e2e

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// The referenced transcript exists only on the other device. Both index
// modes must deliver the same line through the actual CLI hook and TLS API.
func (h *harness) checkMessageExcerpts(t *testing.T, sender, recipient *device, r *result) {
	t.Helper()
	needle := "remote excerpt synthetic " + h.nonce
	line := "second synthetic line " + h.nonce
	appendFile(t, sender.claudePath, sender.claudeUser(needle+"\n"+line))
	var hit format.Hit
	eventually(t, time.Minute, 100*time.Millisecond, "reference uploaded", func() (bool, error) {
		hits, err := sender.serverFind(needle)
		for _, candidate := range hitsOn(hits, sender) {
			if candidate.SessionID == sender.claudeSID {
				hit = candidate
				return true, err
			}
		}
		return false, err
	})
	ref := fmt.Sprintf("%s/%d:2", hit.SessionID, hit.Ordinal)
	load := func(path string) client.Config {
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
	original, err := os.ReadFile(recipient.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		recipient.stop(false)
		if err := os.WriteFile(recipient.cfg, original, 0600); err != nil {
			t.Error(err)
			return
		}
		recipient.start()
	}()
	for _, mode := range []string{"full", "sync-only"} {
		if mode == "sync-only" {
			recipient.stop(false)
			cfg := load(recipient.cfg)
			cfg.Mode = mode
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(recipient.cfg, raw, 0600); err != nil {
				t.Fatal(err)
			}
			recipient.start()
		}
		eventually(t, time.Minute, 100*time.Millisecond, mode+" agent ready", func() (bool, error) {
			resp, err := agent.Call(t.Context(), recipient.sock, agent.Request{Op: "status"})
			return err == nil && resp.OK && resp.Bus != nil, err
		})
		db, err := sql.Open("sqlite", "file:"+recipient.index+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		var copies int
		err = db.QueryRowContext(t.Context(), "SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.session_id=?", sender.claudeSID).Scan(&copies)
		db.Close()
		if err != nil || copies != 0 {
			t.Fatalf("%s recipient holds %d referenced-session messages: %v", mode, copies, err)
		}
		cfg := load(sender.cfg)
		bus := client.Bus{Server: cfg.Server, Token: cfg.Token, HTTP: cfg.HTTPClient()}
		sent, err := bus.Send(t.Context(), busproto.SendRequest{FromSession: sender.claudeSID, To: recipient.claudeSID, Body: "Inspect remote synthetic reference " + mode, Refs: []string{ref}})
		if err != nil {
			t.Fatal(err)
		}
		if sent.State != busproto.StateQueued || sent.To.Session != recipient.claudeSID || sent.To.Agent != "claude" {
			t.Fatalf("%s reference routing: %+v", mode, sent)
		}
		inbox, err := sql.Open("sqlite", "file:"+filepath.Join(filepath.Dir(recipient.cfg), "bus.db")+"?mode=ro&_pragma=busy_timeout(1000)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { inbox.Close() })
		// Status.Pending counts every session's messages and can be nonzero
		// before this send reaches the restarted recipient's local inbox.
		eventually(t, time.Minute, 100*time.Millisecond, mode+" message queued", func() (bool, error) {
			var count int
			err := inbox.QueryRowContext(t.Context(), `SELECT count(*) FROM devbus_messages
				WHERE id=? AND to_session=? AND to_agent='claude' AND from_session=?
				AND state='queued' AND expires_at>?`, sent.ID, recipient.claudeSID, sender.claudeSID, time.Now().UnixMilli()).Scan(&count)
			return count == 1, err
		})
		input, _ := json.Marshal(map[string]string{"session_id": recipient.claudeSID, "transcript_path": recipient.claudePath, "hook_event_name": "PostToolUse", "cwd": "/tmp/e2e-growth"})
		start := time.Now()
		raw, err := h.run(recipient.env(), string(input), "hook", "--socket", recipient.sock)
		elapsed := time.Since(start)
		r.Timings[mode+"_hook"] = ms(elapsed)
		if err != nil {
			t.Fatal(err)
		}
		var output struct {
			Hook struct {
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(raw), &output); err != nil {
			t.Fatalf("%s hook: %v %s", mode, err, raw)
		}
		want := `<flopwire-ref address="` + ref + `">user: ` + line + `</flopwire-ref>`
		if !strings.Contains(output.Hook.Context, sent.ID) || !strings.Contains(output.Hook.Context, want) {
			t.Fatalf("%s remote line excerpt missing: want %s; hook %s", mode, want, raw)
		}
		var state string
		err = inbox.QueryRowContext(t.Context(), `SELECT state FROM devbus_messages WHERE id=? AND to_session=? AND to_agent='claude'`, sent.ID, recipient.claudeSID).Scan(&state)
		inbox.Close()
		if err != nil || state != "delivered" {
			t.Fatalf("%s exact message receipt: state=%s: %v", mode, state, err)
		}
		if mode == "sync-only" {
			db, err := sql.Open("sqlite", "file:"+recipient.index+"?mode=ro")
			if err != nil {
				t.Fatal(err)
			}
			var count int
			err = db.QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&count)
			db.Close()
			if err != nil || count != 0 {
				t.Fatalf("sync-only messages=%d: %v", count, err)
			}
			for _, pattern := range []string{recipient.index + "-tok*", recipient.index + "-tri*"} {
				shards, err := filepath.Glob(pattern)
				if err != nil || len(shards) != 0 {
					t.Fatalf("sync-only search shards: %v: %v", shards, err)
				}
			}
			r.Notes = append(r.Notes, "sync-only messages=0, search shards absent")
		}
	}
}
