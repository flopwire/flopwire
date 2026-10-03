package bus_test

import (
	"context"
	"slices"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

// cloudPoll reports the device's sessions and its person's cloud sessions.
func (f *fixture) cloudPoll(c busproto.Caller, sessions []busproto.PresenceSession, cloud ...busproto.PresenceSession) busproto.PollResponse {
	f.t.Helper()
	out, err := f.s.Poll(context.Background(), c, busproto.PollRequest{Sessions: sessions, Cloud: cloud})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func cloudSession(id, agent string, running bool) busproto.PresenceSession {
	return busproto.PresenceSession{SessionID: id, Agent: agent, Repo: "acme/api", Branch: "claude/fix", Title: "cloud task", Busy: running}
}

func (tm *team) garyMacSessions() []busproto.PresenceSession {
	return []busproto.PresenceSession{live("g-api-1111", "claude", "/Users/gary/src/api", true), live("g-web-2222", "codex", "/Users/gary/src/web", false)}
}

func claimableCloud(resp busproto.PollResponse) []string {
	var out []string
	for _, c := range resp.Claimable {
		if c.Cloud {
			out = append(out, c.Message.ID+"@"+c.Sessions[0])
		}
	}
	return out
}

// A cloud session belongs to its person, not the device that listed it:
// another device's poll does not remove it, peers marks it cloud on no
// device, and it can be addressed by id like any session.
func TestCloudSessionPresenceAndPeers(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloudSession("session_01cloudA", "claude", true))
	// gary's other device lists no cloud sessions (no Claude login there).
	tm.cloudPoll(tm.garyLinux, []busproto.PresenceSession{live("g-lin-3333", "codex", "/home/gary/api", false)})
	peers, err := tm.s.Peers(ctx, tm.alexMac, busproto.PeersQuery{Session: "a-api-4444"})
	if err != nil {
		t.Fatal(err)
	}
	var got *busproto.Peer
	for i, p := range peers.Peers {
		if p.Session == "session_01cloudA" {
			got = &peers.Peers[i]
		}
	}
	if got == nil || !got.Cloud || got.User != "gary@example.test" || got.Device != "" || !got.Busy || got.Repo != "acme/api" {
		t.Fatalf("cloud peer = %+v", got)
	}
	// A repo filter matches a cloud session by the repo's name.
	peers, _ = tm.s.Peers(ctx, tm.alexMac, busproto.PeersQuery{Repo: "/Users/alex/code/api"})
	if !slices.ContainsFunc(peers.Peers, func(p busproto.Peer) bool { return p.Session == "session_01cloudA" }) {
		t.Fatal("--repo by path does not match the cloud session on a repo of that name")
	}
	// A poll from the device that listed it, without it, leaves it too:
	// it ages out.
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions())
	r := tm.mustSend(tm.garyMac, "g-api-1111", "session_01cl", "check the cloud")
	if !r.To.Cloud || !r.To.Live || !r.To.Busy || r.State != busproto.StateQueued || r.Sender != busproto.SenderOwn {
		t.Fatalf("receipt = %+v", r)
	}
	tm.advance(busproto.PresenceTTL + 1)
	tm.presence()
	if _, err := tm.send(tm.garyMac, "g-api-1111", "session_01cl", "again"); code(err) != busproto.CodeUnknownRecipient {
		t.Fatalf("send to an aged-out cloud session: %v", err)
	}
}

// One device of the person pushes: both are offered the message, one
// claims it, the other is refused, and the claimer delivers it.
func TestCloudMessageIsClaimedByOneDevice(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	cloud := cloudSession("session_01cloudB", "claude", true)
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)
	tm.cloudPoll(tm.garyLinux, []busproto.PresenceSession{live("g-lin-3333", "codex", "/home/gary/api", false)}, cloud)
	m := tm.mustSend(tm.garyMac, "g-api-1111", "session_01cloudB", "rebase please")
	for _, c := range []busproto.Caller{tm.garyMac, tm.garyLinux} {
		if got := claimableCloud(tm.cloudPoll(c, nil, cloud)); !slices.Equal(got, []string{m.ID + "@session_01cloudB"}) {
			t.Fatalf("claimable on %s = %v", c.DeviceID, got)
		}
	}
	if got := claimableCloud(tm.present(tm.alexMac)); len(got) != 0 {
		t.Fatalf("another person was offered the cloud message: %v", got)
	}
	if _, err := tm.s.Claim(ctx, tm.alexMac, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudB"}); err == nil || tm.state(m.ID) != "queued" {
		t.Fatalf("another person's claim: %v", err)
	}
	cl, err := tm.s.Claim(ctx, tm.garyLinux, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudB", Agent: "claude"})
	if err != nil || cl.Message.ID != m.ID || cl.Message.ToSession != "session_01cloudB" {
		t.Fatalf("claim = %+v, %v", cl, err)
	}
	if _, err := tm.s.Claim(ctx, tm.garyMac, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudB"}); code(err) != busproto.CodeAlreadyClaimed {
		t.Fatalf("second device's claim: %v", err)
	}
	if again, err := tm.s.Claim(ctx, tm.garyLinux, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudB"}); err != nil || again.Message.ID != m.ID {
		t.Fatalf("claiming again on the claimer = %+v, %v", again, err)
	}
	resp := tm.cloudPoll(tm.garyLinux, nil, cloud)
	if len(resp.Messages) != 1 || resp.Messages[0].ID != m.ID || len(claimableCloud(resp)) != 0 {
		t.Fatalf("the claimer's poll = %+v", resp)
	}
	if got := claimableCloud(tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)); len(got) != 0 {
		t.Fatalf("still offered after the claim: %v", got)
	}
	ack, err := tm.s.Ack(ctx, tm.garyMac, busproto.AckRequest{IDs: []string{m.ID}})
	if err != nil || !slices.Equal(ack.Rejected, []string{m.ID}) {
		t.Fatalf("the other device's ack = %+v, %v", ack, err)
	}
	if ack, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{m.ID}}); err != nil || !slices.Equal(ack.Acked, []string{m.ID}) {
		t.Fatalf("the claimer's ack = %+v, %v", ack, err)
	}
	if s := tm.state(m.ID); s != "delivered" {
		t.Fatalf("state = %s", s)
	}
}

// A cloud session that is not live (no device listed it within
// PresenceTTL) is not offered and cannot be claimed; the message waits.
func TestCloudMessageWaitsWhileTheSessionIsNotListed(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	cloud := cloudSession("session_01cloudC", "devin", false)
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)
	m := tm.mustSend(tm.garyMac, "g-api-1111", "session_01cloudC", "when you run")
	tm.advance(busproto.PresenceTTL + 1)
	if got := claimableCloud(tm.cloudPoll(tm.garyMac, tm.garyMacSessions())); len(got) != 0 {
		t.Fatalf("offered while not listed: %v", got)
	}
	if _, err := tm.s.Claim(ctx, tm.garyMac, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudC"}); code(err) != busproto.CodeNotEligible {
		t.Fatalf("claim while not listed: %v", err)
	}
	if s := tm.state(m.ID); s != "queued" {
		t.Fatalf("state = %s", s)
	}
	if got := claimableCloud(tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)); len(got) != 1 {
		t.Fatalf("not offered again once listed: %v", got)
	}
}

// A teammate's message to a cloud session follows the accept rule (B7):
// held until the owner accepts the sender, then offered to the owner's
// devices.
func TestCloudMessageFromTeammateIsHeldUntilAccepted(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	cloud := cloudSession("session_01cloudD", "claude", true)
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)
	m := tm.mustSend(tm.alexMac, "a-api-4444", "session_01cloudD", "fyi from alex")
	if m.State != busproto.StateHeld || m.Sender != busproto.SenderTeammate || !m.To.Cloud {
		t.Fatalf("receipt = %+v", m)
	}
	if got := claimableCloud(tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)); len(got) != 0 {
		t.Fatalf("a held message was offered: %v", got)
	}
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.gary}, "alex"); err != nil {
		t.Fatal(err)
	}
	if got := claimableCloud(tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)); !slices.Equal(got, []string{m.ID + "@session_01cloudD"}) {
		t.Fatalf("after accept: claimable = %v", got)
	}
}

// An @user message never goes to a cloud session: with only a cloud
// session live, the person has no live session.
func TestUserAddressedMessageSkipsCloudSessions(t *testing.T) {
	tm := newTeam(t)
	tm.cloudPoll(tm.alexMac, nil, cloudSession("session_01cloudE", "claude", true))
	r := tm.mustSend(tm.garyMac, "g-api-1111", "@alex", "to a person", func(r *busproto.SendRequest) { r.Repo = "*" })
	if r.To.Live || r.To.Busy {
		t.Fatalf("@alex counted a cloud session as live: %+v", r.To)
	}
	if got := tm.cloudPoll(tm.alexMac, nil, cloudSession("session_01cloudE", "claude", true)); len(got.Claimable) != 0 {
		t.Fatalf("an @user message was offered for a cloud session: %+v", got.Claimable)
	}
}

// A device cannot list another person's cloud session as its person's.
func TestCloudPresenceIgnoresAnotherPersonsSession(t *testing.T) {
	tm := newTeam(t)
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloudSession("session_01cloudF", "claude", true))
	resp := tm.cloudPoll(tm.alexMac, nil, cloudSession("session_01cloudF", "claude", false))
	if !slices.Equal(resp.Ignored, []string{"session_01cloudF"}) {
		t.Fatalf("ignored = %v", resp.Ignored)
	}
	r := tm.mustSend(tm.garyMac, "g-api-1111", "session_01cloudF", "x")
	if r.To.User != "gary@example.test" || !r.To.Busy {
		t.Fatalf("the cloud session changed hands: %+v", r.To)
	}
}

// A push that failed on every attempt is reported push_failed, and the
// sender sees it.
func TestCloudPushFailedIsReported(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	cloud := cloudSession("session_01cloudG", "claude", true)
	tm.cloudPoll(tm.garyMac, tm.garyMacSessions(), cloud)
	m := tm.mustSend(tm.garyMac, "g-api-1111", "session_01cloudG", "x")
	if _, err := tm.s.Claim(ctx, tm.garyMac, busproto.ClaimRequest{MessageID: m.ID, SessionID: "session_01cloudG"}); err != nil {
		t.Fatal(err)
	}
	ack, err := tm.s.Ack(ctx, tm.garyMac, busproto.AckRequest{PushFailed: []string{m.ID}})
	if err != nil || !slices.Equal(ack.Acked, []string{m.ID}) {
		t.Fatalf("ack = %+v, %v", ack, err)
	}
	in, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Messages) != 1 || in.Messages[0].State != busproto.StateUndelivered || in.Messages[0].Reason != busproto.ReasonPushFailed {
		t.Fatalf("sender's inbox = %+v", in.Messages)
	}
}
