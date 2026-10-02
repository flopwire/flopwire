package devicebus

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// localBus is a bus with no server, a clock the test moves, and three live
// sessions: two on api (one busy), one on web.
type localBus struct {
	*Bus
	p   *presenceSrc
	mu  sync.Mutex
	now time.Time
}

func newLocalBus(t *testing.T) *localBus {
	t.Helper()
	lb := &localBus{p: &presenceSrc{}, now: time.Now().UTC().Truncate(time.Second)}
	cfg := testConfig(nil, nil)
	cfg.Now = func() time.Time {
		lb.mu.Lock()
		defer lb.mu.Unlock()
		return lb.now
	}
	lb.p.set(sess("aaaa1111", "claude", "/src/api", true), sess("aaaa2222", "codex", "/src/api", false), sess("bbbb3333", "claude", "/src/web", false))
	lb.Bus = openBus(t, filepath.Join(t.TempDir(), "bus.db"), cfg, lb.p)
	return lb
}

func (lb *localBus) advance(d time.Duration) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.now = lb.now.Add(d)
	lb.Bus.mu.Lock()
	lb.Bus.presence = presenceCache{} // the cache follows the clock
	lb.Bus.mu.Unlock()
}

func (lb *localBus) send(t *testing.T, from, to, body string, opts ...func(*busproto.SendRequest)) (busproto.SendResponse, error) {
	t.Helper()
	req := busproto.SendRequest{FromSession: from, To: to, Body: body}
	for _, o := range opts {
		o(&req)
	}
	return lb.Send(ctx, req)
}

func code(err error) string {
	var be *busproto.Error
	if errors.As(err, &be) {
		return be.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

// Without a server, a send to a session on the device goes into its inbox
// with the server's envelope, and the outcome reads as the server's.
func TestLocalSendToSession(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "aaaa1111", "bbbb", "Heads-up: pagination is changing.\nDetails follow.", func(r *busproto.SendRequest) {
		r.Intent, r.Refs = "request", []string{"aaaa1111/12"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != busproto.StateQueued || out.Sender != busproto.SenderOwn || out.To.Session != "bbbb3333" || !out.To.Live || out.To.Busy ||
		out.To.Repo != "/src/web" || !strings.HasPrefix(out.ID, "m") || len(out.ID) != 17 || out.ThreadID != out.ID || out.ExpiresAt.Sub(out.Sent) != busproto.DefaultTTL {
		t.Fatalf("outcome: %+v", out)
	}
	got, err := deliver(lb.Bus, "bbbb3333", "", Limit{})
	if err != nil || len(got) != 1 {
		t.Fatalf("pending: %v %v", got, err)
	}
	e := got[0]
	if e.ID != out.ID || e.From != "aaaa1111" || e.FromAgent != "claude" || e.User != "gary" || e.Sender != busproto.SenderOwn ||
		e.Intent != busproto.IntentRequest || e.Repo != "/src/api" || e.Branch != "main" || e.ToSession != "bbbb3333" ||
		e.Addressed != "session" || !slices.Equal(e.Refs, []string{"aaaa1111/12"}) || e.Body != "Heads-up: pagination is changing.\nDetails follow." {
		t.Fatalf("envelope: %+v", e)
	}
	if again, _ := deliver(lb.Bus, "bbbb3333", "", Limit{}); len(again) != 0 {
		t.Fatal("delivered twice")
	}
	// The sender's and the recipient's inboxes.
	in, err := lb.Inbox(ctx, busproto.InboxQuery{Session: "aaaa1111"})
	if err != nil || len(in.Messages) != 1 || in.Messages[0].Direction != "sent" || in.Messages[0].State != busproto.StateDelivered || in.Messages[0].DeliveredAt == nil {
		t.Fatalf("sender inbox: %+v %v", in, err)
	}
	in, _ = lb.Inbox(ctx, busproto.InboxQuery{Session: "bbbb3333"})
	if len(in.Messages) != 1 || in.Messages[0].Direction != "received" {
		t.Fatalf("recipient inbox: %+v", in)
	}
	// Secrets are masked as the server masks them.
	tok := "gh" + "p_" + strings.Repeat("aB3dE5", 6)
	out, err = lb.send(t, "aaaa1111", "bbbb", "use "+tok+" for the push")
	if err != nil || out.Redactions["github-token"] != 1 {
		t.Fatalf("redaction: %+v %v", out, err)
	}
	got, _ = deliver(lb.Bus, "bbbb3333", "", Limit{})
	if len(got) != 1 || strings.Contains(got[0].Body, tok) {
		t.Fatalf("body not redacted: %+v", got)
	}
}

func TestLocalRecipientResolution(t *testing.T) {
	lb := newLocalBus(t)
	cases := []struct {
		from, to, code string
	}{
		{"aaaa1111", "aaaa", busproto.CodeAmbiguousRecipient},
		{"aaaa1111", "zzzz", busproto.CodeUnknownRecipient},
		{"aaaa1111", "aa", busproto.CodeBadRequest},
		{"aaaa1111", "aaaa1111", busproto.CodeBadRequest}, // itself
		{"nope0000", "bbbb", busproto.CodeSessionNotOnDevice},
		{"aaaa1111", "@alex", busproto.CodeUnknownRecipient}, // another person needs a server
	}
	for _, c := range cases {
		if _, err := lb.send(t, c.from, c.to, "x"); code(err) != c.code {
			t.Errorf("%s -> %s: %v, want %s", c.from, c.to, err, c.code)
		}
	}
	_, err := lb.send(t, "aaaa1111", "aaaa", "x")
	var be *busproto.Error
	if !errors.As(err, &be) || len(be.Candidates) != 2 || be.Candidates[0].Session != "aaaa1111" {
		t.Fatalf("candidates: %+v", be)
	}
}

// @user without a server is the device's own person: the message goes to
// a live session on the sender's repo (busy first), else to the first
// eligible session that appears.
func TestLocalUserAddressed(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.send(t, "bbbb3333", "@gary", "for any api session", func(r *busproto.SendRequest) { r.Repo = "api" })
	if err != nil || !out.To.Live || !out.To.Busy || out.To.Repo != "api" || out.To.Session != "" {
		t.Fatalf("send: %+v %v", out, err)
	}
	if got, _ := deliver(lb.Bus, "aaaa2222", "", Limit{}); len(got) != 0 {
		t.Fatal("went to the idle session while a busy one is on the repo")
	}
	got, _ := deliver(lb.Bus, "aaaa1111", "", Limit{})
	if len(got) != 1 || got[0].Addressed != "user" || got[0].ToSession != "aaaa1111" || got[0].ToRepo != "api" {
		t.Fatalf("pending: %+v", got)
	}
	// No session on the repo: it waits, then goes to the first one live.
	out, err = lb.send(t, "aaaa1111", "@gary", "for docs", func(r *busproto.SendRequest) { r.Repo = "docs" })
	if err != nil {
		t.Fatal(err)
	}
	// With none on docs, any session but the sender is eligible: it is
	// taken at once.
	if !out.To.Live {
		t.Fatalf("no eligible session: %+v", out)
	}
	lb.p.set(sess("aaaa1111", "claude", "/src/api", true))
	lb.advance(time.Second)
	out, err = lb.send(t, "aaaa1111", "@gary", "later", func(r *busproto.SendRequest) { r.Repo = "docs" })
	if err != nil || out.To.Live {
		t.Fatalf("only the sender is live: %+v %v", out, err)
	}
	lb.p.set(sess("aaaa1111", "claude", "/src/api", true), sess("cccc4444", "claude", "/src/docs", false))
	lb.advance(time.Second)
	all, _ := lb.sessions(ctx)
	if err := lb.claimLocal(ctx, all); err != nil {
		t.Fatal(err)
	}
	if got, _ := deliver(lb.Bus, "cccc4444", "", Limit{}); len(got) != 1 || got[0].Body != "later" {
		t.Fatalf("waiting message not given to the new session: %+v", got)
	}
}

// The server's loop and volume limits hold on the device.
func TestLocalLimits(t *testing.T) {
	t.Run("duplicate", func(t *testing.T) {
		lb := newLocalBus(t)
		if _, err := lb.send(t, "aaaa1111", "bbbb", "same"); err != nil {
			t.Fatal(err)
		}
		_, err := lb.send(t, "aaaa1111", "bbbb", "same")
		var be *busproto.Error
		if !errors.As(err, &be) || be.Code != busproto.CodeDuplicate || be.MessageID == "" || be.Status != 409 {
			t.Fatalf("duplicate: %v", err)
		}
		in, _ := lb.Inbox(ctx, busproto.InboxQuery{Session: "aaaa1111", SentOnly: true})
		i := slices.IndexFunc(in.Messages, func(m busproto.InboxItem) bool { return m.ID == be.MessageID })
		if len(in.Messages) != 2 || i < 0 || in.Messages[i].State != busproto.StateRefused || in.Messages[i].Reason != busproto.CodeDuplicate {
			t.Fatalf("refused message not listed: %+v", in.Messages)
		}
		if got, _ := deliver(lb.Bus, "bbbb3333", "", Limit{}); len(got) != 1 {
			t.Fatalf("refused message delivered: %d", len(got))
		}
		lb.advance(busproto.DuplicateWindow + time.Second)
		if _, err := lb.send(t, "aaaa1111", "bbbb", "same"); err != nil {
			t.Fatalf("after the window: %v", err)
		}
	})
	t.Run("session_rate", func(t *testing.T) {
		lb := newLocalBus(t)
		for i := range busproto.SessionPerHour {
			if _, err := lb.send(t, "aaaa1111", "bbbb", fmt.Sprint("n", i)); err != nil {
				t.Fatal(i, err)
			}
			deliver(lb.Bus, "bbbb3333", "", Limit{}) // keep the recipient under its cap
		}
		_, err := lb.send(t, "aaaa1111", "bbbb", "one more")
		var be *busproto.Error
		if !errors.As(err, &be) || be.Code != busproto.CodeSessionRate || be.Status != 429 {
			t.Fatalf("rate: %v", err)
		}
		lb.advance(time.Hour + time.Second)
		if _, err := lb.send(t, "aaaa1111", "bbbb", "an hour later"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("thread_rate", func(t *testing.T) {
		lb := newLocalBus(t)
		first, err := lb.send(t, "aaaa1111", "bbbb", "start", func(r *busproto.SendRequest) { r.Intent = "request" })
		if err != nil {
			t.Fatal(err)
		}
		from := []string{"bbbb3333", "aaaa1111"}
		to := []string{"aaaa1111", "bbbb3333"}
		for i := 1; i < busproto.ThreadPerHour; i++ {
			if _, err := lb.send(t, from[i%2], to[i%2], fmt.Sprint("reply ", i), func(r *busproto.SendRequest) { r.ReplyTo = first.ID }); err != nil {
				t.Fatal(i, err)
			}
		}
		_, err = lb.send(t, "aaaa2222", "bbbb", "one more", func(r *busproto.SendRequest) { r.ReplyTo = first.ID })
		if code(err) != busproto.CodeThreadRate {
			t.Fatalf("thread rate: %v", err)
		}
		in, _ := lb.Inbox(ctx, busproto.InboxQuery{Session: "bbbb3333", Thread: first.ID})
		if len(in.Messages) != busproto.ThreadPerHour {
			t.Fatalf("thread listing: %d", len(in.Messages))
		}
	})
	t.Run("reply_to_done", func(t *testing.T) {
		lb := newLocalBus(t)
		done, err := lb.send(t, "aaaa1111", "bbbb", "all done", func(r *busproto.SendRequest) { r.Intent = "done" })
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lb.send(t, "bbbb3333", "aaaa1111", "thanks", func(r *busproto.SendRequest) { r.ReplyTo = done.ID }); code(err) != busproto.CodeReplyToDone {
			t.Fatalf("reply to done: %v", err)
		}
		if _, err := lb.send(t, "bbbb3333", "aaaa1111", "?", func(r *busproto.SendRequest) { r.ReplyTo = "mnosuch" }); code(err) != busproto.CodeNotFound {
			t.Fatalf("reply to unknown: %v", err)
		}
	})
	t.Run("recipient_full", func(t *testing.T) {
		lb := newLocalBus(t)
		senders := []string{"aaaa1111", "aaaa2222"}
		for i := range busproto.MaxUndelivered {
			if _, err := lb.send(t, senders[i%2], "bbbb", fmt.Sprint("m", i)); err != nil {
				t.Fatal(i, err)
			}
			if i%20 == 19 {
				lb.advance(time.Hour + time.Second) // stay under the session rate
			}
		}
		if _, err := lb.send(t, "aaaa1111", "bbbb", "the 51st"); code(err) != busproto.CodeRecipientFull {
			t.Fatalf("recipient full: %v", err)
		}
		deliver(lb.Bus, "bbbb3333", "", Limit{})
		if _, err := lb.send(t, "aaaa1111", "bbbb", "after delivery"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("validation", func(t *testing.T) {
		lb := newLocalBus(t)
		for _, req := range []busproto.SendRequest{
			{FromSession: "aaaa1111", To: "bbbb", Body: strings.Repeat("x", busproto.MaxBodyBytes+1)},
			{FromSession: "aaaa1111", To: "bbbb", Body: "  "},
			{FromSession: "aaaa1111", To: "bbbb", Body: "x", Intent: "shout"},
			{FromSession: "aaaa1111", To: "", Body: "x"},
		} {
			if _, err := lb.Send(ctx, req); code(err) != busproto.CodeBadRequest {
				t.Errorf("%+v: %v", req, err)
			}
		}
	})
}

// Undelivered local messages expire.
func TestLocalExpiry(t *testing.T) {
	lb := newLocalBus(t)
	if _, err := lb.send(t, "aaaa1111", "bbbb", "stale soon"); err != nil {
		t.Fatal(err)
	}
	lb.advance(busproto.DefaultTTL + time.Second)
	if got, _ := deliver(lb.Bus, "bbbb3333", "", Limit{}); len(got) != 0 {
		t.Fatal("expired message delivered")
	}
	in, _ := lb.Inbox(ctx, busproto.InboxQuery{Session: "aaaa1111"})
	if len(in.Messages) != 1 || in.Messages[0].State != busproto.StateExpired {
		t.Fatalf("inbox: %+v", in.Messages)
	}
}

func TestLocalPeers(t *testing.T) {
	lb := newLocalBus(t)
	out, err := lb.Peers(ctx, busproto.PeersQuery{Session: "aaaa1111"})
	if err != nil || len(out.Peers) != 2 {
		t.Fatalf("peers: %+v %v", out, err)
	}
	for _, p := range out.Peers {
		if p.Session == "aaaa1111" || !p.Own || p.User != "gary" {
			t.Fatalf("peer: %+v", p)
		}
	}
	if out, _ := lb.Peers(ctx, busproto.PeersQuery{Repo: "api"}); len(out.Peers) != 2 || !out.Peers[0].Busy {
		t.Fatalf("repo filter, busy first: %+v", out.Peers)
	}
	if out, _ := lb.Peers(ctx, busproto.PeersQuery{Agent: "codex"}); len(out.Peers) != 1 {
		t.Fatalf("agent filter: %+v", out.Peers)
	}
	if out, _ := lb.Peers(ctx, busproto.PeersQuery{User: "alex"}); len(out.Peers) != 0 {
		t.Fatalf("another person: %+v", out.Peers)
	}
}

// Inbox pages newest first with the server's next value.
func TestLocalInboxPaging(t *testing.T) {
	lb := newLocalBus(t)
	for i := range 5 {
		if _, err := lb.send(t, "aaaa1111", "bbbb", fmt.Sprint("page ", i)); err != nil {
			t.Fatal(err)
		}
		lb.advance(time.Second)
	}
	var bodies []string
	before := ""
	for {
		in, err := lb.Inbox(ctx, busproto.InboxQuery{Session: "bbbb3333", Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range in.Messages {
			bodies = append(bodies, m.Body)
		}
		if in.Next == "" {
			break
		}
		before = in.Next
	}
	if want := []string{"page 4", "page 3", "page 2", "page 1", "page 0"}; !slices.Equal(bodies, want) {
		t.Fatalf("pages: %v", bodies)
	}
}

// Take with a bound returns the oldest messages that fit and leaves the
// rest queued, in order, for the next call; a message larger than the
// bound alone is still taken, so it never blocks the queue.
func TestTakeBounded(t *testing.T) {
	lb := newLocalBus(t)
	for i := range 7 {
		if _, err := lb.send(t, fmt.Sprintf("aaaa%d", 1111+1111*(i%2)), "bbbb", fmt.Sprintf("m%d %s", i, strings.Repeat("x", 10*i))); err != nil {
			t.Fatal(err)
		}
		lb.advance(time.Second)
	}
	bodies := func(es []busproto.Envelope) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Body[:2])
		}
		return out
	}
	got, err := deliver(lb.Bus, "bbbb3333", "", Limit{Count: 3})
	if err != nil || !slices.Equal(bodies(got), []string{"m0", "m1", "m2"}) {
		t.Fatalf("count bound: %v %v", bodies(got), err)
	}
	// m3 is 33 bytes, m4 43: with a separator of 2, both need 78.
	got, _ = deliver(lb.Bus, "bbbb3333", "", Limit{Bytes: 77, Sep: 2})
	if !slices.Equal(bodies(got), []string{"m3"}) {
		t.Fatalf("byte bound: %v", bodies(got))
	}
	got, _ = deliver(lb.Bus, "bbbb3333", "", Limit{Bytes: 1, Size: func(e busproto.Envelope) int { return 1000 }})
	if !slices.Equal(bodies(got), []string{"m4"}) {
		t.Fatalf("oversized first message: %v", bodies(got))
	}
	if st := lb.Status(ctx); st.Pending != 2 {
		t.Fatalf("pending after bounded takes: %d", st.Pending)
	}
	got, _ = deliver(lb.Bus, "bbbb3333", "", Limit{})
	if !slices.Equal(bodies(got), []string{"m5", "m6"}) {
		t.Fatalf("the rest: %v", bodies(got))
	}
}

// Concurrent bounded takes for one session hand out each message once.
func TestTakeBoundedConcurrent(t *testing.T) {
	lb := newLocalBus(t)
	for i := range 12 {
		if _, err := lb.send(t, fmt.Sprintf("aaaa%d", 1111+1111*(i%2)), "bbbb", fmt.Sprintf("c%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 4 {
				got, err := deliver(lb.Bus, "bbbb3333", "", Limit{Count: 2})
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				for _, e := range got {
					seen[e.Body]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	// A take while another caller's lease is out gets nothing, so the
	// concurrent takes may leave some queued: one caller takes the rest.
	for range 12 {
		got, err := deliver(lb.Bus, "bbbb3333", "", Limit{Count: 2})
		if err != nil || len(got) == 0 {
			break
		}
		for _, e := range got {
			seen[e.Body]++
		}
	}
	if len(seen) != 12 {
		t.Fatalf("delivered %d of 12: %v", len(seen), seen)
	}
	for b, n := range seen {
		if n != 1 {
			t.Fatalf("%s delivered %d times", b, n)
		}
	}
}
