package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// One real MCP subprocess stays alive across real server-side rotations,
// credential revocation and interactive login. Run by the E2E sync workflow.
func (h *harness) checkMCPRotation(t *testing.T, d *device, needle string, r *result) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, h.cfg.bin, "mcp", "--server")
	cmd.Env = d.env()
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); cancel(); _ = cmd.Wait() }()
	responses := make(chan json.RawMessage)
	decodeErrors := make(chan error, 1)
	go func() {
		dec := json.NewDecoder(out)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				decodeErrors <- err
				return
			}
			select {
			case responses <- raw:
			case <-ctx.Done():
				return
			}
		}
	}()
	id := 0
	rpc := func(method string, params any) json.RawMessage {
		t.Helper()
		id++
		if err := json.NewEncoder(in).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		select {
		case raw := <-responses:
			var reply struct {
				ID     int
				Error  json.RawMessage
				Result json.RawMessage
			}
			if err := json.Unmarshal(raw, &reply); err != nil || reply.ID != id || reply.Error != nil {
				t.Fatalf("MCP RPC: %s (%v)", raw, err)
			}
			return reply.Result
		case err := <-decodeErrors:
			t.Fatalf("MCP exited: %v", err)
		case <-time.After(30 * time.Second):
			t.Fatal("MCP request timed out")
		}
		return nil
	}
	rpc("initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "rotation-e2e", "version": "1"}})
	io.WriteString(in, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
	call := func(tool string, args map[string]any, wantError bool) string {
		t.Helper()
		var result struct {
			IsError bool
			Content []struct{ Text string }
		}
		raw := rpc("tools/call", map[string]any{"name": tool, "arguments": args})
		if err := json.Unmarshal(raw, &result); err != nil || result.IsError != wantError || len(result.Content) == 0 {
			t.Fatalf("%s: %s (%v)", tool, raw, err)
		}
		return result.Content[0].Text
	}
	check := func() {
		t.Helper()
		body := call("flopwire_grep", map[string]any{"pattern": needle, "fixed_strings": true, "include_self": true, "format": "json"}, false)
		var page format.Page
		if err := json.Unmarshal([]byte(body), &page); err != nil || len(hitsOn(page.Hits, d)) == 0 {
			t.Fatalf("grep lost the fixture: %v %s", err, body)
		}
		address := hitsOn(page.Hits, d)[0].MessageID
		if body := call("flopwire_read", map[string]any{"address": address, "format": "json"}, false); !strings.Contains(body, needle) {
			t.Fatalf("read lost fixture: %s", body)
		}
		if body := call("flopwire_search", map[string]any{"query": needle, "include_self": true, "format": "json"}, false); !strings.Contains(body, needle) {
			t.Fatalf("search lost fixture: %s", body)
		}
		if body := call("flopwire_sessions", map[string]any{"session": d.claudeSID, "include_self": true}, false); !strings.Contains(body, d.claudeSID) {
			t.Fatalf("sessions lost fixture: %s", body)
		}
	}
	readConfig := func() client.Config {
		t.Helper()
		raw, err := os.ReadFile(d.cfg)
		var c client.Config
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	check()
	for i := range 2 {
		before := readConfig()
		if _, err := d.cli("rotate-device"); err != nil {
			t.Fatal(err)
		}
		after := readConfig()
		if before.Token == after.Token {
			t.Fatal("rotation did not replace credential")
		}
		if err := before.API(before.Token).JSON(t.Context(), "GET", "/v1/policy", nil, nil); err == nil {
			t.Fatal("server still accepts old credential")
		}
		check()
		r.Notes = append(r.Notes, fmt.Sprintf("all four tools passed after rotation %d without MCP restart", i+1))
	}
	// Revoke only the credential, preserving the device so login can renew it.
	if _, err := h.pg.Exec(t.Context(), `UPDATE credentials SET revoked_at=now(), revoke_reason='revoked' WHERE device_id=$1 AND kind='device' AND revoked_at IS NULL`, d.id); err != nil {
		t.Fatal(err)
	}
	body := call("flopwire_sessions", map[string]any{}, true)
	if !strings.Contains(body, "credential_revoked") || !strings.Contains(body, "flopwire login") {
		t.Fatalf("revocation recovery instruction missing: %s", body)
	}
	if _, err := h.run(d.env(), "e2e member battery staple\n", "login", "--server", h.cfg.server, "--fingerprint", h.cfg.pin, "--email", "gary@e2e.test"); err != nil {
		t.Fatal(err)
	}
	check()
	r.Notes = append(r.Notes, "revocation returned a login instruction; all four tools recovered after login in the same MCP process")
}
