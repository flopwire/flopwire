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
// Either way a hook asks for its session's messages with Take, which
// answers from the local inbox only, never the network. Delivery has two
// steps, so a hook that dies between taking and printing loses nothing:
//
//	queued ──Take──► leased ──Confirm──► delivered (a receipt to the server)
//	   ▲               │
//	   └─lease ends────┤ fewer than MaxAttempts leases: queued again, and
//	     or Requeue    │ the next hook marks it a redelivery (Attempt > 1)
//	                   └─ MaxAttempts leases: undelivered (reported to the
//	                      server, shown in the sender's inbox)
//
// Receipts and reports go to the server in batches.
//
// A delivered message is read once its wrapper appears in hook context in
// the recipient session's transcript, as the agent indexes it (MarkRead):
// its text entered the session's context. It does not say that the model
// acted on it.
package devicebus

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"slices"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/vendorcloud"
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

// Session is one of the device's sessions as the agent knows it, or one
// of its person's vendor cloud sessions (Cloud).
type Session struct {
	busproto.PresenceSession
	// Withheld: the path rules keep the session off the server, so it is
	// not reported there; only routing on the device sees it.
	Withheld bool
	// LastActive is the transcript's last write.
	LastActive time.Time
	// Cloud: a vendor cloud session (cloud.go); Busy means the vendor
	// reports a turn running.
	Cloud bool
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
	// Withheld names a session of the device that the path rules keep off
	// the server and that ref would tell the server about: an archive
	// address (a send's refs) or a session id prefix (its recipient). ""
	// when there is none. The agent sets it (SetWithheld); nil checks
	// nothing.
	Withheld func(ctx context.Context, ref string) (string, error)
	// RepoWithheld reports whether a repo a request names (an @user
	// send's repo, a peers filter or one of its roots: a path or a name)
	// is one the path rules keep off the server: a path they withhold, or
	// a name every session of the device on it is withheld from. The agent
	// sets it (SetWithheld); nil checks nothing.
	RepoWithheld func(ctx context.Context, repo string) (bool, error)

	// User is the device's person without a server: the name @user
	// matches and envelopes carry. Default: the OS account name.
	User string

	// Cloud reaches the person's vendor cloud sessions (cloud.go); nil:
	// none.
	Cloud []vendorcloud.Adapter
	// CloudEvery is how often cloud sessions are listed; default 20s.
	CloudEvery time.Duration

	Logger *slog.Logger
	Now    func() time.Time

	PresenceEvery time.Duration // how often presence is checked for a change; default 2s
	BackoffMin    time.Duration // first retry after a failure: 1s, as sync
	BackoffMax    time.Duration // retry ceiling: 30s, as sync
	RepinEvery    time.Duration // while stopped, how often the saved credential is re-read: 30s, as sync
	DisabledEvery time.Duration // while the server has no bus, how often to ask again: 10m
	AckDelay      time.Duration // receipts wait this long to batch: 250ms
	PollWait      time.Duration // the poll's hold; default busproto.PollWait
	Lease         time.Duration // how long a hook has to confirm what it took; default LeaseFor
	MaxAttempts   int           // leases before a message is undelivered; default MaxAttempts
}

// LeaseFor is how long a hook has to confirm the messages it took before
// they are offered again: past the hook's own deadline for confirming
// (cmd/flopwire hookLate, 3 s) and the 5 s timeout the Flopwire plugins
// give each hook. A var so tests can shorten it.
var LeaseFor = 10 * time.Second

// MaxAttempts is how many leases a message gets. A message offered again
// may already have been shown (the hook printed it and its confirmation was
// lost), so each redelivery is marked, and after MaxAttempts unconfirmed
// leases the message is undelivered: the sender sees it and can send again.
const MaxAttempts = 3

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
	if c.Lease <= 0 {
		c.Lease = LeaseFor
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = MaxAttempts
	}
	if c.CloudEvery <= 0 {
		c.CloudEvery = 20 * time.Second
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
	// or locally); Cloud, how many vendor cloud sessions the device lists.
	Sessions int `json:"sessions"`
	Cloud    int `json:"cloud,omitempty"`
	// Pending counts undelivered messages in the local inbox (queued, or
	// leased to a hook that has not confirmed them); Unacked, delivery and
	// read receipts and undelivered reports the server has not taken yet.
	Pending int `json:"pending"`
	Unacked int `json:"unacked"`
	// Held counts messages from people the user has not accepted (B7);
	// HeldSenders names them.
	Held        int                   `json:"held"`
	HeldSenders []busproto.HeldSender `json:"held_senders,omitempty"`
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
	pushWake chan struct{} // a cloud message may be due
	cloud    map[string]cloudList
	localSeq int64
}

// Open opens (creating) the local inbox at path.
func Open(path string, cfg Config) (*Bus, error) {
	cfg.defaults()
	st, err := openStore(path)
	if err != nil {
		return nil, err
	}
	b := &Bus{cfg: cfg, st: st, log: cfg.Logger, recheck: make(chan struct{}, 1), ackWake: make(chan struct{}, 1), pushWake: make(chan struct{}, 1),
		cloud: map[string]cloudList{}}
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

// SetWithheld installs the agent's checks of the sessions and repos a
// request names.
func (b *Bus) SetWithheld(sessions func(context.Context, string) (string, error), repos func(context.Context, string) (bool, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg.Withheld, b.cfg.RepoWithheld = sessions, repos
}

// Local reports whether the bus routes on the device (no server).
func (b *Bus) Local() bool { return b.cfg.Connect == nil }

// Run polls the server (or, without one, routes on the device) until ctx
// ends. It returns nil on shutdown. A failing server never stops it: it
// backs off and retries, and nothing else in the agent waits for it.
func (b *Bus) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	if len(b.cfg.Cloud) > 0 {
		wg.Go(func() { b.runCloud(ctx) })
	}
	if b.Local() {
		b.runLocal(ctx)
		wg.Wait()
		return nil
	}
	wg.Go(func() { b.runAcks(ctx) })
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

// Pending is Take without a bound.
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

// Take leases the session's oldest queued messages that fit in lim to the
// caller, a hook, and returns them oldest first, each with its Attempt.
// The caller confirms them (Confirm) once it has printed them; a lease not
// confirmed within Config.Lease ends, and the messages are offered to the
// session's next Take (see the package doc). It reads only the local
// inbox. Of several concurrent callers for one session each message goes
// to exactly one, and while one holds a lease the others get nothing, so
// the session sees its messages in order. agent narrows the session when
// two harnesses share an id ("" matches any).
func (b *Bus) Take(ctx context.Context, session, agent string, lim Limit) ([]busproto.Envelope, error) {
	_, out, err := b.TakeWith(ctx, session, agent, lim, nil)
	return out, err
}

// TakeWith is Take for a hook that prints the standing instruction: with
// ins it also reports whether the caller is to print the instruction
// (leased to it now; ConfirmInstruction once printed), and while another
// hook holds the instruction's lease it returns nothing (see
// instruct.go).
func (b *Bus) TakeWith(ctx context.Context, session, agent string, lim Limit, ins *Instruction) (bool, []busproto.Envelope, error) {
	if session == "" {
		return false, nil, errors.New("pending: a session id is required")
	}
	return b.st.take(ctx, session, agent, b.cfg.Now(), lim, b.cfg.Lease, b.cfg.MaxAttempts, ins)
}

// Confirm records that a hook of the session printed these messages
// (taken by Take): they are delivered, and receipts for messages from the
// server are queued and sent in a batch.
func (b *Bus) Confirm(ctx context.Context, session string, ids []string) error {
	if session == "" {
		return errors.New("confirm: a session id is required")
	}
	n, err := b.st.confirm(ctx, session, ids, b.cfg.Now())
	if err == nil && n > 0 && !b.Local() {
		b.kickAcks()
	}
	return err
}

// Read is one sighting of a message in its recipient session's
// transcript: a row of hook context (transcript.HookContext) that holds the
// message's wrapper, recorded at At.
type Read struct {
	Session string
	Agent   string // the harness, as the transcript's source names it
	ID      string
	At      time.Time
}

// MarkRead records sightings of messages in their recipients' transcripts.
// The first sighting of a message sets read_at (later ones, such as a
// redelivery printed again, change nothing); the message reads as read once
// it is delivered (Confirm), and a message from the server owes the server
// a read receipt, sent in the ack batch after its delivery receipt. A
// sighting by another session or harness than the message's recipient, or
// of a message no hook took, is ignored.
func (b *Bus) MarkRead(ctx context.Context, reads []Read) error {
	if len(reads) == 0 {
		return nil
	}
	reads = slices.Clone(reads)
	slices.SortStableFunc(reads, func(x, y Read) int { return x.At.Compare(y.At) })
	n, err := b.st.markRead(ctx, reads, b.cfg.Now())
	if err == nil && n > 0 && !b.Local() {
		b.kickAcks()
	}
	return err
}

// Requeue undoes Take for messages its caller never received (the hook
// left before the answer reached it): they are queued again, and the
// lease does not count as an attempt, so the session's next Take returns
// them unmarked.
func (b *Bus) Requeue(ctx context.Context, ids []string) error {
	return b.st.untake(ctx, ids)
}

// expireLeases settles the leases that ended (see store.expireLeases);
// the loop runs it on every presence tick, so a message reaches
// undelivered (and its sender learns it) without another hook.
func (b *Bus) expireLeases(ctx context.Context) {
	gone, err := expireLeases(ctx, b.st.db, b.cfg.Now(), b.cfg.Lease, b.cfg.MaxAttempts)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("devicebus: leases", "err", err)
		}
		return
	}
	if gone > 0 {
		b.log.Warn("devicebus: messages undelivered: no hook confirmed printing them", "count", gone, "attempts", b.cfg.MaxAttempts)
		if !b.Local() {
			b.kickAcks()
		}
	}
}

// Held returns the senders whose messages wait for the user's acceptance
// (B7), from the last poll: what the hook's user-visible notice needs.
// Without a server nothing is held.
func (b *Bus) Held() []busproto.HeldSender {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]busproto.HeldSender{}, b.held...)
}

// NoticeEvery is how often the user is told about one sender's held
// messages: once a day, the lifetime of a held message (B8), whichever
// session the notice reaches.
const NoticeEvery = 24 * time.Hour

// HeldNotice returns the senders whose held messages the user should be
// told about now (held, and not noticed in the last NoticeEvery), and
// records them noticed. The caller shows them to the person only, never
// to a model.
func (b *Bus) HeldNotice(ctx context.Context) ([]busproto.HeldSender, error) {
	held := b.Held()
	if len(held) == 0 {
		return nil, nil
	}
	return b.st.notice(ctx, held, b.cfg.Now(), NoticeEvery)
}

// Status reports the bus state and the local inbox counts.
func (b *Bus) Status(ctx context.Context) Status {
	b.mu.Lock()
	st := b.status
	for _, h := range b.held {
		st.Held += h.Count
	}
	st.HeldSenders = append([]busproto.HeldSender(nil), b.held...)
	b.mu.Unlock()
	st.Cloud = len(b.CloudSessions())
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
