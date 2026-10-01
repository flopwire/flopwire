// Package devicebus is the device agent's side of the message bus
// (notes/message-bus/plan.md §4, item 2 of §7): the local inbox, presence,
// the long poll, claims and receipts, and routing between the device's own
// sessions when no server is configured. The server side is internal/bus;
// the wire types are internal/busproto.
//
// With a server, Run holds one poll at a time (internal/busproto, Delivery).
// Each poll carries the device's whole presence: its live sessions, less
// those the path rules keep off the server. When presence changes, the
// poll is cancelled and sent again. The answer is the device's whole
// deliverable set; the local inbox is reconciled against it. Each @user
// message offered is claimed for one live session. Without a server, a
// send to one of the device's own sessions (or to @ its user) goes
// straight into the local inbox with the same envelope and limits.
//
// Either way a hook asks for its session's messages with Pending, which
// answers from the local inbox only, never the network. Pending marks the
// messages delivered; the receipts go to the server in batches.
package devicebus

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

// Server is the bus routes the device calls; client.Bus implements them.
type Server interface {
	Send(context.Context, busproto.SendRequest) (busproto.SendResponse, error)
	Poll(context.Context, busproto.PollRequest) (busproto.PollResponse, error)
	Claim(context.Context, busproto.ClaimRequest) (busproto.ClaimResponse, error)
	Ack(context.Context, busproto.AckRequest) (busproto.AckResponse, error)
	Peers(context.Context, busproto.PeersQuery) (busproto.PeersResponse, error)
	Inbox(context.Context, busproto.InboxQuery) (busproto.InboxResponse, error)
}

// Session is one of the device's sessions as the agent knows it.
type Session struct {
	busproto.PresenceSession
	// Withheld: the path rules keep the session off the server, so it is
	// not reported there; only routing on the device sees it.
	Withheld bool
	// LastActive is the transcript's last write.
	LastActive time.Time
}

// Config configures a Bus. Zero fields take defaults.
type Config struct {
	// Connect returns the client for the saved server credential and a key
	// that changes when the token or the TLS pin does (a rotation, a
	// re-login, a re-pin). nil: no server is configured, and the device
	// routes between its own sessions.
	Connect func() (Server, string)
	// Presence lists the device's live sessions. Known lists the sessions
	// the device holds (live or not) whose id starts with a prefix, for
	// addressing without a server. The agent sets both (SetSources).
	Presence func(context.Context) ([]Session, error)
	Known    func(ctx context.Context, prefix string) ([]Session, error)

	// User is the device's person without a server: the name @user
	// matches and envelopes carry. Default: the OS account name.
	User string

	Logger *slog.Logger
	Now    func() time.Time

	PresenceEvery time.Duration // how often presence is checked for a change; default 2s
	BackoffMin    time.Duration // first retry after a failure: 1s, as sync
	BackoffMax    time.Duration // retry ceiling: 30s, as sync
	RepinEvery    time.Duration // while stopped, how often the saved credential is re-read: 30s, as sync
	DisabledEvery time.Duration // while the server has no bus, how often to ask again: 10m
	AckDelay      time.Duration // receipts wait this long to batch: 250ms
	PollWait      time.Duration // the poll's hold; default busproto.PollWait
}

func (c *Config) defaults() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.User == "" {
		c.User = localUser()
	}
	if c.PresenceEvery <= 0 {
		c.PresenceEvery = 2 * time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.RepinEvery <= 0 {
		c.RepinEvery = 30 * time.Second
	}
	if c.DisabledEvery <= 0 {
		c.DisabledEvery = 10 * time.Minute
	}
	if c.AckDelay <= 0 {
		c.AckDelay = 250 * time.Millisecond
	}
	if c.PollWait <= 0 {
		c.PollWait = busproto.PollWait
	}
}

func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if s := os.Getenv("USER"); s != "" {
		return s
	}
	return "me"
}

// Bus states, as status shows them.
const (
	StateLocal      = "local"      // no server: routing on the device
	StateConnecting = "connecting" // no poll answered yet
	StateConnected  = "connected"  // the last poll was answered
	StateBackoff    = "backing off"
	StateStopped    = "stopped"  // the credential or the pin was refused; resumes when the saved one changes
	StateDisabled   = "disabled" // the server has no bus, or refuses this credential the bus
)

// Status is the bus as `flopwire agent status` shows it.
type Status struct {
	State     string    `json:"state"`
	LastError string    `json:"last_error,omitempty"`
	RetryAt   time.Time `json:"retry_at,omitzero"`
	LastPoll  time.Time `json:"last_poll,omitzero"` // the last answered poll
	// Sessions is how many live sessions presence reports (to the server,
	// or locally).
	Sessions int `json:"sessions"`
	// Pending counts undelivered messages in the local inbox; Unacked,
	// delivered messages whose receipt the server has not taken yet.
	Pending int `json:"pending"`
	Unacked int `json:"unacked"`
	// Held counts messages from people the user has not accepted (B7).
	Held int `json:"held"`
}

// Bus is the device's message bus. Open it, set its sources, then Run it.
// Pending, Send, Peers and Inbox are safe to call at any time, before or
// during Run.
type Bus struct {
	cfg Config
	st  *store
	log *slog.Logger

	mu       sync.Mutex
	status   Status
	held     []busproto.HeldSender
	presence presenceCache
	recheck  chan struct{} // Recheck: re-read the saved credential now
	ackWake  chan struct{} // a delivery owes a receipt
	localSeq int64
}

// Open opens (creating) the local inbox at path.
func Open(path string, cfg Config) (*Bus, error) {
	cfg.defaults()
	st, err := openStore(path)
	if err != nil {
		return nil, err
	}
	b := &Bus{cfg: cfg, st: st, log: cfg.Logger, recheck: make(chan struct{}, 1), ackWake: make(chan struct{}, 1)}
	b.status.State = StateConnecting
	if cfg.Connect == nil {
		b.status.State = StateLocal
	}
	return b, nil
}

// Close closes the local inbox. Run must have returned.
func (b *Bus) Close() error { return b.st.db.Close() }

// SetSources installs the agent's presence and session lookups.
func (b *Bus) SetSources(presence func(context.Context) ([]Session, error), known func(context.Context, string) ([]Session, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.Presence, b.cfg.Known = presence, known
}

// Local reports whether the bus routes on the device (no server).
func (b *Bus) Local() bool { return b.cfg.Connect == nil }

// Run polls the server (or, without one, routes on the device) until ctx
// ends. It returns nil on shutdown. A failing server never stops it: it
// backs off and retries, and nothing else in the agent waits for it.
func (b *Bus) Run(ctx context.Context) error {
	if b.Local() {
		b.runLocal(ctx)
		return nil
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.runAcks(ctx)
	}()
	b.runServer(ctx)
	wg.Wait()
	return nil
}

// Recheck asks a stopped bus to re-read the saved credential now (after
// `flopwire login` saved a new token or pin).
func (b *Bus) Recheck() {
	select {
	case b.recheck <- struct{}{}:
	default:
	}
}

// Pending returns the session's undelivered messages and marks them
// delivered; receipts for messages from the server are queued and sent in
// a batch. It reads only the local inbox. Of several concurrent callers
// for one session, each message goes to exactly one. agent narrows the
// session when two harnesses share an id ("" matches any).
func (b *Bus) Pending(ctx context.Context, session, agent string) ([]busproto.Envelope, error) {
	return b.Take(ctx, session, agent, Limit{})
}

// Limit bounds what one Take returns. Zero fields mean no bound.
type Limit struct {
	// Count is the most messages taken.
	Count int
	// Bytes bounds the sum of Size over the messages taken, plus Sep
	// between each two. The oldest message is taken even when it alone is
	// larger, so an oversized message never blocks the queue; the caller
	// cuts it.
	Bytes int
	Sep   int
	// Size is a message's cost against Bytes; default: the body and refs'
	// length.
	Size func(busproto.Envelope) int
}

// Take is Pending with a bound: it marks delivered and returns the
// session's oldest undelivered messages that fit in lim. The rest stay
// queued for the next call, in order.
func (b *Bus) Take(ctx context.Context, session, agent string, lim Limit) ([]busproto.Envelope, error) {
	if session == "" {
		return nil, errors.New("pending: a session id is required")
	}
	out, err := b.st.take(ctx, session, agent, b.cfg.Now(), lim)
	if err != nil {
		return nil, err
	}
	if len(out) > 0 && !b.Local() {
		select {
		case b.ackWake <- struct{}{}:
		default:
		}
	}
	return out, nil
}

// Requeue undoes Pending for messages its caller never received (the
// hook left before the answer reached it): they are undelivered again
// and owe no receipt, so the session's next Pending returns them.
func (b *Bus) Requeue(ctx context.Context, ids []string) error {
	return b.st.untake(ctx, ids)
}

// Held returns the senders whose messages wait for the user's acceptance
// (B7), from the last poll: what the hook's user-visible notice needs.
// Without a server nothing is held.
func (b *Bus) Held() []busproto.HeldSender {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]busproto.HeldSender{}, b.held...)
}

// Status reports the bus state and the local inbox counts.
func (b *Bus) Status(ctx context.Context) Status {
	b.mu.Lock()
	st := b.status
	for _, h := range b.held {
		st.Held += h.Count
	}
	b.mu.Unlock()
	if c, err := b.st.counts(ctx, b.cfg.Now()); err == nil {
		st.Pending, st.Unacked = c.pending, c.owed
	}
	return st
}

func (b *Bus) setStatus(fn func(*Status)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(&b.status)
}
