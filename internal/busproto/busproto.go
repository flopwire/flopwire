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
// session. A message to @user arrives in Claimable on each of that person's
// devices with an eligible live session; the device claims it for one
// session with ClaimRequest (atomic: one claim wins) and then delivers it.
// After a hook prints a message, the device acknowledges it with
// AckRequest, which sets delivered_at.
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
	// MaxAck bounds the ids one ack may name.
	MaxAck = 100
	// MinPrefix is the shortest session id prefix send accepts.
	MinPrefix = 4
)

// Loop and volume limits (plan §3). A send past one is refused.
const (
	ThreadPerHour  = 8  // messages per thread per hour
	SessionPerHour = 30 // sends per sending session per hour
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
	StateRead      State = "read"
	StateExpired   State = "expired"
	StateRefused   State = "refused"
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
	// Repo routes an @user message: a repo name or path (its last element
	// is used). Empty: the sending session's repo. "*": any repo.
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
}

// PresenceSession is one live session in a poll's heartbeat.
type PresenceSession struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	// Repo is the repo root as the device placed the session (an absolute
	// path); Branch its current git branch.
	Repo   string `json:"repo,omitempty"`
	Branch string `json:"branch,omitempty"`
	Title  string `json:"title,omitempty"`
	// Busy: a turn is running.
	Busy bool `json:"busy"`
}

// PollRequest is POST /v1/bus/poll.
type PollRequest struct {
	// Sessions is the device's whole presence; it replaces the last one.
	Sessions []PresenceSession `json:"sessions"`
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
// sessions it may go to (busy sessions first).
type Claimable struct {
	Message  Envelope `json:"message"`
	Sessions []string `json:"sessions"`
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
// session on this device.
type ClaimRequest struct {
	MessageID string `json:"message_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent,omitempty"`
}

// ClaimResponse is the claimed message, now addressed to the session.
type ClaimResponse struct {
	Message Envelope `json:"message"`
}

// AckRequest is POST /v1/bus/ack: these messages were delivered.
type AckRequest struct {
	IDs []string `json:"ids"`
}

// AckResponse splits the ids. Rejected ids are not deliverable by this
// device (unknown, another device's, held again, or expired); the device
// drops them from its inbox.
type AckResponse struct {
	Acked    []string `json:"acked"`
	Rejected []string `json:"rejected"`
}

// Peer is one live session, the row `flopwire peers` prints.
type Peer struct {
	Session  string    `json:"session"`
	Agent    string    `json:"agent"`
	User     string    `json:"user"`
	UserID   string    `json:"user_id"`
	UserName string    `json:"user_name,omitempty"`
	Device   string    `json:"device,omitempty"`
	Repo     string    `json:"repo,omitempty"`
	Branch   string    `json:"branch,omitempty"`
	Title    string    `json:"title,omitempty"`
	Busy     bool      `json:"busy"`
	Own      bool      `json:"own"` // the caller's own person
	SeenAt   time.Time `json:"seen_at"`
}

// PeersQuery is GET /v1/bus/peers: session (the calling session, left
// out), repo (a name or an absolute path), user, agent.
type PeersQuery struct {
	Session, Repo, User, Agent string
	// Roots are the checkout roots of the repository Repo names, as the
	// caller's device expanded it (local.ExpandRepo): a session on any of
	// them, or under one, is on that repository.
	Roots []string
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
	Direction    string     `json:"direction"`
	State        State      `json:"state"`
	RefuseReason string     `json:"refuse_reason,omitempty"`
	DeliveredAt  *time.Time `json:"delivered_at,omitempty"`
	ReadAt       *time.Time `json:"read_at,omitempty"`
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
