package devicebus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/bus"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// server is the real API with Postgres and the bus.
type server struct {
	t     *testing.T
	url   string
	hc    *http.Client
	admin string
}

func newServer(t *testing.T) *server {
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
	s := store.NewPostgres(pool, nil, "")
	reg := prometheus.NewRegistry()
	h := httptest.NewServer(api.New(s, api.Config{Bus: &bus.Store{Pool: pool}, Registry: reg}).Handler(reg))
	t.Cleanup(h.Close)
	// The first administrator, as `flopwire bootstrap` makes one.
	now := time.Now().UTC()
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	plain, tokenHash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	u := domain.User{ID: uuid.NewString(), Email: "admin@example.test", Name: "Admin", Role: domain.RoleAdmin, IdentityType: domain.IdentityHuman, PasswordHash: hash, CreatedAt: now}
	c := domain.Credential{ID: uuid.NewString(), UserID: u.ID, Kind: domain.CredentialSession, TokenHash: tokenHash, ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now, Active: true}
	if err := s.BootstrapIdentity(ctx, u, c, domain.AuditEvent{ID: uuid.NewString(), ActorID: u.ID, Action: "bootstrap", TargetType: "user", TargetID: u.ID, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return &server{t: t, url: h.URL, hc: h.Client(), admin: plain}
}

func (s *server) post(path, token string, body, out any) {
	s.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.url+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := s.hc.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		s.t.Fatalf("POST %s: %d %s", path, res.StatusCode, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			s.t.Fatal(err)
		}
	}
}

// member invites a person; it returns their login session.
func (s *server) member(email string) string {
	var invite struct {
		Code string `json:"code"`
	}
	s.post("/v1/admin/invites", s.admin, map[string]string{"email": email, "role": "member"}, &invite)
	var claimed struct {
		Token string `json:"token"`
	}
	s.post("/v1/invites/claim", "", map[string]string{"code": invite.Code, "name": email, "password": "a member's correct password"}, &claimed)
	return claimed.Token
}

// device enrolls a device for the person and returns its credential.
func (s *server) device(session string) string {
	var enrolled struct {
		Token string `json:"token"`
	}
	s.post("/v1/devices", session, map[string]string{"name": "laptop", "platform": "darwin-arm64"}, &enrolled)
	return enrolled.Token
}

// agentBus is one device agent's bus against the server, with a fixed
// presence.
type agentBus struct {
	*devicebus.Bus
	mu       sync.Mutex
	sessions []devicebus.Session
}

func (s *server) agent(token string, sessions ...devicebus.Session) *agentBus {
	s.t.Helper()
	ab := &agentBus{sessions: sessions}
	b, err := devicebus.Open(filepath.Join(s.t.TempDir(), "bus.db"), devicebus.Config{
		Connect: func() (devicebus.Server, string) {
			return client.Bus{Server: s.url, Token: token, HTTP: s.hc}, "k"
		},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		PresenceEvery: 50 * time.Millisecond, BackoffMin: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond,
		AckDelay: 20 * time.Millisecond, PollWait: 2 * time.Second, Lease: 500 * time.Millisecond,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	b.SetSources(func(context.Context) ([]devicebus.Session, error) {
		ab.mu.Lock()
		defer ab.mu.Unlock()
		return slices.Clone(ab.sessions), nil
	}, nil)
	ab.Bus = b
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	s.t.Cleanup(func() { cancel(); <-done; b.Close() })
	return ab
}

// hook is what a hook does: take the session's messages, print them,
// confirm them.
func (b *agentBus) hook(ctx context.Context, session string) ([]busproto.Envelope, error) {
	got, err := b.Take(ctx, session, "", devicebus.Limit{})
	if err != nil || len(got) == 0 {
		return got, err
	}
	ids := make([]string, len(got))
	for i, e := range got {
		ids[i] = e.ID
	}
	return got, b.Confirm(ctx, session, ids)
}

func live(id, agent, repo string, busy bool) devicebus.Session {
	return devicebus.Session{PresenceSession: busproto.PresenceSession{SessionID: id, Agent: agent, Repo: repo, Branch: "main", Busy: busy}, LastActive: time.Now()}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reported waits until the server lists the session as live.
func reported(t *testing.T, b *agentBus, session string) {
	t.Helper()
	waitFor(t, "presence of "+session, func() bool {
		out, err := b.Peers(context.Background(), busproto.PeersQuery{})
		if err != nil {
			return false
		}
		return slices.ContainsFunc(out.Peers, func(p busproto.Peer) bool { return p.Session == session })
	})
}

func sentState(t *testing.T, b *agentBus, session, id string) busproto.State {
	t.Helper()
	in, err := b.Inbox(context.Background(), busproto.InboxQuery{Session: session, SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == id {
			return m.State
		}
	}
	return ""
}

// Two of one person's devices and another person's device against the
// real server: a message to a session on another device is returned by
// that device's pending and acknowledged; a held message is counted, not
// delivered; an @user message raced by two devices is delivered by exactly
// one.
func TestDevicesOverTheServer(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	gary, alex := s.member("gary@example.test"), s.member("alex@example.test")
	lap := s.agent(s.device(gary), live("g-lap-1111", "claude", "/src/api", true))
	desk := s.agent(s.device(gary), live("g-desk-2222", "codex", "/home/g/api", true))
	ax := s.agent(s.device(alex), live("a-mac-3333", "claude", "/src/api", true))
	reported(t, lap, "g-desk-2222")
	reported(t, desk, "g-lap-1111")
	reported(t, desk, "a-mac-3333")

	// Session to session, one person, two devices.
	out, err := lap.Send(ctx, busproto.SendRequest{FromSession: "g-lap-1111", To: "g-desk", Body: "cursor pagination lands today", Intent: "request"})
	if err != nil || out.State != busproto.StateQueued || out.Sender != busproto.SenderOwn || !out.To.Live || !out.To.Busy {
		t.Fatalf("send: %+v %v", out, err)
	}
	var got []busproto.Envelope
	waitFor(t, "the message on the desktop", func() bool {
		got, err = desk.hook(ctx, "g-desk-2222")
		return err == nil && len(got) > 0
	})
	e := got[0]
	if len(got) != 1 || e.ID != out.ID || e.From != "g-lap-1111" || e.User != "gary@example.test" || e.Sender != busproto.SenderOwn ||
		e.Intent != busproto.IntentRequest || e.Body != "cursor pagination lands today" || e.ToSession != "g-desk-2222" {
		t.Fatalf("delivered: %+v", got)
	}
	waitFor(t, "the receipt", func() bool { return sentState(t, lap, "g-lap-1111", out.ID) == busproto.StateDelivered })
	if again, _ := desk.hook(ctx, "g-desk-2222"); len(again) != 0 {
		t.Fatalf("delivered twice: %+v", again)
	}

	// From another person: held until gary accepts alex, and counted.
	held, err := ax.Send(ctx, busproto.SendRequest{FromSession: "a-mac-3333", To: "g-lap", Body: "can you review my PR?"})
	if err != nil || held.State != busproto.StateHeld {
		t.Fatalf("cross-person send: %+v %v", held, err)
	}
	waitFor(t, "the held count", func() bool {
		h := lap.Held()
		return len(h) == 1 && h[0].User == "alex@example.test" && h[0].Count == 1
	})
	if got, _ := lap.hook(ctx, "g-lap-1111"); len(got) != 0 {
		t.Fatalf("held message delivered: %+v", got)
	}
	s.post(busproto.PathAccepts, gary, busproto.AcceptRequest{Sender: "alex@example.test", Password: "a member's correct password"}, nil)
	waitFor(t, "the released message", func() bool {
		got, _ = lap.hook(ctx, "g-lap-1111")
		return len(got) == 1 && got[0].ID == held.ID && got[0].Sender == busproto.SenderTeammate
	})

	// @user: both of gary's devices have a busy session on api; both are
	// offered the message and race to claim it. Exactly one delivers it.
	for round := range 3 {
		u, err := ax.Send(ctx, busproto.SendRequest{FromSession: "a-mac-3333", To: "@gary", Repo: "api", Body: "who owns the pager? round " + string(rune('0'+round))})
		if err != nil || u.State != busproto.StateQueued || !u.To.Live {
			t.Fatalf("@user send: %+v %v", u, err)
		}
		var mu sync.Mutex
		by := map[string]int{}
		collect := func(b *agentBus, session, name string) {
			got, _ := b.hook(ctx, session)
			mu.Lock()
			defer mu.Unlock()
			for _, e := range got {
				if e.ID == u.ID {
					by[name]++
				}
			}
		}
		waitFor(t, "the @user message", func() bool {
			collect(lap, "g-lap-1111", "lap")
			collect(desk, "g-desk-2222", "desk")
			mu.Lock()
			defer mu.Unlock()
			return by["lap"]+by["desk"] > 0
		})
		waitFor(t, "the @user receipt", func() bool { return sentState(t, ax, "a-mac-3333", u.ID) == busproto.StateDelivered })
		// Both devices have polled since; neither delivers it again.
		time.Sleep(300 * time.Millisecond)
		collect(lap, "g-lap-1111", "lap")
		collect(desk, "g-desk-2222", "desk")
		if by["lap"]+by["desk"] != 1 {
			t.Fatalf("round %d: @user message delivered %v", round, by)
		}
	}
	if st := lap.Status(ctx); st.State != devicebus.StateConnected || st.Sessions != 1 || st.Unacked != 0 {
		t.Fatalf("status: %+v", st)
	}
}

// Through the real server: delivered_at is set only after a hook confirms
// the print, and a message no hook confirmed after MaxAttempts leases
// becomes undelivered (unconfirmed) in the sender's inbox.
func TestServerDeliveryNeedsConfirmation(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	gary := s.member("gary@example.test")
	lap := s.agent(s.device(gary), live("g-lap-1111", "claude", "/src/api", true))
	desk := s.agent(s.device(gary), live("g-desk-2222", "codex", "/home/g/api", true))
	reported(t, lap, "g-desk-2222")
	reported(t, desk, "g-lap-1111")

	out, err := lap.Send(ctx, busproto.SendRequest{FromSession: "g-lap-1111", To: "g-desk", Body: "confirm me"})
	if err != nil {
		t.Fatal(err)
	}
	var got []busproto.Envelope
	waitFor(t, "the message on the desktop", func() bool {
		got, err = desk.Take(ctx, "g-desk-2222", "", devicebus.Limit{})
		return err == nil && len(got) > 0
	})
	time.Sleep(200 * time.Millisecond) // past AckDelay, inside the lease
	if st := sentState(t, lap, "g-lap-1111", out.ID); st != busproto.StateQueued {
		t.Fatalf("taken, not confirmed, and the sender sees %s", st)
	}
	if err := desk.Confirm(ctx, "g-desk-2222", []string{out.ID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the receipt", func() bool { return sentState(t, lap, "g-lap-1111", out.ID) == busproto.StateDelivered })

	// Never confirmed: offered MaxAttempts times, then undelivered.
	lost, err := lap.Send(ctx, busproto.SendRequest{FromSession: "g-lap-1111", To: "g-desk", Body: "no hook confirms me"})
	if err != nil {
		t.Fatal(err)
	}
	var attempts []int
	waitFor(t, "the undelivered state", func() bool {
		if g, _ := desk.Take(ctx, "g-desk-2222", "", devicebus.Limit{}); len(g) == 1 {
			attempts = append(attempts, g[0].Attempt)
		}
		return sentState(t, lap, "g-lap-1111", lost.ID) == busproto.StateUndelivered
	})
	if !slices.Equal(attempts, []int{1, 2, 3}) {
		t.Fatalf("offers: %v", attempts)
	}
	in, err := lap.Inbox(ctx, busproto.InboxQuery{Session: "g-lap-1111", SentOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in.Messages {
		if m.ID == lost.ID && (m.Reason != busproto.ReasonUnconfirmed || m.DeliveredAt != nil) {
			t.Fatalf("undelivered message: reason %q, delivered_at %v", m.Reason, m.DeliveredAt)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if g, _ := desk.Take(ctx, "g-desk-2222", "", devicebus.Limit{}); len(g) != 0 {
		t.Fatalf("an undelivered message was offered again: %+v", g)
	}
	if st := desk.Status(ctx); st.Unacked != 0 || st.Pending != 0 {
		t.Fatalf("desk status: %+v", st)
	}
}
