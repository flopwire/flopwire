package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// testStarted is when this test binary began running its init: a bound
// for processStart.
var testStarted = time.Now()

func init() {
	// The test binary is far older than any hook: in-process hooks start
	// when hookCmd is called. Tests of lateness set hookStart themselves.
	hookStart = time.Now
	// The helper process of TestHookEndToEndKilledAfterTaking: take the
	// session's messages as a hook does, then die by SIGKILL before
	// printing anything.
	// With a third field, a SessionStart source, it is a SessionStart hook
	// and must have taken the standing instruction.
	if v := os.Getenv("FLOPWIRE_TEST_TAKE_AND_DIE"); v != "" {
		sock, rest, _ := strings.Cut(v, "|")
		session, start, _ := strings.Cut(rest, "|")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := agent.Call(ctx, sock, agent.Request{Op: "pending", Session: session, Start: start, HookStart: time.Now().UnixMilli()})
		if err != nil || len(resp.Messages) == 0 && start == "" || !resp.Instruct && start != "" {
			os.Exit(3)
		}
		syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
}

// processStart reads this process's creation time from the OS.
func TestProcessStart(t *testing.T) {
	got := processStart()
	if got.IsZero() || got.After(testStarted) || testStarted.Sub(got) > time.Minute {
		t.Fatalf("process start %v, test binary init at %v", got, testStarted)
	}
}

func setHookTimes(t *testing.T, start func() time.Time, late time.Duration) {
	t.Helper()
	prevStart, prevLate := hookStart, hookLate
	hookStart, hookLate = start, late
	t.Cleanup(func() { hookStart, hookLate = prevStart, prevLate })
}

// The hook confirms what it printed, after the write, with the printed
// ids; nothing printed, nothing confirmed.
func TestHookConfirmsAfterPrinting(t *testing.T) {
	fa := newHookAgent(t)
	fa.resp.Instruct = true
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "one", busproto.IntentInform), testEnvelope("m2", "two", busproto.IntentRequest)}
	out, errOut := runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if !strings.Contains(out, `id=\"m1\"`) && !strings.Contains(out, `id="m1"`) || errOut != "" {
		t.Fatalf("out %q err %q", out, errOut)
	}
	c := fa.requests("confirm")
	if len(c) != 1 || c[0].Session != claudeSID || !slices.Equal(c[0].IDs, []string{"m1", "m2"}) {
		t.Fatalf("confirm requests: %+v", c)
	}
	// Nothing pending: no confirm.
	fa.resp.Instruct = false
	runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if c := fa.requests("confirm"); len(c) != 1 {
		t.Fatalf("confirmed without anything printed: %+v", c)
	}
	// Only the standing instruction: confirmed, with no ids.
	fa.resp.Instruct = true
	if out, _ := runHook(t, fa.sock, claudeIn(evSessionStart), nil); !strings.Contains(out, "flopwire-instructions") {
		t.Fatalf("instruction: %q", out)
	}
	if c := fa.requests("confirm"); len(c) != 2 || !c[1].Instruction || len(c[1].IDs) != 0 {
		t.Fatalf("instruction confirm: %+v", c)
	}
}

// A write that fails confirms nothing: the messages come again.
func TestHookNoConfirmWhenTheWriteFails(t *testing.T) {
	fa := newHookAgent(t)
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "lost write", busproto.IntentInform)}
	var errOut strings.Builder
	hookCmd(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), failWriter{}, &errOut, func(string) string { return "" })
	if c := fa.requests("confirm"); len(c) != 0 {
		t.Fatalf("confirmed an output that was not written: %+v", c)
	}
	if !strings.Contains(errOut.String(), "wait for the next hook") || strings.Contains(errOut.String(), "lost write") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

// slowWriter is a stdout whose reader is gone or stuck: the write returns
// only after d.
type slowWriter struct {
	d time.Duration
	strings.Builder
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.d)
	return w.Builder.Write(p)
}

// A hook that started too long ago takes nothing; one whose print ended
// past hookLate does not confirm it. Either way it exits cleanly.
func TestHookLate(t *testing.T) {
	fa := newHookAgent(t)
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "late", busproto.IntentInform)}
	setHookTimes(t, func() time.Time { return time.Now().Add(-hookLate) }, hookLate)
	out, errOut := runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if out != "" || len(fa.requests("pending")) != 0 || !strings.Contains(errOut, "too late to deliver") {
		t.Fatalf("late hook: out %q err %q pending %v", out, errOut, fa.requests("pending"))
	}
	// The flush still goes, without the stale event: a Stop may have come
	// since, and a late PostToolUse must not mark the session busy.
	if f := waitFlush(t, fa, 1); f[0].Event != "" || f[0].Session != claudeSID {
		t.Fatalf("late flush: %+v", f[0])
	}

	setHookTimes(t, time.Now, 300*time.Millisecond)
	w := &slowWriter{d: 400 * time.Millisecond}
	var errs strings.Builder
	hookCmd(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), w, &errs, func(string) string { return "" })
	if !strings.Contains(w.String(), "late") || len(fa.requests("confirm")) != 0 || !strings.Contains(errs.String(), "too late to confirm") {
		t.Fatalf("slow print: out %q err %q confirm %v", w.String(), errs.String(), fa.requests("confirm"))
	}
}

// A confirmation the agent refuses or never answers is reported on stderr
// and changes nothing else: the output stands, the exit is clean.
func TestHookConfirmLost(t *testing.T) {
	fa := newHookAgent(t)
	fa.confirmFail = "bus.db is busy"
	fa.msgs = []busproto.Envelope{testEnvelope("m1", "BODY-MARKER", busproto.IntentInform)}
	out, errOut := runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if !strings.Contains(out, "BODY-MARKER") || !strings.Contains(errOut, "could not confirm the delivery") || strings.Contains(errOut, "BODY-MARKER") {
		t.Fatalf("out %q err %q", out, errOut)
	}
}

// leaseE2E is hookE2E with a short lease.
func leaseE2E(t *testing.T) string {
	t.Helper()
	prev := devicebus.LeaseFor
	devicebus.LeaseFor = time.Second
	t.Cleanup(func() { devicebus.LeaseFor = prev })
	return hookE2E(t)
}

// A real hook process takes the message from a real device agent and is
// killed (SIGKILL) before printing. The message is not lost: the session's
// next hook inside the lease prints nothing, the first after it prints the
// message once, marked redelivery="true", and confirms it.
func TestHookEndToEndKilledAfterTaking(t *testing.T) {
	sock := leaseE2E(t)
	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "survives a killed hook"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FLOPWIRE_TEST_TAKE_AND_DIE="+sock+"|"+e2eB)
	err := cmd.Run()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not take and die by SIGKILL: %v", err)
	}
	if o := runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0]; o != "" {
		t.Fatalf("offered again inside the lease: %q", o)
	}
	time.Sleep(devicebus.LeaseFor + 100*time.Millisecond)
	// Two hooks at once (two hook configs), then one more: one prints it.
	var c string
	for _, o := range append(runHooks(t, sock, hookFor(e2eB, evPostToolUse), 2), runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)...) {
		c += contextOf(t, o)
	}
	if strings.Count(c, "<flopwire-message ") != 1 || !strings.Contains(c, `redelivery="true"`) || !strings.Contains(c, "survives a killed hook") {
		t.Fatalf("after the lease:\n%s", c)
	}
	time.Sleep(devicebus.LeaseFor + 100*time.Millisecond)
	if o := runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0]; o != "" {
		t.Fatalf("delivered again after the confirmation: %q", o)
	}
	if out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent"); err != nil || !strings.Contains(out, "inform  delivered\n") {
		t.Fatalf("sender's inbox:\n%s %v", out, err)
	}
}

var messageTag = regexp.MustCompile(`<flopwire-message [^>]*>`)

// Every hook prints the message and every confirmation is lost: the
// message comes again, marked, devicebus.MaxAttempts times in all; then it
// is undelivered and the sender's inbox says so.
func TestHookEndToEndConfirmLost(t *testing.T) {
	sock := leaseE2E(t)
	prev := hookConfirmBudget
	hookConfirmBudget = 0 // each confirmation times out before it is sent
	t.Cleanup(func() { hookConfirmBudget = prev })
	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "printed, never confirmed"); err != nil {
		t.Fatal(err)
	}
	var marks []bool
	deadline := time.Now().Add(time.Duration(devicebus.MaxAttempts+3) * devicebus.LeaseFor)
	for time.Now().Before(deadline) {
		// The standing instruction (never confirmed either) mentions the
		// marker; look at the message's own tag.
		if tag := messageTag.FindString(contextOf(t, runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0])); tag != "" {
			marks = append(marks, strings.Contains(tag, `redelivery="true"`))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !slices.Equal(marks, []bool{false, true, true}) {
		t.Fatalf("prints (redelivery marked): %v, want %d prints, all but the first marked", marks, devicebus.MaxAttempts)
	}
	out, err := busCLI(t, sock, e2eA, "", "inbox", "--sent")
	if err != nil || !strings.Contains(out, "inform  undelivered (unconfirmed)\n") {
		t.Fatalf("sender's inbox:\n%s %v", out, err)
	}
}

// takeAndDie runs a helper hook process that takes from the agent (as a
// SessionStart hook with source start, when set) and dies by SIGKILL
// before printing.
func takeAndDie(t *testing.T, sock, session, start string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FLOPWIRE_TEST_TAKE_AND_DIE="+sock+"|"+session+"|"+start)
	err := cmd.Run()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not take and die by SIGKILL: %v", err)
	}
}

// A real SessionStart hook process takes the standing instruction (and the
// queued message) from a real device agent and is killed before printing
// (#101). The session's next hook inside the lease prints nothing; the
// first after it prints the instruction, then the message, each once; no
// later hook prints the instruction again.
func TestHookEndToEndKilledSessionStart(t *testing.T) {
	sock := leaseE2E(t)
	if _, err := busCLI(t, sock, e2eA, "", "send", "e2e0bbbb", "--", "after a killed start"); err != nil {
		t.Fatal(err)
	}
	takeAndDie(t, sock, e2eB, "startup")
	if o := runHooks(t, sock, hookFor(e2eB, evPostToolUse), 1)[0]; o != "" {
		t.Fatalf("printed inside the instruction's lease: %q", o)
	}
	time.Sleep(devicebus.LeaseFor + 100*time.Millisecond)
	c := contextOf(t, runHooks(t, sock, hookFor(e2eB, evUserPromptSubmit), 1)[0])
	i, j := strings.Index(c, busrender.StandingInstruction), strings.Index(c, "<flopwire-message id=")
	if strings.Count(c, busrender.StandingInstruction) != 1 || i != 0 || j < i || strings.Count(c, "after a killed start") != 1 {
		t.Fatalf("after the lease:\n%s", c)
	}
	time.Sleep(devicebus.LeaseFor + 100*time.Millisecond)
	for _, ev := range []string{evPostToolUse, evUserPromptSubmit, evSessionStart} {
		if o := runHooks(t, sock, hookFor(e2eB, ev), 1)[0]; o != "" {
			t.Fatalf("%s after the confirmation printed %q", ev, o)
		}
	}
}

func TestHookFailureStatusPrintAckAndPrivacy(t *testing.T) {
	fa := newHookAgent(t)
	fa.resp.Failures = []busproto.DeliveryFailure{{ID: "mfailed", Session: claudeSID, Agent: "claude", Reason: "session_ended", LeaseID: "status:lease", Attempt: 1}}
	out, errOut := runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if errOut != "" || !strings.Contains(out, "flopwire-delivery-status") || !strings.Contains(out, "mfailed") || !strings.Contains(out, "session_ended") || strings.Contains(out, "flopwire-message") {
		t.Fatalf("status output: %s stderr: %s", out, errOut)
	}
	c := fa.requests("confirm")
	if len(c) != 1 || !slices.Equal(c[0].IDs, []string{"status:lease"}) {
		t.Fatalf("status confirmation: %+v", c)
	}
	fa = newHookAgent(t)
	fa.resp.Failures = []busproto.DeliveryFailure{{ID: "mfailed", Reason: "expired", LeaseID: "status:lease", Attempt: 2}}
	var errBuf strings.Builder
	hookCmd(t.Context(), []string{"--socket", fa.sock}, strings.NewReader(claudeIn(evPostToolUse)), failWriter{}, &errBuf, func(string) string { return "" })
	if c := fa.requests("confirm"); len(c) != 0 {
		t.Fatalf("acknowledged failed print: %+v", c)
	}
	out, _ = runHook(t, fa.sock, claudeIn(evPostToolUse), nil)
	if !strings.Contains(out, "Status redelivery") {
		t.Fatalf("ambiguous print did not mark redelivery: %s", out)
	}
}

func TestHookFailureStatusSubagentIsolation(t *testing.T) {
	fa := newHookAgent(t)
	fa.resp.Failures = []busproto.DeliveryFailure{{ID: "mparent", Reason: "expired", LeaseID: "status:parent"}}
	in := strings.TrimSuffix(claudeIn(evPostToolUse), "}") + `,"agent_id":"synthetic-child"}`
	out, _ := runHook(t, fa.sock, in, nil)
	if out != "" || len(fa.requests("pending")) != 0 || len(fa.requests("confirm")) != 0 {
		t.Fatalf("subagent took parent status: %s", out)
	}
}
