package bus_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
	s    *bus.Store
	mu   sync.Mutex
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, pool: pool, now: time.Now().UTC().Truncate(time.Second)}
	f.s = &bus.Store{Pool: pool, Now: func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}}
	return f
}

func (f *fixture) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func (f *fixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

// user creates a person; name is the email's local part.
func (f *fixture) user(name string) string {
	id := uuid.NewString()
	f.exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,$2,$3,'member','human',now())`, id, name+"@example.test", strings.ToUpper(name[:1])+name[1:])
	return id
}

// device enrolls a device for the person and returns it as a caller.
func (f *fixture) device(userID string) busproto.Caller {
	id := uuid.NewString()
	f.exec(`INSERT INTO devices(id,user_id,name,platform,created_at) VALUES($1,$2,'laptop','darwin',now())`, id, userID)
	return busproto.Caller{UserID: userID, DeviceID: id}
}

// present reports the device's live sessions (a poll that does not wait).
func (f *fixture) present(c busproto.Caller, sessions ...busproto.PresenceSession) busproto.PollResponse {
	f.t.Helper()
	out, err := f.s.Poll(context.Background(), c, busproto.PollRequest{Sessions: sessions})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

// uploaded records a transcript the device uploaded.
func (f *fixture) uploaded(c busproto.Caller, agent, session, repo string) {
	f.exec(`INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root,title,branches) VALUES($1,$2,$3,$4,$5,$6,'uploaded title','{main}')`,
		uuid.NewString(), agent, session, c.DeviceID, c.UserID, repo)
}

func live(id, agent, repo string, busy bool) busproto.PresenceSession {
	return busproto.PresenceSession{SessionID: id, Agent: agent, Repo: repo, Branch: "main", Busy: busy}
}

func (f *fixture) send(c busproto.Caller, from, to, body string, opts ...func(*busproto.SendRequest)) (busproto.SendResponse, error) {
	req := busproto.SendRequest{FromSession: from, To: to, Body: body}
	for _, o := range opts {
		o(&req)
	}
	return f.s.Send(context.Background(), c, req)
}

func (f *fixture) mustSend(c busproto.Caller, from, to, body string, opts ...func(*busproto.SendRequest)) busproto.SendResponse {
	f.t.Helper()
	out, err := f.send(c, from, to, body, opts...)
	if err != nil {
		f.t.Fatalf("send %s -> %s: %v", from, to, err)
	}
	return out
}

func replyTo(id string) func(*busproto.SendRequest) {
	return func(r *busproto.SendRequest) { r.ReplyTo = id }
}

func intent(i busproto.Intent) func(*busproto.SendRequest) {
	return func(r *busproto.SendRequest) { r.Intent = string(i) }
}

func code(err error) string {
	var be *busproto.Error
	if errors.As(err, &be) {
		return be.Code
	}
	if err != nil {
		return "error: " + err.Error()
	}
	return ""
}

func (f *fixture) auditCount(action string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action=$1`, action).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) state(id string) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT state FROM bus_messages WHERE id=$1`, id).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// team is two people (gary and alex), gary with two devices, each device
// with live sessions.
type team struct {
	*fixture
	gary, alex         string
	garyMac, garyLinux busproto.Caller
	alexMac            busproto.Caller
}

func newTeam(t *testing.T) *team {
	f := newFixture(t)
	tm := &team{fixture: f, gary: f.user("gary"), alex: f.user("alex")}
	tm.garyMac, tm.garyLinux, tm.alexMac = f.device(tm.gary), f.device(tm.gary), f.device(tm.alex)
	tm.presence()
	return tm
}

// presence reports every device's sessions: gary's g-api (busy) and
// g-web on his mac, g-lin on linux; alex's a-api (idle).
func (tm *team) presence() {
	tm.present(tm.garyMac, live("g-api-1111", "claude", "/Users/gary/src/api", true), live("g-web-2222", "codex", "/Users/gary/src/web", false))
	tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false))
	tm.present(tm.alexMac, live("a-api-4444", "claude", "/Users/alex/code/api", false))
}

func TestSendToSessionPollAckInbox(t *testing.T) {
	tm := newTeam(t)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "Heads-up: pagination is changing.\nUse the cursor.", intent(busproto.IntentRequest))
	if out.State != busproto.StateQueued || out.To.Session != "g-lin-3333" || out.To.Agent != "codex" || out.To.User != "gary@example.test" ||
		!out.To.Live || out.To.Busy || out.Sender != busproto.SenderOwn || out.Intent != busproto.IntentRequest || out.ThreadID != out.ID ||
		!out.ExpiresAt.Equal(out.Sent.Add(busproto.DefaultTTL)) || !strings.HasPrefix(out.ID, "m") {
		t.Fatalf("send: %+v", out)
	}
	// Only the device holding the session receives it.
	if got := tm.present(tm.garyMac, live("g-api-1111", "claude", "/Users/gary/src/api", true)); len(got.Messages) != 0 {
		t.Fatalf("mac got %+v", got.Messages)
	}
	got := tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false))
	if len(got.Messages) != 1 {
		t.Fatalf("linux got %+v", got)
	}
	e := got.Messages[0]
	if e.ID != out.ID || e.From != "g-api-1111" || e.FromAgent != "claude" || e.User != "gary@example.test" || e.Repo != "/Users/gary/src/api" ||
		e.Branch != "main" || e.Sender != "own" || e.Intent != "request" || e.ToSession != "g-lin-3333" || e.Sent.IsZero() || got.Cursor != e.Seq {
		t.Fatalf("envelope %+v", e)
	}
	ack, err := tm.s.Ack(context.Background(), tm.garyMac, busproto.AckRequest{IDs: []string{out.ID}})
	if err != nil || len(ack.Rejected) != 1 {
		t.Fatalf("another device acked: %+v %v", ack, err)
	}
	ack, err = tm.s.Ack(context.Background(), tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID, out.ID, "mnope"}})
	if err != nil || len(ack.Acked) != 1 || ack.Acked[0] != out.ID || len(ack.Rejected) != 1 {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if again, err := tm.s.Ack(context.Background(), tm.garyLinux, busproto.AckRequest{IDs: []string{out.ID}}); err != nil || len(again.Acked) != 1 {
		t.Fatalf("ack again: %+v %v", again, err)
	}
	if got := tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false)); len(got.Messages) != 0 {
		t.Fatalf("delivered message polled again: %+v", got.Messages)
	}
	in, err := tm.s.Inbox(context.Background(), tm.garyLinux, busproto.InboxQuery{Session: "g-lin-3333"})
	if err != nil || len(in.Messages) != 1 || in.Messages[0].Direction != "received" || in.Messages[0].State != busproto.StateDelivered || in.Messages[0].DeliveredAt == nil {
		t.Fatalf("recipient inbox %+v %v", in, err)
	}
	sent, err := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true})
	if err != nil || len(sent.Messages) != 1 || sent.Messages[0].Direction != "sent" || sent.Messages[0].State != busproto.StateDelivered {
		t.Fatalf("sender inbox %+v %v", sent, err)
	}
	if n := tm.auditCount("bus.send"); n != 1 {
		t.Fatalf("bus.send audits %d", n)
	}
	if n := tm.auditCount("bus.deliver"); n != 3 {
		t.Fatalf("bus.deliver audits %d", n)
	}
}

func TestSenderMustBeOnTheCallingDevice(t *testing.T) {
	tm := newTeam(t)
	// Another device's session, even the same person's.
	if _, err := tm.send(tm.garyLinux, "g-api-1111", "a-api", "hi"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("other device's session: %v", err)
	}
	if _, err := tm.send(tm.alexMac, "g-api-1111", "g-lin", "hi"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("other person's session: %v", err)
	}
	// A session the device uploaded but no longer reports live.
	tm.uploaded(tm.garyLinux, "codex", "g-old-5555", "/home/gary/api")
	if out, err := tm.send(tm.garyLinux, "g-old-5555", "g-api", "from an uploaded session"); err != nil || out.State != busproto.StateQueued {
		t.Fatalf("uploaded session: %+v %v", out, err)
	}
	// Presence goes stale after PresenceTTL.
	tm.advance(busproto.PresenceTTL + time.Second)
	if _, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", "late"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("stale presence: %v", err)
	}
}

func TestPresenceIgnoresAnotherPersonsSession(t *testing.T) {
	tm := newTeam(t)
	tm.uploaded(tm.alexMac, "claude", "a-up-6666", "/Users/alex/code/api")
	got := tm.present(tm.garyMac, live("g-api-1111", "claude", "/x/api", true), live("a-up-6666", "claude", "/x/api", true))
	if len(got.Ignored) != 1 || got.Ignored[0] != "a-up-6666" {
		t.Fatalf("ignored %v", got.Ignored)
	}
	if _, err := tm.send(tm.garyMac, "a-up-6666", "a-api", "posing"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("posed as another person's session: %v", err)
	}
	// A poll replaces the device's presence: g-web is gone.
	if _, err := tm.send(tm.garyMac, "g-web-2222", "a-api", "gone"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("session dropped from presence still sends: %v", err)
	}
}

func TestRecipientResolution(t *testing.T) {
	tm := newTeam(t)
	sam := tm.user("sam")
	tm.exec(`INSERT INTO users(id,email,name,role,identity_type,created_at) VALUES($1,'sam@other.test','Sam Two','member','human',now())`, uuid.NewString())
	tm.uploaded(tm.device(sam), "claude", "g-api-9999", "/s/api") // shares the g-api prefix
	for _, c := range []struct{ to, code string }{
		{"g-a", busproto.CodeBadRequest},
		{"zzzz", busproto.CodeUnknownRecipient},
		{"g-api", busproto.CodeAmbiguousRecipient},
		{"@nobody", busproto.CodeUnknownRecipient},
		{"@sam", busproto.CodeAmbiguousRecipient},
		{"g-web-2222", busproto.CodeBadRequest}, // itself
	} {
		_, err := tm.send(tm.garyMac, "g-web-2222", c.to, "hi")
		if code(err) != c.code {
			t.Errorf("to %q: %v, want %s", c.to, err, c.code)
		}
		var be *busproto.Error
		if c.code == busproto.CodeAmbiguousRecipient && (!errors.As(err, &be) || len(be.Candidates) != 2) {
			t.Errorf("to %q: candidates %+v", c.to, be)
		}
	}
	if out, err := tm.send(tm.garyMac, "g-web-2222", "g-api-1", "hi"); err != nil || out.To.Session != "g-api-1111" {
		t.Fatalf("longer prefix: %+v %v", out, err)
	}
	if out, err := tm.send(tm.garyMac, "g-web-2222", "@sam@example.test", "hi"); err != nil || out.To.User != "sam@example.test" {
		t.Fatalf("@email: %+v %v", out, err)
	}
	// LIKE wildcards in a prefix match themselves only.
	if _, err := tm.send(tm.garyMac, "g-web-2222", "g-a%", "hi"); code(err) != busproto.CodeUnknownRecipient {
		t.Fatalf("wildcard prefix: %v", err)
	}
}

func TestHoldAcceptRevoke(t *testing.T) {
	tm := newTeam(t)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "a-api", "cross-user")
	if out.State != busproto.StateHeld || out.Sender != busproto.SenderTeammate || !out.To.Live || out.To.Busy {
		t.Fatalf("send %+v", out)
	}
	alex := live("a-api-4444", "claude", "/Users/alex/code/api", false)
	got := tm.present(tm.alexMac, alex)
	if len(got.Messages) != 0 || len(got.Held) != 1 || got.Held[0].User != "gary@example.test" || got.Held[0].Count != 1 {
		t.Fatalf("held poll %+v", got)
	}
	if in, _ := tm.s.Inbox(context.Background(), tm.alexMac, busproto.InboxQuery{Session: "a-api-4444"}); len(in.Messages) != 0 {
		t.Fatalf("held message in recipient inbox %+v", in.Messages)
	}
	alexLogin := busproto.Caller{UserID: tm.alex}
	acc, err := tm.s.Accept(context.Background(), alexLogin, "gary")
	if err != nil || acc.Released != 1 || acc.UserID != tm.gary {
		t.Fatalf("accept %+v %v", acc, err)
	}
	got = tm.present(tm.alexMac, alex)
	if len(got.Messages) != 1 || got.Messages[0].ID != out.ID || got.Messages[0].Sender != "teammate" || len(got.Held) != 0 {
		t.Fatalf("after accept %+v", got)
	}
	if next := tm.mustSend(tm.garyMac, "g-api-1111", "a-api", "second"); next.State != busproto.StateQueued {
		t.Fatalf("accepted sender held: %+v", next)
	}
	list, err := tm.s.Accepts(context.Background(), tm.alex)
	if err != nil || len(list.Accepted) != 1 || list.Accepted[0].User != "gary@example.test" {
		t.Fatalf("accepts %+v %v", list, err)
	}
	rev, err := tm.s.Revoke(context.Background(), alexLogin, "gary@example.test")
	if err != nil || rev.Reheld != 2 {
		t.Fatalf("revoke %+v %v", rev, err)
	}
	if got = tm.present(tm.alexMac, alex); len(got.Messages) != 0 || got.Held[0].Count != 2 {
		t.Fatalf("after revoke %+v", got)
	}
	if third := tm.mustSend(tm.garyMac, "g-api-1111", "a-api", "third"); third.State != busproto.StateHeld {
		t.Fatalf("revoked sender queued: %+v", third)
	}
	if _, err := tm.s.Accept(context.Background(), alexLogin, "alex"); code(err) != busproto.CodeBadRequest {
		t.Fatalf("accept self: %v", err)
	}
	if tm.auditCount("bus.accept") != 1 || tm.auditCount("bus.revoke") != 1 {
		t.Fatal("accept and revoke are audited")
	}
}

func TestUserAddressedRoutingAndClaim(t *testing.T) {
	tm := newTeam(t)
	alexLogin := busproto.Caller{UserID: tm.alex}
	if _, err := tm.s.Accept(context.Background(), alexLogin, "gary"); err != nil {
		t.Fatal(err)
	}
	// Default repo: the sender's (api). alex has a live session on api.
	out := tm.mustSend(tm.garyMac, "g-web-2222", "@alex", "to a person", func(r *busproto.SendRequest) { r.Repo = "api" })
	if out.State != busproto.StateQueued || out.To.Session != "" || out.To.Repo != "api" || !out.To.Live {
		t.Fatalf("send %+v", out)
	}
	// alex starts a second device with a web session: not eligible while an
	// api session is live.
	alexLinux := tm.device(tm.alex)
	web := live("a-web-7777", "codex", "/home/alex/web", true)
	if got := tm.present(alexLinux, web); len(got.Claimable) != 0 {
		t.Fatalf("non-repo session offered %+v", got.Claimable)
	}
	if _, err := tm.s.Claim(context.Background(), alexLinux, busproto.ClaimRequest{MessageID: out.ID, SessionID: "a-web-7777"}); code(err) != busproto.CodeNotEligible {
		t.Fatalf("claim for non-repo session: %v", err)
	}
	api := live("a-api-4444", "claude", "/Users/alex/code/api", false)
	got := tm.present(tm.alexMac, api)
	if len(got.Claimable) != 1 || got.Claimable[0].Message.ID != out.ID || got.Claimable[0].Sessions[0] != "a-api-4444" {
		t.Fatalf("claimable %+v", got)
	}
	claimed, err := tm.s.Claim(context.Background(), tm.alexMac, busproto.ClaimRequest{MessageID: out.ID, SessionID: "a-api-4444"})
	if err != nil || claimed.Message.ToSession != "a-api-4444" || claimed.Message.ToAgent != "claude" {
		t.Fatalf("claim %+v %v", claimed, err)
	}
	if again, err := tm.s.Claim(context.Background(), tm.alexMac, busproto.ClaimRequest{MessageID: out.ID, SessionID: "a-api-4444"}); err != nil || again.Message.ID != out.ID {
		t.Fatalf("idempotent claim %v", err)
	}
	got = tm.present(tm.alexMac, api)
	if len(got.Messages) != 1 || got.Messages[0].ID != out.ID || len(got.Claimable) != 0 {
		t.Fatalf("claimed message not in the claimer's set: %+v", got)
	}
	if ack, err := tm.s.Ack(context.Background(), tm.alexMac, busproto.AckRequest{IDs: []string{out.ID}}); err != nil || len(ack.Acked) != 1 {
		t.Fatalf("ack claimed %+v %v", ack, err)
	}
	// With no api session live, any session may take the next one.
	tm.present(tm.alexMac)
	next := tm.mustSend(tm.garyMac, "g-web-2222", "@alex", "anywhere", func(r *busproto.SendRequest) { r.Repo = "api" })
	if next.To.Live != true || next.To.Busy != true {
		t.Fatalf("send with only a web session live: %+v", next.To)
	}
	if got := tm.present(alexLinux, web); len(got.Claimable) != 1 {
		t.Fatalf("fallback claimable %+v", got)
	}
	// Nobody live: queued, not live.
	tm.present(alexLinux)
	if q := tm.mustSend(tm.garyMac, "g-web-2222", "@alex", "later"); q.To.Live || q.State != busproto.StateQueued {
		t.Fatalf("nobody live %+v", q)
	}
	if tm.auditCount("bus.claim") != 1 {
		t.Fatalf("claims audited %d", tm.auditCount("bus.claim"))
	}
}

// Two devices of one person claim the same @user message at once: exactly
// one wins.
func TestClaimIsAtomic(t *testing.T) {
	tm := newTeam(t)
	var callers []busproto.Caller
	var sessions []string
	for i := range 6 {
		d := tm.device(tm.alex)
		id := fmt.Sprintf("a-dev%d-000", i)
		tm.present(d, live(id, "claude", "/home/alex/api", true))
		callers, sessions = append(callers, d), append(sessions, id)
	}
	tm.present(tm.alexMac, live("a-api-4444", "claude", "/Users/alex/code/api", false))
	for round := range 5 {
		out := tm.mustSend(tm.alexMac, "a-api-4444", "@alex", fmt.Sprintf("race %d", round))
		var wg sync.WaitGroup
		var mu sync.Mutex
		wins, lost := 0, 0
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := tm.s.Claim(context.Background(), callers[i], busproto.ClaimRequest{MessageID: out.ID, SessionID: sessions[i]})
				mu.Lock()
				defer mu.Unlock()
				switch code(err) {
				case "":
					wins++
				case busproto.CodeAlreadyClaimed:
					lost++
				default:
					t.Errorf("claim: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins != 1 || lost != len(callers)-1 {
			t.Fatalf("round %d: %d wins, %d lost", round, wins, lost)
		}
	}
}

func TestBodyCapRedactionAndValidation(t *testing.T) {
	tm := newTeam(t)
	if _, err := tm.send(tm.garyMac, "g-api-1111", "g-lin", strings.Repeat("x", busproto.MaxBodyBytes+1)); code(err) != busproto.CodeBadRequest {
		t.Fatalf("over cap: %v", err)
	}
	tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", strings.Repeat("x", busproto.MaxBodyBytes))
	for _, req := range []busproto.SendRequest{
		{FromSession: "g-api-1111", To: "g-lin", Body: "  "},
		{FromSession: "g-api-1111", To: "", Body: "x"},
		{FromSession: "g-api-1111", To: "g-lin", Body: "x", Intent: "shout"},
		{FromSession: "g-api-1111", To: "g-lin", Body: "x", Refs: make([]string, busproto.MaxRefs+1)},
		{FromSession: "", To: "g-lin", Body: "x"},
	} {
		if _, err := tm.s.Send(context.Background(), tm.garyMac, req); code(err) != busproto.CodeBadRequest {
			t.Errorf("%+v: %v", req, err)
		}
	}
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "use "+tok+" for the push")
	if out.Redactions["github-token"] != 1 {
		t.Fatalf("redactions %v", out.Redactions)
	}
	var body string
	if err := tm.pool.QueryRow(context.Background(), `SELECT body FROM bus_messages WHERE id=$1`, out.ID).Scan(&body); err != nil || strings.Contains(body, tok) || !strings.Contains(body, "[REDACTED:github-token") {
		t.Fatalf("stored body %q %v", body, err)
	}
	var meta string
	if err := tm.pool.QueryRow(context.Background(), `SELECT metadata::text FROM audit_events WHERE action='bus.send' AND target_id=$1`, out.ID).Scan(&meta); err != nil || strings.Contains(meta, "push") || !strings.Contains(meta, "github-token") {
		t.Fatalf("audit metadata %s %v", meta, err)
	}
}

func TestLimits(t *testing.T) {
	t.Run("reply to done", func(t *testing.T) {
		tm := newTeam(t)
		done := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "all done", intent(busproto.IntentDone))
		_, err := tm.send(tm.garyLinux, "g-lin-3333", "g-api", "thanks", replyTo(done.ID))
		var be *busproto.Error
		if code(err) != busproto.CodeReplyToDone || !errors.As(err, &be) || be.MessageID == "" || tm.state(be.MessageID) != "refused" {
			t.Fatalf("reply to done: %v", err)
		}
		in, _ := tm.s.Inbox(context.Background(), tm.garyLinux, busproto.InboxQuery{Session: "g-lin-3333", SentOnly: true})
		if len(in.Messages) != 1 || in.Messages[0].State != busproto.StateRefused || in.Messages[0].RefuseReason != busproto.CodeReplyToDone {
			t.Fatalf("refused send in inbox %+v", in.Messages)
		}
		// A reply names a message its person sent or received.
		if _, err := tm.send(tm.alexMac, "a-api-4444", "g-api", "nosy", replyTo(done.ID)); code(err) != busproto.CodeNotFound {
			t.Fatalf("reply to a stranger's message: %v", err)
		}
	})
	t.Run("thread per hour", func(t *testing.T) {
		tm := newTeam(t)
		last := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "m0")
		for i := 1; i < busproto.ThreadPerHour; i++ {
			from, to, c := "g-lin-3333", "g-api", tm.garyLinux
			if i%2 == 0 {
				from, to, c = "g-api-1111", "g-lin", tm.garyMac
			}
			last = tm.mustSend(c, from, to, fmt.Sprintf("m%d", i), replyTo(last.ID))
		}
		if _, err := tm.send(tm.garyLinux, "g-lin-3333", "g-api", "one more", replyTo(last.ID)); code(err) != busproto.CodeThreadRate {
			t.Fatalf("9th in thread: %v", err)
		}
		// A new thread is not limited by the old one.
		tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "fresh thread")
		tm.advance(time.Hour)
		tm.presence()
		tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "an hour later", replyTo(last.ID))
	})
	t.Run("session per hour", func(t *testing.T) {
		tm := newTeam(t)
		for i := range busproto.SessionPerHour {
			tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", fmt.Sprintf("n%d", i))
		}
		if _, err := tm.send(tm.garyMac, "g-api-1111", "@alex", "31st"); code(err) != busproto.CodeSessionRate {
			t.Fatalf("31st send: %v", err)
		}
		// Refused sends do not count; another session is not limited.
		tm.mustSend(tm.garyMac, "g-web-2222", "g-lin", "other session")
		tm.advance(time.Hour + time.Second)
		tm.presence()
		tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "next hour")
	})
	t.Run("duplicate", func(t *testing.T) {
		tm := newTeam(t)
		tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "same")
		if _, err := tm.send(tm.garyMac, "g-api-1111", "g-lin-3333", "same"); code(err) != busproto.CodeDuplicate {
			t.Fatalf("duplicate: %v", err)
		}
		tm.mustSend(tm.garyMac, "g-api-1111", "@alex", "same") // another recipient
		tm.mustSend(tm.garyMac, "g-web-2222", "g-lin", "same") // another sender
		tm.advance(busproto.DuplicateWindow + time.Second)
		tm.presence()
		tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "same")
	})
	t.Run("undelivered per recipient", func(t *testing.T) {
		tm := newTeam(t)
		var sessions []busproto.PresenceSession
		for i := range 3 {
			sessions = append(sessions, live(fmt.Sprintf("g-s%d-0000", i), "claude", "/x/api", true))
		}
		tm.present(tm.garyMac, sessions...)
		for i := range busproto.MaxUndelivered {
			tm.mustSend(tm.garyMac, sessions[i%3].SessionID, "g-lin", fmt.Sprintf("u%d", i))
			if i%25 == 24 {
				tm.advance(time.Hour + time.Second) // stay under the hourly limit
				tm.present(tm.garyMac, sessions...)
				tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false))
			}
		}
		if _, err := tm.send(tm.garyMac, "g-s0-0000", "g-lin", "51st"); code(err) != busproto.CodeRecipientFull {
			t.Fatalf("51st undelivered: %v", err)
		}
		got := tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false))
		ids := []string{got.Messages[0].ID}
		if _, err := tm.s.Ack(context.Background(), tm.garyLinux, busproto.AckRequest{IDs: ids}); err != nil {
			t.Fatal(err)
		}
		tm.mustSend(tm.garyMac, "g-s0-0000", "g-lin", "room again")
	})
}

func TestExpiry(t *testing.T) {
	tm := newTeam(t)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "soon stale")
	user := tm.mustSend(tm.garyMac, "g-api-1111", "@gary", "for any of my sessions", func(r *busproto.SendRequest) { r.Repo = "*" })
	tm.advance(busproto.DefaultTTL)
	tm.presence()
	if got := tm.present(tm.garyLinux, live("g-lin-3333", "codex", "/home/gary/api", false)); len(got.Messages)+len(got.Claimable) != 0 {
		t.Fatalf("expired delivered: %+v", got)
	}
	if _, err := tm.s.Claim(context.Background(), tm.garyLinux, busproto.ClaimRequest{MessageID: user.ID, SessionID: "g-lin-3333"}); code(err) != busproto.CodeNotEligible {
		t.Fatalf("claim expired: %v", err)
	}
	in, _ := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111"})
	if len(in.Messages) != 2 || in.Messages[0].State != busproto.StateExpired || in.Messages[1].State != busproto.StateExpired {
		t.Fatalf("inbox %+v", in.Messages)
	}
	n, err := tm.s.Sweep(context.Background())
	if err != nil || n != 2 || tm.state(out.ID) != "expired" {
		t.Fatalf("sweep %d %v", n, err)
	}
}

func TestPeers(t *testing.T) {
	tm := newTeam(t)
	tm.uploaded(tm.alexMac, "claude", "a-api-4444", "/Users/alex/code/api")
	got, err := tm.s.Peers(context.Background(), tm.alexMac, busproto.PeersQuery{Session: "a-api-4444"})
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, p := range got.Peers {
		rows = append(rows, fmt.Sprintf("%s %s %s %v %v", p.Session, p.User, p.Agent, p.Busy, p.Own))
	}
	want := []string{"g-api-1111 gary@example.test claude true false", "g-lin-3333 gary@example.test codex false false", "g-web-2222 gary@example.test codex false false"}
	if strings.Join(rows, "\n") != strings.Join(want, "\n") {
		t.Fatalf("peers:\n%s", strings.Join(rows, "\n"))
	}
	got, _ = tm.s.Peers(context.Background(), tm.garyMac, busproto.PeersQuery{Session: "g-api-1111", Repo: "api"})
	if len(got.Peers) != 2 || !got.Peers[0].Own || got.Peers[0].Session != "g-lin-3333" || got.Peers[1].Session != "a-api-4444" || got.Peers[1].Title != "uploaded title" {
		t.Fatalf("own first, repo filter: %+v", got.Peers)
	}
	for q, n := range map[busproto.PeersQuery]int{
		{User: "alex"}: 1, {User: "@gary@example.test"}: 3, {Agent: "codex"}: 2, {Repo: "/Users/gary"}: 2, {Repo: "web"}: 1,
	} {
		if got, _ := tm.s.Peers(context.Background(), tm.garyMac, q); len(got.Peers) != n {
			t.Errorf("%+v: %d peers, want %d", q, len(got.Peers), n)
		}
	}
	tm.advance(busproto.PresenceTTL + time.Second)
	if got, _ := tm.s.Peers(context.Background(), tm.garyMac, busproto.PeersQuery{}); len(got.Peers) != 0 {
		t.Fatalf("stale peers %+v", got.Peers)
	}
}

func TestInboxThreadsAndPaging(t *testing.T) {
	tm := newTeam(t)
	first := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "q1", intent(busproto.IntentRequest))
	tm.advance(time.Second)
	reply := tm.mustSend(tm.garyLinux, "g-lin-3333", "g-api", "a1", replyTo(first.ID))
	tm.advance(time.Second)
	tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "other thread")
	if reply.ThreadID != first.ID {
		t.Fatalf("reply thread %s", reply.ThreadID)
	}
	all, err := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111"})
	if err != nil || len(all.Messages) != 3 || all.Messages[0].Body != "other thread" || all.Messages[1].Direction != "received" || all.Messages[1].ReplyTo != first.ID {
		t.Fatalf("inbox %+v %v", all, err)
	}
	thread, _ := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", Thread: first.ID})
	if len(thread.Messages) != 2 {
		t.Fatalf("thread %+v", thread.Messages)
	}
	page, _ := tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", Limit: 2})
	if len(page.Messages) != 2 || page.Next == "" {
		t.Fatalf("page 1 %+v", page)
	}
	page, _ = tm.s.Inbox(context.Background(), tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", Limit: 2, Before: page.Next})
	if len(page.Messages) != 1 || page.Messages[0].ID != first.ID || page.Next != "" {
		t.Fatalf("page 2 %+v", page)
	}
	if _, err := tm.s.Inbox(context.Background(), tm.alexMac, busproto.InboxQuery{Session: "g-api-1111"}); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("another person's inbox: %v", err)
	}
}

func TestPollWakesOnSendAndTimesOut(t *testing.T) {
	tm := newTeam(t)
	lin := live("g-lin-3333", "codex", "/home/gary/api", false)
	start := time.Now()
	out, err := tm.s.Poll(context.Background(), tm.garyLinux, busproto.PollRequest{Sessions: []busproto.PresenceSession{lin}, WaitSeconds: 1})
	if err != nil || len(out.Messages) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("timeout poll: %+v %v after %s", out, err, time.Since(start))
	}
	type result struct {
		out busproto.PollResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := tm.s.Poll(context.Background(), tm.garyLinux, busproto.PollRequest{Sessions: []busproto.PresenceSession{lin}, Cursor: out.Cursor, WaitSeconds: 20})
		done <- result{out, err}
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("poll returned before a send: %+v", r)
	default:
	}
	sent := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "wake up")
	select {
	case r := <-done:
		if r.err != nil || len(r.out.Messages) != 1 || r.out.Messages[0].ID != sent.ID {
			t.Fatalf("woken poll %+v %v", r.out, r.err)
		}
		// The next poll from that cursor waits again.
		cursor := r.out.Cursor
		again, err := tm.s.Poll(context.Background(), tm.garyLinux, busproto.PollRequest{Sessions: []busproto.PresenceSession{lin}, Cursor: cursor})
		if err != nil || len(again.Messages) != 1 || again.Cursor != cursor {
			t.Fatalf("full set again %+v %v", again, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("poll not woken by send")
	}
	// A canceled poll returns the context's error.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	if _, err := tm.s.Poll(ctx, tm.garyLinux, busproto.PollRequest{Sessions: []busproto.PresenceSession{lin}, Cursor: 1 << 40, WaitSeconds: 20}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled poll: %v", err)
	}
}

// Accepting a sender wakes the recipient's poll with the released message.
func TestAcceptWakesPoll(t *testing.T) {
	tm := newTeam(t)
	held := tm.mustSend(tm.garyMac, "g-api-1111", "a-api", "held")
	alex := live("a-api-4444", "claude", "/Users/alex/code/api", false)
	first := tm.present(tm.alexMac, alex)
	done := make(chan busproto.PollResponse, 1)
	go func() {
		out, _ := tm.s.Poll(context.Background(), tm.alexMac, busproto.PollRequest{Sessions: []busproto.PresenceSession{alex}, Cursor: first.Cursor, WaitSeconds: 20})
		done <- out
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := tm.s.Accept(context.Background(), busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if len(out.Messages) != 1 || out.Messages[0].ID != held.ID {
			t.Fatalf("after accept %+v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("accept did not wake the poll")
	}
}

// A bus write that cannot be audited does not happen: the event commits
// with it.
func TestWritesFailClosedWithoutAudit(t *testing.T) {
	tm := newTeam(t)
	toLin := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "for linux")
	toMe := tm.mustSend(tm.garyMac, "g-api-1111", "@gary", "for any session", func(r *busproto.SendRequest) { r.Repo = "*" })
	tm.exec(`ALTER TABLE audit_events ADD CONSTRAINT no_bus_audit CHECK (action NOT LIKE 'bus.%') NOT VALID`)
	ctx := context.Background()
	if _, err := tm.send(tm.garyMac, "g-api-1111", "a-api", "unaudited"); err == nil || code(err) != "error: "+err.Error() {
		t.Fatalf("send: %v", err)
	}
	if _, err := tm.s.Claim(ctx, tm.garyLinux, busproto.ClaimRequest{MessageID: toMe.ID, SessionID: "g-lin-3333"}); err == nil {
		t.Fatal("claim succeeded")
	}
	if _, err := tm.s.Ack(ctx, tm.garyLinux, busproto.AckRequest{IDs: []string{toLin.ID}}); err == nil {
		t.Fatal("ack succeeded")
	}
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err == nil {
		t.Fatal("accept succeeded")
	}
	if _, err := tm.s.Revoke(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err == nil {
		t.Fatal("revoke succeeded")
	}
	var messages, accepts int
	if err := tm.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM bus_messages),(SELECT count(*) FROM bus_accepts)`).Scan(&messages, &accepts); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || accepts != 0 || tm.state(toMe.ID) != "queued" || tm.state(toLin.ID) != "queued" {
		t.Fatalf("unaudited writes landed: %d messages, %d accepts, %s, %s", messages, accepts, tm.state(toMe.ID), tm.state(toLin.ID))
	}
}

// lockWaiters waits until n backends of the test database wait on a lock.
func (f *fixture) lockWaiters(n int) {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var got int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'`).Scan(&got); err != nil {
			f.t.Fatal(err)
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("%d lock waiters, want %d", got, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A revoke that commits while a send from the revoked sender is in flight
// still holds that message: the send cannot commit it queued after the
// revoke (B7).
func TestRevokeDuringSendHoldsTheMessage(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	alexLogin := busproto.Caller{UserID: tm.alex}
	if _, err := tm.s.Accept(ctx, alexLogin, "gary"); err != nil {
		t.Fatal(err)
	}
	// Stall every audit insert, so the send stops after its acceptance
	// check and before its commit.
	tx, err := tm.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE audit_events IN EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out busproto.SendResponse
		err error
	}
	sent := make(chan result, 1)
	go func() {
		out, err := tm.send(tm.garyMac, "g-api-1111", "a-api", "in flight")
		sent <- result{out, err}
	}()
	tm.lockWaiters(1)
	revoked := make(chan error, 1)
	go func() {
		_, err := tm.s.Revoke(ctx, alexLogin, "gary")
		revoked <- err
	}()
	tm.lockWaiters(2)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-sent
	if err := <-revoked; err != nil || r.err != nil {
		t.Fatalf("send %v, revoke %v", r.err, err)
	}
	if got := tm.state(r.out.ID); got != "held" {
		t.Fatalf("message sent during a revoke is %s after it, want held", got)
	}
}

// Held messages from a sender the recipient has not accepted do not fill
// the recipient: an accepted teammate can still reach it.
func TestHeldMessagesDoNotFillTheRecipient(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	sam := tm.user("sam")
	samMac := tm.device(sam)
	var sessions []busproto.PresenceSession
	for i := range 2 {
		sessions = append(sessions, live(fmt.Sprintf("s-%d-0000", i), "claude", "/s/api", true))
	}
	tm.present(samMac, sessions...)
	for i := range busproto.MaxUndelivered {
		if out := tm.mustSend(samMac, sessions[i%2].SessionID, "a-api", fmt.Sprintf("spam %d", i)); out.State != busproto.StateHeld {
			t.Fatalf("send %d: %+v", i, out)
		}
	}
	if _, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil {
		t.Fatal(err)
	}
	if out, err := tm.send(tm.garyMac, "g-api-1111", "a-api", "from an accepted teammate"); err != nil || out.State != busproto.StateQueued {
		t.Fatalf("accepted sender refused: %+v %v", out, err)
	}
	if out, err := tm.send(tm.garyMac, "g-api-1111", "@alex", "to the person", func(r *busproto.SendRequest) { r.Repo = "*" }); err != nil || out.State != busproto.StateQueued {
		t.Fatalf("accepted sender to @alex refused: %+v %v", out, err)
	}
	// The held sender is still capped.
	if _, err := tm.send(samMac, "s-0-0000", "a-api", "one more"); code(err) != busproto.CodeRecipientFull {
		t.Fatalf("held sender past the cap: %v", err)
	}
}

// A device cannot report another person's live session (not uploaded yet)
// as its own: it would send as that session id, count against its limits,
// and make it ambiguous to address.
func TestPresenceIgnoresAnotherPersonsLiveSession(t *testing.T) {
	tm := newTeam(t)
	got := tm.present(tm.garyMac, live("g-api-1111", "claude", "/x/api", true), live("a-api-4444", "claude", "/x/api", true))
	if len(got.Ignored) != 1 || got.Ignored[0] != "a-api-4444" {
		t.Fatalf("ignored %v", got.Ignored)
	}
	if _, err := tm.send(tm.garyMac, "a-api-4444", "g-lin", "posing"); code(err) != busproto.CodeSessionNotOnDevice {
		t.Fatalf("posed as another person's live session: %v", err)
	}
	if out, err := tm.send(tm.garyLinux, "g-lin-3333", "a-api-4444", "to alex"); err != nil || out.To.UserID != tm.alex {
		t.Fatalf("alex's session by its full id: %+v %v", out, err)
	}
	if got := tm.present(tm.alexMac, live("a-api-4444", "claude", "/Users/alex/code/api", false)); len(got.Ignored) != 0 {
		t.Fatalf("owner's own session ignored: %v", got.Ignored)
	}
}

// A held message that expires stays out of the recipient's inbox: the
// recipient's human never accepted its sender (B7). Its sender sees it
// expired.
func TestExpiredHeldMessageStaysHidden(t *testing.T) {
	tm := newTeam(t)
	ctx := context.Background()
	out := tm.mustSend(tm.garyMac, "g-api-1111", "a-api", "never accepted")
	tm.advance(busproto.DefaultTTL)
	if _, err := tm.s.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	tm.presence()
	in, err := tm.s.Inbox(ctx, tm.alexMac, busproto.InboxQuery{Session: "a-api-4444"})
	if err != nil || len(in.Messages) != 0 {
		t.Fatalf("expired held message in the recipient's inbox: %+v %v", in.Messages, err)
	}
	sent, err := tm.s.Inbox(ctx, tm.garyMac, busproto.InboxQuery{Session: "g-api-1111", SentOnly: true})
	if err != nil || len(sent.Messages) != 1 || sent.Messages[0].ID != out.ID || sent.Messages[0].State != busproto.StateExpired {
		t.Fatalf("sender inbox %+v %v", sent.Messages, err)
	}
	// Accepting later does not release it.
	if acc, err := tm.s.Accept(ctx, busproto.Caller{UserID: tm.alex}, "gary"); err != nil || acc.Released != 0 {
		t.Fatalf("accept after expiry %+v %v", acc, err)
	}
}

// Refs pass the server redactor as the body does.
func TestRefsAreRedacted(t *testing.T) {
	tm := newTeam(t)
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	out := tm.mustSend(tm.garyMac, "g-api-1111", "g-lin", "see ref", func(r *busproto.SendRequest) { r.Refs = []string{"fw://s/1 token=" + tok} })
	var refs []string
	if err := tm.pool.QueryRow(context.Background(), `SELECT refs FROM bus_messages WHERE id=$1`, out.ID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || strings.Contains(refs[0], tok) || out.Redactions["github-token"] != 1 {
		t.Fatalf("refs %q, redactions %v", refs, out.Redactions)
	}
}
