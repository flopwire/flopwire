package devicebus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/vendorcloud"
)

// TestLiveCloud checks cloud delivery against a real vendor cloud session
// with the device's own vendor login, without a server. It runs only when
// FLOPWIRE_CLOUD_LIVE names the session ("claude:session_…" or
// "devin:devin-…"), and it pushes into that session: use a scratch session
// on the cheapest model. Results are recorded in
// notes/message-bus/cloud-2026-10-03.md.
//
//  1. Discovery lists the session.
//  2. While it runs no turn, a message stays queued (nothing is pushed).
//  3. The test starts a turn in it (as its human would), and the message
//     is pushed into the running turn and delivered; where the vendor
//     shows it (Claude's session events, Devin's echo), it is read.
func TestLiveCloud(t *testing.T) {
	spec := os.Getenv("FLOPWIRE_CLOUD_LIVE")
	if spec == "" {
		t.Skip("FLOPWIRE_CLOUD_LIVE is not set")
	}
	agentName, id, ok := cutSpec(spec)
	if !ok {
		t.Fatalf("FLOPWIRE_CLOUD_LIVE=%q: want claude:ID or devin:ID", spec)
	}
	os.Unsetenv("FLOPWIRE_CLOUD")
	var a vendorcloud.Adapter
	for _, x := range vendorcloud.Default() {
		if x.Agent() == agentName {
			a = x
		}
	}
	if a == nil {
		t.Fatalf("no %s CLI on PATH", agentName)
	}
	cfg := testConfig(nil, nil)
	cfg.Cloud, cfg.CloudEvery, cfg.PresenceEvery = []vendorcloud.Adapter{a}, 5*time.Second, time.Second
	p := &presenceSrc{}
	p.set(sess("live0000-sender", "claude", "/src/api", true))
	b := openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, p)
	run(t, b)
	deadline := time.Now().Add(3 * time.Minute)
	find := func() (Session, bool) { return b.cloudSession(id, agentName) }
	for {
		if _, ok := find(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("1. discovery: the session was not listed")
		}
		time.Sleep(time.Second)
	}
	s, _ := find()
	t.Logf("1. discovery: listed (cloud=%v running=%v repo=%q)", s.Cloud, s.Busy, s.Repo)
	if s.Busy {
		t.Fatal("the session is running a turn; start the check while it is idle")
	}
	marker := "LIVE-" + time.Now().UTC().Format("150405")
	out, err := b.Send(ctx, busproto.SendRequest{FromSession: "live0000-sender", To: id, Intent: "request",
		Body: "Flopwire live check " + marker + ". When you see this, reply to your user with the marker " + marker + " and the sender and intent attributes of the tag it came in."})
	if err != nil {
		t.Fatal(err)
	}
	if !out.To.Cloud || out.To.Busy {
		t.Fatalf("receipt: %+v", out.To)
	}
	time.Sleep(30 * time.Second)
	if st := cloudSent(t, b, "live0000-sender", out.ID).State; st != busproto.StateQueued {
		t.Fatalf("2. while idle the message is %s, want queued", st)
	}
	t.Log("2. idle: the message stayed queued for 30 s")
	task := "Run this shell command in the foreground and wait for it to finish: for i in 1 2 3 4 5 6 7 8 9; do date; sleep 10; done. Then say DONE."
	if _, err := a.Push(ctx, id, task); err != nil {
		t.Fatalf("starting a turn: %v", err)
	}
	t.Log("3. started a turn in the session")
	deadline = time.Now().Add(4 * time.Minute)
	for {
		st := cloudSent(t, b, "live0000-sender", out.ID)
		if st.State == busproto.StateDelivered || st.State == busproto.StateRead {
			t.Logf("3. %s at %s", st.State, st.DeliveredAt.Format(time.RFC3339))
			break
		}
		if st.State != busproto.StateQueued || time.Now().After(deadline) {
			t.Fatalf("3. the message is %s (%s), want delivered", st.State, st.Reason)
		}
		time.Sleep(time.Second)
	}
	deadline = time.Now().Add(5 * time.Minute)
	for {
		st := cloudSent(t, b, "live0000-sender", out.ID)
		if st.State == busproto.StateRead {
			t.Logf("3. read at %s (%s after delivery)", st.ReadAt.Format(time.RFC3339), st.ReadAt.Sub(*st.DeliveredAt).Round(time.Second))
			break
		}
		if time.Now().After(deadline) {
			t.Log("3. no read receipt within 5 minutes")
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("marker %s message %s", marker, out.ID)
}

func cutSpec(spec string) (agent, id string, ok bool) {
	agent, id, ok = strings.Cut(spec, ":")
	return agent, id, ok && agent != "" && id != ""
}
