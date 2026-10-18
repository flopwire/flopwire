package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func diagnosticFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	cfg := hookDiagnosticConfig{Created: time.Now().Add(-time.Second), Expires: time.Now().Add(9 * time.Minute), Salt: strings.Repeat("ab", 32)}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHookDiagnosticOutputParityAndPrivacy(t *testing.T) {
	for _, mode := range []string{"success", "error", "partial"} {
		t.Run(mode, func(t *testing.T) {
			dir := diagnosticFixture(t)
			d := openHookDiagnostic(dir, time.Now())
			if d == nil {
				t.Fatal("diagnostic unavailable")
			}
			fa := newHookAgent(t)
			fa.msgs = []busproto.Envelope{testEnvelope("private-message-id", "SECRET-BODY", busproto.IntentInform)}
			var stderr strings.Builder
			w := &tapTestWriter{mode: mode, beforeWrite: func() {
				if len(fa.requests("confirm")) != 0 {
					t.Error("confirmed before real output")
				}
			}}
			hookCmdObserved(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), w, &stderr, func(string) string { return "" }, time.Now(), d)
			want := 0
			if mode == "success" {
				want = 1
			}
			if len(fa.requests("confirm")) != want {
				t.Fatal("confirmation parity failed")
			}
			b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET-BODY", "private-message-id", claudeSID, "/tmp/", "transcript", "synthetic closed"} {
				if strings.Contains(string(b), secret) {
					t.Fatalf("private data %q in diagnostic", secret)
				}
			}
			var e hookObservation
			if json.Unmarshal(b, &e) != nil {
				t.Fatal("invalid entry")
			}
			if mode == "success" {
				if e.Reason != "printed" || e.ConfirmResult != "confirmed" {
					t.Fatalf("entry %+v", e)
				}
			} else if e.Reason != "output_error" || e.ConfirmResult != "" {
				t.Fatalf("entry %+v", e)
			}
		})
	}
}

func TestHookDiagnosticBoundsAndLock(t *testing.T) {
	for _, mode := range []string{"count", "bytes", "expired", "lock"} {
		t.Run(mode, func(t *testing.T) {
			dir := diagnosticFixture(t)
			d := openHookDiagnostic(dir, time.Now())
			if d == nil {
				t.Fatal("diagnostic unavailable")
			}
			p := filepath.Join(dir, "events.jsonl")
			switch mode {
			case "count":
				os.WriteFile(p, []byte(strings.Repeat("{}\n", 256)), 0600)
			case "bytes":
				os.WriteFile(p, []byte(strings.Repeat("x", diagnosticMaxBytes)), 0600)
			case "expired":
				d.cfg.Expires = time.Now().Add(-time.Second)
			case "lock":
				f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			before, _ := os.ReadFile(p)
			d.finish(time.Now())
			after, _ := os.ReadFile(p)
			if string(before) != string(after) {
				t.Fatal("cap/error appended data")
			}
			if _, err := os.Stat(filepath.Join(dir, "unavailable")); err != nil {
				t.Fatal("missing unavailable status")
			}
			if openHookDiagnostic(dir, time.Now()) != nil {
				t.Fatal("disabled run reopened")
			}
		})
	}
}

func TestHookDiagnosticAllowlistedEventAndAlias(t *testing.T) {
	dir := diagnosticFixture(t)
	d := openHookDiagnostic(dir, time.Now())
	if d == nil {
		t.Fatal("unavailable")
	}
	d.reason("SECRET-REASON")
	if d.entry.Reason != "internal_error" {
		t.Fatal("nonallowlisted reason")
	}
	d.input(hookInput{Event: "SECRET-EVENT", SessionID: "SECRET-SESSION"})
	if d.entry.Event != "other" || d.entry.Session == "SECRET-SESSION" {
		t.Fatal("unaliased input")
	}
	other := openHookDiagnostic(diagnosticFixture(t), time.Now())
	other.salt = []byte(strings.Repeat("z", 32))
	if d.alias("session", "id") == other.alias("session", "id") {
		t.Fatal("alias stable across runs")
	}
}

func TestHookDiagnosticReasonsAndOriginalDeadline(t *testing.T) {
	for _, scenario := range []string{"invalid_input", "no_session", "non_delivery_event", "empty", "pending_error", "pending_timeout", "confirm_error", "confirm_timeout", "late"} {
		t.Run(scenario, func(t *testing.T) {
			started := time.Now()
			if scenario == "late" {
				started = started.Add(-hookLate)
			}
			// Setup runs after the original timestamp. Passing that same timestamp
			// into the hook must preserve its late decision.
			d := openHookDiagnostic(diagnosticFixture(t), time.Now())
			if d == nil {
				t.Fatal("unavailable")
			}
			fa := newHookAgent(t)
			input := claudeIn(evPostToolUse)
			switch scenario {
			case "invalid_input":
				input = "not json"
			case "no_session":
				input = `{"hook_event_name":"PostToolUse"}`
			case "non_delivery_event":
				input = claudeIn("Stop")
			case "pending_error":
				fa.fail = "synthetic pending error"
			case "pending_timeout":
				fa.delay = hookPendingBudget + 50*time.Millisecond
			case "confirm_error", "confirm_timeout":
				fa.msgs = []busproto.Envelope{testEnvelope("private-id", "private body", busproto.IntentInform)}
				if scenario == "confirm_error" {
					fa.confirmFail = "synthetic confirm error"
				} else {
					fa.confirmDelay = hookConfirmBudget + 50*time.Millisecond
				}
			}
			var out, stderr strings.Builder
			hookCmdObserved(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(input), &out, &stderr, func(string) string { return "" }, started, d)
			if d.entry.Reason != scenario {
				t.Fatalf("reason %q want %q; stderr=%q", d.entry.Reason, scenario, stderr.String())
			}
			if scenario == "late" && len(fa.requests("pending")) != 0 {
				t.Fatal("diagnostic reset original late deadline")
			}
		})
	}
}

func TestHookDiagnosticRejectsUnsafeConfig(t *testing.T) {
	for _, mode := range []string{"directory_mode", "config_mode", "too_long", "bad_salt", "config_symlink", "events_symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := diagnosticFixture(t)
			p := filepath.Join(dir, "config.json")
			switch mode {
			case "directory_mode":
				os.Chmod(dir, 0755)
			case "config_mode":
				os.Chmod(p, 0644)
			case "too_long", "bad_salt":
				b, _ := os.ReadFile(p)
				var cfg hookDiagnosticConfig
				json.Unmarshal(b, &cfg)
				if mode == "too_long" {
					cfg.Expires = cfg.Created.Add(11 * time.Minute)
				} else {
					cfg.Salt = "bad"
				}
				b, _ = json.Marshal(cfg)
				os.WriteFile(p, b, 0600)
			case "config_symlink":
				b, _ := os.ReadFile(p)
				target := filepath.Join(t.TempDir(), "private-config")
				os.WriteFile(target, b, 0600)
				os.Remove(p)
				os.Symlink(target, p)
			case "events_symlink":
				target := filepath.Join(t.TempDir(), "target")
				os.WriteFile(target, []byte("untouched"), 0600)
				os.Symlink(target, filepath.Join(dir, "events.jsonl"))
				d := openHookDiagnostic(dir, time.Now())
				if d == nil {
					t.Fatal("unavailable before append")
				}
				d.finish(time.Now())
				b, _ := os.ReadFile(target)
				if string(b) != "untouched" {
					t.Fatal("symlink target modified")
				}
				return
			}
			if openHookDiagnostic(dir, time.Now()) != nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
}
