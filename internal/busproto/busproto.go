// Package busproto is the message bus wire protocol between the device
// agent, the CLI and the server (notes/message-bus/plan.md §3, §4). The
// server implements it in internal/bus and serves it in internal/api.
//
// Every route authenticates with a person's enrolled device credential
// ("Authorization: Bearer <token>"), except the accept routes, which need
// that person's login session, and the held list, which shows previews of
// messages the person has not accepted and so must never reach an agent. The server takes the device and user from
// the credential, never from the request.
//
//	POST   /v1/bus/send             SendRequest  -> SendResponse
//	POST   /v1/bus/poll             PollRequest  -> PollResponse (long poll)
//	POST   /v1/bus/claim            ClaimRequest -> ClaimResponse
//	POST   /v1/bus/ack              AckRequest   -> AckResponse
//	GET    /v1/bus/peers            ?session=&repo=&user=&agent= -> PeersResponse
//	GET    /v1/bus/inbox            ?session=&sent=&thread=&limit=&before= -> InboxResponse
//	GET    /v1/bus/accepts          -> AcceptsResponse (login session)
//	POST   /v1/bus/accepts          AcceptRequest -> AcceptResponse (login session)
//	DELETE /v1/bus/accepts/{user}   -> AcceptResponse (login session)
//	GET    /v1/bus/held             -> HeldResponse (login session)
//
// A failure is a problem document (Error) with a stable Code.
//
// # Sessions
//
// A session is (agent, session id), the ids `flopwire sessions` prints. A
// request names the calling session in FromSession or the session query
// parameter; the server accepts it only when the calling device reported
// it in its presence or uploaded its transcript.
//
// # Delivery
//
// The device agent holds one poll open at a time. Each poll carries the
// device's presence (every live session) and replaces what the server held
// for the device; a device whose presence changes (a session starts, ends,
// or turns busy or idle) cancels its poll and starts another. The answer
// holds every message the device should deliver now, not only new ones,
// so the device can reconcile its local inbox: a message missing from it
// was delivered elsewhere, expired, or held again. Cursor tells new from
// old; the poll waits while nothing is newer than the cursor sent.
//
// A message to a session arrives in Messages on the devices that hold that
// session. A message to a vendor cloud session (Claude cloud, Devin cloud)
// arrives in Claimable, marked Cloud, on each of its owner's devices: the
// device that claims it pushes it into the session while the session
// reports a turn running (see Cloud sessions). A message to @user arrives in Claimable on each of that person's
// devices with an eligible live session; the device claims it for one
// session with ClaimRequest (atomic: one claim wins) and then delivers it.
// A hook takes a message on lease and confirms it after printing it; the
// device then acknowledges it with AckRequest, which sets delivered_at. A
// message no hook confirmed after MaxAttempts leases (devicebus) is
// reported in AckRequest.Undelivered and becomes undelivered; so does one
// whose session ended before a hook delivered it (AckRequest.SessionEnded).
// When the device then finds a delivered message's wrapper in the hook
// context its session's transcript recorded, it sends AckRequest.Read, which
// sets read_at.
//
// # Cloud sessions
//
// A vendor cloud session runs on the vendor's machines and is owned by a
// person, not a device. Each of the person's devices that can list them
// (the vendor's CLI login) reports them in PollRequest.Cloud; the server
// keeps one row per (person, agent, session) whichever device reported it
// last, live for PresenceTTL like any session. Peers marks them Cloud.
// Sending to one follows the rules of any session: own, or held until the
// recipient accepts the sender (B7). An @user message never goes to a
// cloud session. The device that claims a cloud message pushes it with the
// vendor's own route (internal/vendorcloud) only while the session runs a
// turn; a push that fails is retried, and after devicebus.MaxAttempts
// failures reported in AckRequest.PushFailed.
package busproto

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	PathSend    = "/v1/bus/send"
	PathPoll    = "/v1/bus/poll"
	PathClaim   = "/v1/bus/claim"
	PathAck     = "/v1/bus/ack"
	PathPeers   = "/v1/bus/peers"
	PathInbox   = "/v1/bus/inbox"
	PathAccepts = "/v1/bus/accepts" // DELETE PathAccepts + "/" + user to revoke
	PathHeld    = "/v1/bus/held"
)

const (
	// MaxBodyBytes bounds a message body. Longer material goes by ref.
	// Measured 2026-10-03 (notes/message-bus/hook-caps-2026-10-03.md):
	// Claude Code and Codex take 10,000 characters or bytes of hook
	// context intact, so a body this size, its usual frame (about 430
	// bytes) and the standing instruction fit one hook call
	// (busrender.HookBytes).
	MaxBodyBytes = 4000
	// MaxRefs and MaxRefBytes bound a message's archive addresses.
	MaxRefs     = 10
	MaxRefBytes = 512
	// DefaultTTL is how long an undelivered message waits (B8).
	DefaultTTL = 24 * time.Hour
	// PollWait is the longest a poll holds; WaitSeconds above it is clamped.
	PollWait = 25 * time.Second
	// PresenceTTL is how long a session stays live after the poll that
	// last reported it.
	PresenceTTL = 75 * time.Second
	// MaxPresence bounds the sessions one poll may report.
	MaxPresence = 200
	// MaxAck bounds the ids and read receipts one ack may carry.
	MaxAck = 100
	// MinPrefix is the shortest session id prefix send accepts.
	MinPrefix = 4
)

// Loop and volume limits (plan §3). A send past one is refused.
const (
	ThreadPerHour  = 8  // messages per thread per hour
	SessionPerHour = 30 // sends per sending session per hour, refused ones included (not refusals by a send ceiling)
	// DevicePerHour and UserPerHour bound sends per device and per person.
	// The session limit is keyed on a session id the device reports
	// itself, so these hold a device that invents session ids. Honest use
	// stays far below them: the device ceiling is four sessions each at
	// its loop limit, the person ceiling two and a half such devices.
	DevicePerHour     = 120
	UserPerHour       = 300
	DuplicateWindow   = 10 * time.Minute
	MaxUndelivered    = 50 // undelivered messages per recipient session (or per person for @user); held ones count only for their sender
	InboxDefaultLimit = 50
	InboxMaxLimit     = 200
)

// Intent says what the sender expects.
type Intent string

const (
	IntentRequest Intent = "request" // expects a reply
	IntentInform  Intent = "inform"  // the default; no reply expected
	IntentDone    Intent = "done"    // closes the thread; a reply is refused
)

// ParseIntent maps "" to IntentInform and refuses unknown values.
func ParseIntent(s string) (Intent, error) {
	switch Intent(strings.ToLower(strings.TrimSpace(s))) {
	case "", IntentInform:
		return IntentInform, nil
	case IntentRequest:
		return IntentRequest, nil
	case IntentDone:
		return IntentDone, nil
	}
	return "", fmt.Errorf("intent must be request, inform or done")
}

// State is a message's delivery state.
type State string

const (
	StateQueued    State = "queued"
	StateHeld      State = "held"
	StateClaimed   State = "claimed"
	StateDelivered State = "delivered"
	// StateRead: delivered, and the message's text has since appeared in
	// the recipient session's transcript as hook context (ReadReceipt).
	// It says the text entered the session's context, not that the model
	// acted on it.
	StateRead    State = "read"
	StateExpired State = "expired"
	StateRefused State = "refused"
	// StateUndelivered: the message will not be delivered; Reason says
	// why (ReasonUnconfirmed, ReasonSessionEnded). The sender can send it
	// again.
	StateUndelivered State = "undelivered"
)

// Reasons of an undelivered message.
const (
	// ReasonUnconfirmed: hooks took the message devicebus.MaxAttempts times
	// and none confirmed printing it (each was killed, timed out, or lost
	// its confirmation).
	ReasonUnconfirmed = "unconfirmed"
	// ReasonSessionEnded: the session the message was for (addressed, or
	// claimed for an @user message) ended before a hook delivered it. It
	// is not given to another session (#67). For a cloud session: its
	// vendor reported it archived or exited when the push was tried.
	ReasonSessionEnded = "session_ended"
	// ReasonPushFailed: the message is for a vendor cloud session, and
	// devicebus.MaxAttempts pushes into it failed (the vendor refused or
	// did not answer).
	ReasonPushFailed = "push_failed"
)

// Sender is own when both sessions belong to one person, else teammate (B4).
const (
	SenderOwn      = "own"
	SenderTeammate = "teammate"
)

// Error codes. Refusals by a limit carry MessageID: the refused message is
// kept, and inbox --sent lists it as refused.
const (
	CodeBadRequest         = "bad_request"
	CodeUnknownRecipient   = "unknown_recipient"   // 404: no session or person matches To
	CodeAmbiguousRecipient = "ambiguous_recipient" // 409: Candidates lists the matches
	CodeSessionNotOnDevice = "session_not_on_device"
	CodeWithheldSession    = "withheld_session" // 403: the recipient or a ref names a session a path rule keeps off the server
	CodeWithheldRepo       = "withheld_repo"    // 403: an @user send's repo or a peers filter names a repo a path rule keeps off the server
	CodeDeviceRequired     = "bus_device_required"
	CodeLoginRequired      = "login_session_required"
	CodePasswordRequired   = "password_required" // 403: accept needs the person's password
	CodeNotFound           = "not_found"
	CodeAlreadyClaimed     = "already_claimed"
	CodeNotEligible        = "not_eligible"
	CodeReplyToDone        = "reply_to_done"
	CodeThreadRate         = "thread_rate"
	CodeSessionRate        = "session_rate"
	CodeDeviceRate         = "device_rate"
	CodeUserRate           = "user_rate"
	CodeDuplicate          = "duplicate"
	CodeRecipientFull      = "recipient_full"
)

// Error is the problem document of a failed bus request.
type Error struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
	// MessageID names the stored refused message (a limit refusal).
	MessageID string `json:"message_id,omitempty"`
	// Candidates are the matches of an ambiguous or too-short recipient.
	Candidates []Candidate `json:"candidates,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("bus: %d %s: %s", e.Status, e.Code, e.Detail) }

// Candidate is one match of an ambiguous recipient: a session or a person.
type Candidate struct {
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`
	User    string `json:"user"`
	Repo    string `json:"repo,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Title   string `json:"title,omitempty"`
	Live    bool   `json:"live,omitempty"`
}

// SendRequest is POST /v1/bus/send.
type SendRequest struct {
	// FromSession is the sending session's id; it must be on the calling
	// device. FromAgent narrows it when two harnesses share an id.
	FromSession string `json:"from_session"`
	FromAgent   string `json:"from_agent,omitempty"`
	// To is a session id prefix (at least MinPrefix characters, unique
	// across the organization) or @user: an email, its local part, or the
	// person's name.
	To     string `json:"to"`
	Body   string `json:"body"`
	Intent string `json:"intent,omitempty"`
	// ReplyTo is a message id the sending session sent or received (or
	// one its person sent or received); the reply joins its thread.
	ReplyTo string   `json:"reply_to,omitempty"`
	Refs    []string `json:"refs,omitempty"`
	// Repo routes an @user message: a normalized remote (host/owner/name,
	// or owner/name), which the device resolves a path or a name it knows
	// to, or else a repo name or path (its last element is used). Empty:
	// the sending session's repo, by its remote when it has one. "*": any
	// repo.
	Repo string `json:"repo,omitempty"`
}

// Recipient describes where a sent message is going.
type Recipient struct {
	// Session is set for a message to a session.
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`
	User    string `json:"user"`
	UserID  string `json:"user_id"`
	// Repo is the session's repo root, or the repo name an @user message
	// is routed by ("" for any).
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Live: the session is live now; for @user, at least one session the
	// message may go to is live. Busy: that session (one of them) is
	// running a turn, so the message arrives at its next tool call.
	Live bool `json:"live"`
	Busy bool `json:"busy"`
	// Cloud: the session is a vendor cloud session. It gets the message
	// pushed while it runs a turn, and it cannot reply.
	Cloud bool `json:"cloud,omitempty"`
}

// SendResponse is the outcome of a send that was not refused.
type SendResponse struct {
	ID        string    `json:"id"`
	ThreadID  string    `json:"thread_id"`
	State     State     `json:"state"` // queued or held
	To        Recipient `json:"to"`
	Sender    string    `json:"sender"`
	Intent    Intent    `json:"intent"`
	Sent      time.Time `json:"sent"`
	ExpiresAt time.Time `json:"expires_at"`
	// Redactions counts secrets the server masked in the body, per rule.
	Redactions map[string]int `json:"redactions,omitempty"`
}

// Envelope is a message as a recipient (or its sender's inbox) sees it.
// Every field but Body, Intent, ReplyTo and Refs is set by the server.
type Envelope struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	ReplyTo  string `json:"reply_to,omitempty"`
	// From is the sending session; User its person's email.
	From      string    `json:"from"`
	FromAgent string    `json:"agent"`
	User      string    `json:"user"`
	UserID    string    `json:"user_id"`
	Repo      string    `json:"repo,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Sender    string    `json:"sender"`
	Intent    Intent    `json:"intent"`
	Body      string    `json:"body"`
	Refs      []string  `json:"refs,omitempty"`
	Sent      time.Time `json:"sent"`
	ExpiresAt time.Time `json:"expires_at"`
	ToSession string    `json:"to_session,omitempty"`
	ToAgent   string    `json:"to_agent,omitempty"`
	ToUser    string    `json:"to_user"`
	ToUserID  string    `json:"to_user_id"`
	ToRepo    string    `json:"to_repo,omitempty"`
	Addressed string    `json:"addressed"` // session or user
	Seq       int64     `json:"seq"`
	// Attempt is set by the device agent only, never by the server: how
	// many hooks have been handed the message, this one included. Above 1
	// it is a redelivery: an earlier hook took it and never confirmed
	// printing it, so the session may have seen it already.
	Attempt int `json:"attempt,omitempty"`
}

// PresenceSession is one live session in a poll's heartbeat.
type PresenceSession struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	// Repo is the repo root as the device placed the session (an absolute
	// path); Branch its current git branch.
	Repo string `json:"repo,omitempty"`
	// Remote is the normalized remote (host/owner/name) of the session's
	// repository, "" for none: a repo filter or @user route matches it on
	// any device and at any path (issue #102).
	Remote string `json:"remote,omitempty"`
	// Main is the main checkout of the session's repository as the device
	// placed it: a repo filter matches it as it matches Repo, without
	// listing every worktree (#102).
	Main   string `json:"main,omitempty"`
	Branch string `json:"branch,omitempty"`
	Title  string `json:"title,omitempty"`
	// Busy: a turn is running.
	Busy bool `json:"busy"`
}

// PollRequest is POST /v1/bus/poll.
type PollRequest struct {
	// Sessions is the device's whole presence; it replaces the last one.
	Sessions []PresenceSession `json:"sessions"`
	// Cloud is the person's vendor cloud sessions the device listed
	// (Busy: a turn is running). They belong to the person, not the
	// device: each is recorded for the person, and one the device leaves
	// out stays live until PresenceTTL after the last report of any of
	// the person's devices. At most MaxPresence.
	Cloud []PresenceSession `json:"cloud,omitempty"`
	// Cursor is the Cursor of the last answer (0 at start). The poll
	// answers at once when it holds a message newer than Cursor.
	Cursor int64 `json:"cursor"`
	// Gen is the Gen of the last answer (0 at start). The poll answers at
	// once when the person's generation differs: their set changed in a
	// way the cursor does not show (an accept, or a revoke that held
	// messages again).
	Gen int64 `json:"gen"`
	// WaitSeconds is how long to hold when nothing is new (0: answer at
	// once), at most PollWait.
	WaitSeconds int `json:"wait_seconds"`
}

// Claimable is an @user message this device may claim, with the device's
// sessions it may go to (busy sessions first); or, Cloud, a message to one
// of the person's cloud sessions (Sessions holds that one), which the
// device claims when it is about to push it.
type Claimable struct {
	Message  Envelope `json:"message"`
	Sessions []string `json:"sessions"`
	Cloud    bool     `json:"cloud,omitempty"`
}

// HeldSender counts held messages from one sender to the device's person;
// the person accepts the sender with the accept route (console or CLI).
type HeldSender struct {
	User   string    `json:"user"`
	UserID string    `json:"user_id"`
	Count  int       `json:"count"`
	Oldest time.Time `json:"oldest"`
}

// PollResponse is the device's whole deliverable set.
type PollResponse struct {
	Cursor int64 `json:"cursor"`
	// Gen is the person's change generation; the next poll sends it back.
	Gen int64 `json:"gen"`
	// Messages are addressed to sessions on this device (or claimed by
	// it) and not yet delivered.
	Messages  []Envelope   `json:"messages"`
	Claimable []Claimable  `json:"claimable"`
	Held      []HeldSender `json:"held"`
	// Ignored lists reported sessions the server did not record: the id
	// belongs to another person's session.
	Ignored []string `json:"ignored,omitempty"`
}

// ClaimRequest is POST /v1/bus/claim: take an @user message for one live
// session on this device, or a message to a live cloud session of the
// device's person (SessionID is that session) for this device to push.
type ClaimRequest struct {
	MessageID string `json:"message_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent,omitempty"`
}

// ClaimResponse is the claimed message, now addressed to the session.
type ClaimResponse struct {
	Message Envelope `json:"message"`
}

// AckRequest is POST /v1/bus/ack: the messages in IDs were delivered (a
// hook confirmed printing them); those in Undelivered will not be (no hook
// confirmed them after devicebus.MaxAttempts leases) and become
// undelivered with ReasonUnconfirmed; those in SessionEnded will not be
// either (their session ended first) and become undelivered with
// ReasonSessionEnded; those in PushFailed (cloud messages whose pushes
// failed devicebus.MaxAttempts times) become undelivered with
// ReasonPushFailed; those in Read were read. Together at most MaxAck
// entries.
type AckRequest struct {
	IDs          []string      `json:"ids"`
	Undelivered  []string      `json:"undelivered,omitempty"`
	SessionEnded []string      `json:"session_ended,omitempty"`
	PushFailed   []string      `json:"push_failed,omitempty"`
	Read         []ReadReceipt `json:"read,omitempty"`
}

// ReadReceipt says that a delivered message's text entered its recipient
// session's context: the session's transcript holds the message's wrapper
// in hook context (transcript.HookContext), first at At. It does not say
// that the model acted on it. The server sets read_at once, on a message
// delivered to Session (and Agent) that the calling device holds; the
// state becomes read.
type ReadReceipt struct {
	ID      string    `json:"id"`
	Session string    `json:"session"`
	Agent   string    `json:"agent"`
	At      time.Time `json:"at"`
}

// AckResponse splits the ids of both lists. Rejected ids are not
// deliverable by this device (unknown, another device's, held again, or
// expired); the device does not send them again. Read lists the read
// receipts taken (read_at set now or before), ReadRejected the others (not
// a message delivered to that session on this device); neither is sent
// again.
type AckResponse struct {
	Acked        []string `json:"acked"`
	Rejected     []string `json:"rejected"`
	Read         []string `json:"read"`
	ReadRejected []string `json:"read_rejected"`
}

// Peer is one live session, the row `flopwire peers` prints.
type Peer struct {
	Session  string `json:"session"`
	Agent    string `json:"agent"`
	User     string `json:"user"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name,omitempty"`
	Device   string `json:"device,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Remote   string `json:"remote,omitempty"`
	Main     string `json:"main,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Title    string `json:"title,omitempty"`
	Busy     bool   `json:"busy"`
	Own      bool   `json:"own"` // the caller's own person
	// Cloud: a vendor cloud session, owned by User and on no device
	// (Device is empty). A message is pushed into it while it is busy;
	// it cannot reply.
	Cloud  bool      `json:"cloud,omitempty"`
	SeenAt time.Time `json:"seen_at"`
}

// PeersQuery is GET /v1/bus/peers: session (the calling session, left
// out), repo (a name or an absolute path), user, agent.
type PeersQuery struct {
	Session, Repo, User, Agent string
	// Roots are the checkout roots of the repository Repo names, as the
	// caller's device expanded it (local.ExpandRepo): a session on any of
	// them, or under one, is on that repository.
	Roots []string
	// Mains are its main checkouts: a session placed in one is on it.
	Mains []string
	// Remotes are its normalized remotes: a session whose remote is one
	// of them is on that repository, on any device (issue #102).
	Remotes []string
}

// PeersResponse lists live sessions, the caller's own person first, the
// calling session left out.
type PeersResponse struct {
	Peers []Peer `json:"peers"`
}

// InboxQuery is GET /v1/bus/inbox: session (required) and agent; sent=1
// lists only sent messages; thread narrows to one thread; limit (default
// InboxDefaultLimit) and before (a Next value) page.
type InboxQuery struct {
	Session, Agent string
	SentOnly       bool
	Thread         string
	Limit          int
	Before         string
}

// InboxItem is one message in a session's inbox.
type InboxItem struct {
	Envelope
	// Direction is received or sent, from the session's side.
	Direction string `json:"direction"`
	State     State  `json:"state"`
	// Reason says why a message is refused (the limit's code) or
	// undelivered (ReasonUnconfirmed).
	Reason      string     `json:"reason,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
	ReadAt      *time.Time `json:"read_at,omitempty"`
	// Attempts, on a refused send, counts the refusals with its reason
	// from the session within the hour after it (the server keeps one row
	// for them); LastAt is the latest. Unset for a single attempt.
	Attempts int        `json:"attempts,omitempty"`
	LastAt   *time.Time `json:"last_at,omitempty"`
}

// InboxResponse is one page, newest first. Next, when set, is the before
// parameter of the next page.
type InboxResponse struct {
	Messages []InboxItem `json:"messages"`
	Next     string      `json:"next,omitempty"`
}

// AcceptRequest is POST /v1/bus/accepts: accept messages from Sender (an
// email, its local part, a name or a user id). Password is the person's
// own, typed by them: a login session alone does not accept, because the
// one `flopwire login` saves can be read by any process of the person's
// OS user, an agent included (CodePasswordRequired).
type AcceptRequest struct {
	Sender   string `json:"sender"`
	Password string `json:"password,omitempty"`
}

// Accepted is one sender the person accepts.
type Accepted struct {
	User       string    `json:"user"`
	UserID     string    `json:"user_id"`
	AcceptedAt time.Time `json:"accepted_at"`
}

// AcceptsResponse is GET /v1/bus/accepts: whom the person accepts, and
// who is waiting.
type AcceptsResponse struct {
	Accepted []Accepted   `json:"accepted"`
	Held     []HeldSender `json:"held"`
}

// AcceptResponse is the outcome of an accept or revoke: Released counts
// held messages now deliverable (accept); Reheld counts queued messages
// held again (revoke).
type AcceptResponse struct {
	User     string `json:"user"`
	UserID   string `json:"user_id"`
	Accepted bool   `json:"accepted"`
	Released int    `json:"released,omitempty"`
	Reheld   int    `json:"reheld,omitempty"`
}

// HeldMessage is one held message as its recipient's human reviews it
// before accepting the sender. It carries a preview, never the body: the
// first line, cut to PreviewRunes, with control and format characters
// removed. The preview is text another person's agent wrote; a client
// shows it as inert text.
type HeldMessage struct {
	ID string `json:"id"`
	// Agent, Repo (the repo name, not its path) and Branch describe the
	// sending session.
	Agent     string    `json:"agent"`
	Session   string    `json:"session"`
	Repo      string    `json:"repo,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Intent    Intent    `json:"intent"`
	Addressed string    `json:"addressed"` // session or user
	Preview   string    `json:"preview"`
	Bytes     int       `json:"bytes"` // the whole body's length
	Refs      int       `json:"refs,omitempty"`
	Sent      time.Time `json:"sent"`
	ExpiresAt time.Time `json:"expires_at"`
}

// HeldGroup is one sender's held messages, newest first, at most
// HeldPerSender of them; More counts the rest.
type HeldGroup struct {
	HeldSender
	UserName string        `json:"user_name,omitempty"`
	Newest   time.Time     `json:"newest"`
	Messages []HeldMessage `json:"messages"`
	More     int           `json:"more,omitempty"`
}

// HeldResponse is GET /v1/bus/held: the person's held messages by sender.
type HeldResponse struct {
	Senders []HeldGroup `json:"senders"`
}

const (
	// PreviewRunes bounds a held message's preview.
	PreviewRunes = 200
	// HeldPerSender bounds the previews listed per sender.
	HeldPerSender = 20
)

// Preview is the first non-blank line of body, without control or format
// characters (which could reorder or hide text), cut to PreviewRunes.
func Preview(body string) string {
	line := ""
	for l := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(line) {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == PreviewRunes {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Caller is the authenticated device (or, for accepts, person) a server
// request acts for. It is not on the wire: the server derives it from the
// credential.
type Caller struct {
	UserID   string
	DeviceID string // "" for a login session
	ClientIP string
}
