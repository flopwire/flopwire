package main

import (
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

type queueSession struct {
	probeSession
	id string
}

func (s queueSession) ID() string { return s.id }

func TestProbeObservedCaseIsOptInAndLocal(t *testing.T) {
	defaults, err := parseProbeFlags(nil, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range defaults.cases {
		if c == casePromptSubmitObserved {
			t.Fatal("observation changed the default send/prompt timing")
		}
	}
	for _, local := range []bool{false, true} {
		args := []string{"--case", casePromptSubmitObserved}
		if local {
			args = append(args, "--local")
		}
		_, err := parseProbeFlags(args, &strings.Builder{})
		if (err == nil) != local {
			t.Fatalf("local=%v err=%v", local, err)
		}
	}
}

// These real local-agent controls establish what the new observation can say.
// Recipient row visibility is recorded independently of the send receipt. A
// visible queued row is distinct from output and native harness context.
func TestProbeLocalInboxBoundary(t *testing.T) {
	sock, home := hookE2EHome(t)
	path := writeCodexRollout(t, home, e2eCodexParent, "", "/tmp/e2e-api", "synthetic recipient")
	if _, err := agent.Call(t.Context(), sock, agent.Request{Op: "flush", Agent: "codex", Session: e2eCodexParent, Path: path}); err != nil {
		t.Fatal(err)
	}
	waitPeer(t, sock, e2eA, e2eCodexParent)
	r := &harnessRun{p: &prober{o: probeOpts{local: true}, sock: sock}, name: transcript.AgentCodex, recv: queueSession{id: e2eCodexParent}}
	check := func(id, state string) {
		t.Helper()
		o := r.observeLocalInbox(t.Context(), id)
		if o.State != state || o.Started.IsZero() || o.Finished.Before(o.Started) {
			t.Fatalf("snapshot: %+v, want %s", o, state)
		}
	}
	check("not-imported-yet", "missing")
	sent, err := agent.Call(t.Context(), sock, agent.Request{Op: "send", Send: &busproto.SendRequest{
		FromSession: e2eA, FromAgent: "claude", To: e2eCodexParent, Intent: "inform", Body: "synthetic boundary marker",
	}})
	if err != nil || sent.Sent == nil {
		t.Fatalf("send: %+v %v", sent, err)
	}
	id := sent.Sent.ID
	check(id, "queued")
	// The same message is absent from another recipient's view.
	r.recv = queueSession{id: e2eCodexChild}
	check(id, "missing")
	r.recv = queueSession{id: e2eCodexParent}
	check(id, "queued")
	// Observation must not take or confirm the message. The delivery hook still
	// prints it once. This is output evidence, not a native transcript oracle.
	in := codexHookIn(e2eCodexParent, path, "", evUserPromptSubmit)
	out := contextOf(t, runHooks(t, sock, in, 1)[0])
	if !strings.Contains(out, `id="`+id+`"`) || !strings.Contains(out, "synthetic boundary marker") {
		t.Fatalf("hook did not retain the observed message: %q", out)
	}
	check(id, "delivered")
	if out := runHooks(t, sock, in, 1)[0]; out != "" {
		t.Fatalf("duplicate output: %q", out)
	}
}

func TestProbeObservedVerdictKeepsQueueAndContextIndependent(t *testing.T) {
	for _, state := range []string{"queued", "missing", "unavailable", "delivered"} {
		for _, native := range []bool{false, true} {
			d := unseen
			if native {
				d = seen
			}
			v := verdictPromptSubmit([]tapEntry{at(1, evUserPromptSubmit, printed(pID))}, pSess, pID, pMarker, "", d)
			v.localInbox(probeLocalInbox{Started: time.Now(), Finished: time.Now(), State: state})
			r := v.result(probeResult{})
			if r.Pass != (state == "queued" && native) {
				t.Fatalf("state=%s native=%v: %+v", state, native, r)
			}
		}
	}
}
