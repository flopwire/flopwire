package main

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

type tapTestWriter struct {
	mode        string
	wrote       bool
	beforeWrite func()
}

func (w *tapTestWriter) Write(p []byte) (int, error) {
	if w.beforeWrite != nil {
		w.beforeWrite()
	}
	w.wrote = true
	switch w.mode {
	case "error":
		return 0, errors.New("synthetic closed output")
	case "partial":
		return len(p) / 2, nil
	default:
		return len(p), nil
	}
}

func TestProbeTapPreservesOutputConfirmation(t *testing.T) {
	for _, tapped := range []bool{false, true} {
		for _, mode := range []string{"success", "error", "partial"} {
			t.Run(mode+map[bool]string{false: "/direct", true: "/tap"}[tapped], func(t *testing.T) {
				fa := newHookAgent(t)
				fa.msgs = []busproto.Envelope{testEnvelope("m1", "synthetic marker", busproto.IntentInform)}
				w := &tapTestWriter{mode: mode, beforeWrite: func() {
					if len(fa.requests("confirm")) != 0 {
						t.Error("confirmed before downstream write")
					}
				}}
				path := filepath.Join(t.TempDir(), "tap.jsonl")
				var stderr strings.Builder
				if tapped {
					if err := probeTap(t.Context(), []string{"--log", path, "--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), w, &stderr); err != nil {
						t.Fatal(err)
					}
				} else {
					hookCmd(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), w, &stderr, func(string) string { return "" })
				}
				if tapped {
					entries, err := readTap(path)
					if err != nil || len(entries) != 1 {
						t.Fatalf("tap entries: %+v %v", entries, err)
					}
					e := entries[0]
					wantStatus := map[string]string{"success": "written", "error": "error", "partial": "short_write"}[mode]
					if e.OutputStatus != wantStatus {
						t.Fatalf("output status=%q want %q", e.OutputStatus, wantStatus)
					}
					if mode == "success" {
						if len(e.Printed) != 1 || e.Printed[0] != "m1" || e.OutputBytes == 0 {
							t.Fatalf("successful output: %+v", e)
						}
					} else if len(e.Printed) != 0 || e.Instruction {
						t.Fatalf("failed output claimed printed context: %+v", e)
					}
				}
				want := 0
				if mode == "success" {
					want = 1
				}
				if got := len(fa.requests("confirm")); got != want {
					t.Fatalf("confirms=%d want %d; write attempted=%v stderr=%q", got, want, w.wrote, stderr.String())
				}
			})
		}
	}
}

var _ io.Writer = (*tapTestWriter)(nil)

// Printed is evidence of a completed stdout write, not a confirmation receipt
// or harness context entry. A failed confirmation must remain visible on stderr.
func TestProbeTapConfirmFailureRemainsVisible(t *testing.T) {
	for _, tapped := range []bool{false, true} {
		for _, timeout := range []bool{false, true} {
			name := map[bool]string{false: "error", true: "timeout"}[timeout] + map[bool]string{false: "/direct", true: "/tap"}[tapped]
			t.Run(name, func(t *testing.T) {
				fa := newHookAgent(t)
				fa.msgs = []busproto.Envelope{testEnvelope("m1", "synthetic marker", busproto.IntentInform)}
				if timeout {
					fa.confirmDelay = hookConfirmBudget + 50*time.Millisecond
				} else {
					fa.confirmFail = "synthetic confirm failure"
				}
				var out, stderr strings.Builder
				if tapped {
					if err := probeTap(t.Context(), []string{"--log", filepath.Join(t.TempDir(), "tap.jsonl"), "--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), &out, &stderr); err != nil {
						t.Fatal(err)
					}
				} else {
					hookCmd(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), &out, &stderr, func(string) string { return "" })
				}
				if out.Len() == 0 || len(fa.requests("confirm")) != 1 || !strings.Contains(stderr.String(), "could not confirm the delivery") {
					t.Fatalf("output=%q confirms=%d stderr=%q", out.String(), len(fa.requests("confirm")), stderr.String())
				}
				if timeout && !strings.Contains(stderr.String(), "no answer within") {
					t.Fatalf("timeout status: %q", stderr.String())
				}
			})
		}
	}
}
