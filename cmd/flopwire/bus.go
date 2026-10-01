package main

// The message bus verbs (notes/message-bus/plan.md §3): peers, send and
// inbox, the same three in the CLI and over MCP (flopwire_peers,
// flopwire_send, flopwire_inbox). They never call the server themselves:
// the device agent answers them over its control socket, through the team
// server or, without one, between this device's own sessions. Every
// request names the calling session, found by exact evidence only
// (local.Detector, or the MCP request's _meta); a send without one is
// refused rather than sent anonymously.
//
// Output (issue #55): the CLI prints compact JSON by default, in the
// busproto field names, with full session ids, a kind field and the
// fields that say how to get more; --text prints the readable forms of
// plan §3. MCP answers text by default and JSON with format=json, as the
// retrieval tools do. A failure is a busErr: a stable code, the cause, a
// fix and an example; in JSON mode the CLI writes it to stderr as JSON.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
)

// busStyle picks the output form (JSON or text), how hints read (CLI
// flags or MCP arguments) and the text budget.
type busStyle struct {
	JSON   bool
	MCP    bool
	Budget int
	// record, when set, receives the answer's JSON object (MCP's
	// structuredContent), whichever form is written.
	record *any
}

// emit writes the answer: v as compact JSON, or text's output.
func (s busStyle) emit(w io.Writer, v any, text func() error) error {
	if s.record != nil {
		*s.record = v
	}
	if s.JSON {
		return writeOut(w, v)
	}
	return text()
}

// fits reports whether v encodes within the budget (0: no budget).
func (s busStyle) fits(v any) bool {
	if s.Budget <= 0 {
		return true
	}
	b, err := json.Marshal(v)
	return err == nil && len(b) < s.Budget
}

// cmd is the CLI or the MCP form of a hint.
func (s busStyle) cmd(cli, mcp string) string {
	if s.MCP {
		return mcp
	}
	return cli
}

// busClient reaches the device agent's control socket as one session.
type busClient struct {
	socket string
	caller func(context.Context) (local.Caller, bool)
	// retry is the wait before the one retry of a request the agent
	// refused because the session is not in its index or presence yet (a
	// session younger than about two seconds).
	retry time.Duration
}

// busTimeout bounds one request: the agent gives the server 20s.
const busTimeout = 25 * time.Second

// defaultBusRetry is how long a new session takes to reach the agent's
// presence: the index (about 1s) plus one presence check (2s).
const defaultBusRetry = 3 * time.Second

// Result bounds: peers and inbox entries per answer by default and at
// most. The inbox's wire default (50 of up to 4000 bytes) is cut to 20 so
// one answer stays small.
const (
	peersDefaultLimit = 50
	peersMaxLimit     = 500
	inboxDefaultLimit = 20
)

// defaultSocket is the agent's control socket beside the client config.
func defaultSocket() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.sock"), nil
}

// --- errors ---

// Error codes the CLI adds to the server's (busproto.Code*).
const (
	codeAgentNotRunning = "agent_not_running"
	codeMessagingOff    = "messaging_off"
	codeAgentOutdated   = "agent_outdated"
	codeAgentTimeout    = "agent_timeout"
	codeAgentError      = "agent_error"
	codeNoCaller        = "no_caller"
)

// busErr is a failed bus command: a stable code, the cause, what to do,
// and a valid example. Refused marks the server's (or the device's)
// refusal of the request, as opposed to a local or transport failure.
type busErr struct {
	Code       string               `json:"code"`
	Status     int                  `json:"status,omitempty"`
	Detail     string               `json:"detail"`
	Fix        string               `json:"fix,omitempty"`
	Example    string               `json:"example,omitempty"`
	MessageID  string               `json:"message_id,omitempty"`
	Candidates []busproto.Candidate `json:"candidates,omitempty"`
	Refused    bool                 `json:"refused"`
	// text is extra text the readable form prints after the detail (the
	// candidates as rows).
	text string
}

func (e *busErr) Error() string {
	var b strings.Builder
	if e.Refused {
		fmt.Fprintf(&b, "refused (%s): ", e.Code)
	}
	b.WriteString(strings.TrimRight(format.Clean(e.Detail), "\n"))
	b.WriteString(e.text)
	if e.Fix != "" {
		fmt.Fprintf(&b, "\nFix: %s.", e.Fix)
	}
	if e.Example != "" {
		fmt.Fprintf(&b, "\nExample: %s", e.Example)
	}
	return b.String()
}

// errorJSON is how a failure prints in JSON mode.
type errorJSON struct {
	Kind  string  `json:"kind"` // "error"
	Error *busErr `json:"error"`
}

// asBusErr is err as a busErr: a server refusal keeps its code, anything
// else is an agent_error.
func asBusErr(err error) *busErr {
	var e *busErr
	if errors.As(err, &e) {
		return e
	}
	var be *busproto.Error
	if errors.As(err, &be) {
		return &busErr{Code: be.Code, Status: be.Status, Detail: be.Detail, MessageID: be.MessageID, Candidates: be.Candidates, Refused: true}
	}
	return &busErr{Code: codeAgentError, Detail: err.Error()}
}

func badUsage(detail, example string) *busErr {
	return &busErr{Code: busproto.CodeBadRequest, Status: 400, Detail: detail, Example: example}
}

// call sends one bus request and turns transport failures into errors that
// say what to do.
func (c *busClient) call(ctx context.Context, req agent.Request) (agent.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, busTimeout)
	defer cancel()
	resp, err := agent.Call(ctx, c.socket, req)
	if err == nil {
		return resp, nil
	}
	var be *busproto.Error
	if errors.As(err, &be) {
		return resp, be
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "agent not running"):
		return resp, &busErr{Code: codeAgentNotRunning, Detail: fmt.Sprintf("the Flopwire device agent is not running (nothing answers on %s); messages go through it", c.socket),
			Fix: "start it with flopwire agent run, or start the service the installer set up", Example: "flopwire agent status"}
	case msg == "messaging is off in this agent":
		return resp, &busErr{Code: codeMessagingOff, Detail: "the device agent runs with messaging off: its local inbox (bus.db) could not be opened",
			Fix: "read the agent log, then restart the agent", Example: "flopwire agent status"}
	case strings.HasPrefix(msg, "unknown op "):
		return resp, &busErr{Code: codeAgentOutdated, Detail: "the running device agent predates messaging", Fix: "restart it so it runs this version", Example: "flopwire agent run"}
	case errors.Is(err, context.DeadlineExceeded):
		return resp, &busErr{Code: codeAgentTimeout, Detail: fmt.Sprintf("the device agent did not answer within %s; the team server may be slow or unreachable", busTimeout), Fix: "check the agent, then try again", Example: "flopwire agent status"}
	}
	return resp, &busErr{Code: codeAgentError, Detail: "the device agent could not complete the request: " + msg, Fix: "check the agent", Example: "flopwire agent status"}
}

// notSeenYet reports whether the agent refused the session only because it
// has not seen it yet (index lag, or the next presence push); a session a
// path rule keeps off the server is not retried.
func notSeenYet(err error) bool {
	var be *busproto.Error
	return errors.As(err, &be) && be.Code == busproto.CodeSessionNotOnDevice && !strings.Contains(be.Detail, "path rule")
}

// callRetry is call with the one retry of a session the agent has not
// seen yet.
func (c *busClient) callRetry(ctx context.Context, req agent.Request) (agent.Response, error) {
	resp, err := c.call(ctx, req)
	if err == nil || !notSeenYet(err) {
		return resp, err
	}
	select {
	case <-ctx.Done():
		return resp, err
	case <-time.After(c.retry):
	}
	return c.call(ctx, req)
}

// errNoCaller is the refusal of a send (and inbox) that cannot name the
// calling session.
func errNoCaller(verb string, st busStyle) *busErr {
	e := &busErr{Code: codeNoCaller, Detail: verb + ": cannot identify the calling session (no Claude Code session file, no Codex thread id, no Devin session lock, no FLOPWIRE_SESSION_ID), and messages always name the session they come from or belong to",
		Fix:     "run it from inside an agent session, or set FLOPWIRE_SESSION_ID (and FLOPWIRE_AGENT: claude, codex or devin) to your session's id",
		Example: "FLOPWIRE_SESSION_ID=0b7e2c1a-… FLOPWIRE_AGENT=claude flopwire " + verb + " …"}
	if st.MCP {
		e.Fix = "set FLOPWIRE_SESSION_ID and FLOPWIRE_AGENT in this MCP server's environment, or run flopwire " + verb + " from your shell tool instead"
	}
	return e
}

// writeOut writes v as compact JSON, one line.
func writeOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// --- arguments ---

type peersArgs struct {
	Repo, User, Agent, Session string
	Limit                      int
}

type sendArgs struct {
	To, Text, Intent, ReplyTo, Repo string
	Refs                            []string
}

type inboxArgs struct {
	Sent           bool
	Thread, Cursor string
	Limit          int
}

// callerJSON names the calling session in an answer.
type callerJSON struct {
	Session string `json:"session"`
	Agent   string `json:"agent,omitempty"`
}

// --- peers ---

// peersJSON is peers' answer: busproto.PeersResponse's peers, with how
// many matched and whether the limit cut them.
type peersJSON struct {
	Kind  string          `json:"kind"` // "peers"
	Peers []busproto.Peer `json:"peers"`
	// Total is how many live sessions matched; More is true when Limit
	// cut the list, and Hint then says how to narrow it.
	Total int  `json:"total"`
	More  bool `json:"more"`
	Limit int  `json:"limit"`
	// Caller is the calling session, left out of Peers; absent when it
	// could not be identified (it may then be listed).
	Caller *callerJSON `json:"caller,omitempty"`
	Hint   string      `json:"hint,omitempty"`
}

// runPeers lists live sessions. The calling session is left out when it is
// known; without one the list is still useful, and says so.
func runPeers(ctx context.Context, c *busClient, a peersArgs, w io.Writer, st busStyle) error {
	if a.Limit < 0 || a.Limit > peersMaxLimit {
		return badUsage(fmt.Sprintf("limit: 1 to %d (default %d)", peersMaxLimit, peersDefaultLimit), st.cmd("flopwire peers --limit 100", "flopwire_peers limit=100"))
	}
	if a.Limit == 0 {
		a.Limit = peersDefaultLimit
	}
	q := busproto.PeersQuery{Repo: a.Repo, User: strings.TrimPrefix(a.User, "@"), Agent: a.Agent}
	if q.Repo != "" {
		q.Repo = local.ResolveRepo(q.Repo)
	}
	self, known := c.caller(ctx)
	q.Session = self.SessionID
	resp, err := c.call(ctx, agent.Request{Op: "peers", Peers: &q})
	var be *busproto.Error
	if err != nil && known && errors.As(err, &be) && be.Code == busproto.CodeSessionNotOnDevice {
		// The agent has not seen this session yet, or a path rule keeps it
		// off the server: ask without naming it (nothing about it leaves
		// the device) and leave it out here.
		q.Session = ""
		resp, err = c.call(ctx, agent.Request{Op: "peers", Peers: &q})
	}
	if err != nil {
		return asBusErr(err)
	}
	peers := []busproto.Peer{}
	if resp.Peers != nil {
		for _, p := range resp.Peers.Peers {
			if known && p.Session == self.SessionID || a.Session != "" && !strings.HasPrefix(p.Session, a.Session) {
				continue
			}
			p.SeenAt = p.SeenAt.UTC() // every time printed is UTC
			peers = append(peers, p)
		}
	}
	slices.SortStableFunc(peers, func(x, y busproto.Peer) int {
		switch {
		case x.Own == y.Own:
			return 0
		case x.Own:
			return -1
		}
		return 1
	})
	out := peersJSON{Kind: "peers", Peers: peers, Total: len(peers), Limit: a.Limit}
	if len(peers) > a.Limit {
		out.Peers, out.More = peers[:a.Limit], true
		out.Hint = fmt.Sprintf("%d of %d shown; narrow with %s, or raise %s (at most %d)", a.Limit, len(peers), st.cmd("--repo, --user, --agent or --session", "repo, user, agent or session"), st.cmd("--limit", "limit"), peersMaxLimit)
	}
	if known {
		out.Caller = &callerJSON{Session: self.SessionID, Agent: string(self.Agent)}
	}
	// Within the budget, whole rows only; the named fields say so.
	for len(out.Peers) > 1 && !st.fits(out) {
		out.Peers, out.More = out.Peers[:len(out.Peers)*3/4], true
		out.Hint = fmt.Sprintf("%d of %d shown (output budget of %d bytes); narrow with %s", len(out.Peers), out.Total, st.Budget, st.cmd("--repo, --user, --agent or --session", "repo, user, agent or session"))
	}
	return st.emit(w, out, func() error { return writePeers(w, out, a, st) })
}

// shortUser is how a person prints: the local part of their email, which
// @user also accepts.
func shortUser(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

// repoBranch is repo@branch with the repo's last element.
func repoBranch(repo, branch string) string {
	r := filepath.Base(strings.TrimRight(repo, "/"))
	if repo == "" {
		r = "-"
	}
	if branch != "" {
		r += "@" + branch
	}
	return format.Clean(r)
}

// quoted is s on one line, cut to about n bytes, quoted; "" stays "".
func quoted(s string, n int) string {
	s = strings.Join(strings.Fields(format.Clean(s)), " ")
	if s == "" {
		return ""
	}
	return strconv.Quote(format.ClipAround(s, 0, n))
}

// writePeers is peers' --text form: the sessions header shape.
func writePeers(w io.Writer, out peersJSON, a peersArgs, st busStyle) error {
	var b strings.Builder
	if out.Caller == nil {
		fmt.Fprintf(&b, "[your own session could not be identified, so it may be listed]\n")
	}
	ids := make([]string, len(out.Peers))
	for i, p := range out.Peers {
		ids[i] = p.Session
	}
	shown, own := 0, 0
	for _, p := range out.Peers {
		state := "live idle"
		if p.Busy {
			state = "live busy"
		}
		fields := []string{format.ShortPrefix(p.Session, ids), format.Clean(shortUser(p.User)), format.Clean(p.Agent), state, repoBranch(p.Repo, p.Branch)}
		if t := quoted(p.Title, 80); t != "" {
			fields = append(fields, t)
		}
		line := strings.Join(fields, "  ") + "\n"
		if st.Budget > 0 && shown > 0 && b.Len()+len(line) > st.Budget-200 {
			break
		}
		b.WriteString(line)
		shown++
		if p.Own {
			own++
		}
	}
	narrow := st.cmd("--repo, --user, --agent or --session", "repo, user, agent or session")
	switch {
	case out.Total == 0 && a.Session != "":
		fmt.Fprintf(&b, "[no live session starts with %s: it ended or is not running now; a message to it waits until it resumes or expires]\n", format.Clean(a.Session))
	case out.Total == 0 && (a.Repo != "" || a.User != "" || a.Agent != ""):
		fmt.Fprintf(&b, "[no other live session matches; %s lists every live session]\n", st.cmd("flopwire peers --text without filters", "flopwire_peers without arguments"))
	case out.Total == 0:
		fmt.Fprintf(&b, "[no other live sessions; %s queues a message for a person's next session]\n", st.cmd("flopwire send @user -- TEXT", `flopwire_send to="@user"`))
	case shown < len(out.Peers):
		fmt.Fprintf(&b, "[%d of %d live sessions shown; output budget of %d bytes reached; narrow with %s]\n", shown, out.Total, st.Budget, narrow)
	case shown < out.Total:
		fmt.Fprintf(&b, "[%s]\n", out.Hint)
	default:
		fmt.Fprintf(&b, "[%d live %s (%d yours); busy: a message arrives at its next tool call; idle: with its human's next prompt. Address one by its first column, or a person as @user]\n",
			shown, plural(shown, "session", "sessions"), own)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// --- send ---

// Arrival is when a sent message reaches its recipient.
const (
	arriveNextToolCall = "next_tool_call"  // a busy session: at its next tool call
	arriveNextPrompt   = "next_prompt"     // an idle session: with its human's next prompt
	arriveAccepted     = "when_accepted"   // held until the recipient's human accepts the sender
	arriveNextSession  = "next_session"    // @user with no live session: their next session
	arriveIfResumed    = "only_if_resumed" // a session that is not running
)

// sendJSON is send's receipt: busproto.SendResponse (id, thread_id,
// state, to, sender, intent, sent, expires_at, redactions) with who sent
// it, when it arrives, and the --text line. It is a receipt, never a
// reply: a reply arrives later as a received message.
type sendJSON struct {
	Kind string `json:"kind"` // "send_receipt"
	busproto.SendResponse
	From    callerJSON `json:"from"`
	Arrives string     `json:"arrives"`
	Outcome string     `json:"outcome"`
}

// runSend sends one message as the calling session and prints its receipt.
func runSend(ctx context.Context, c *busClient, a sendArgs, w io.Writer, st busStyle) error {
	if strings.TrimSpace(a.To) == "" {
		return sendUsage(st, "a recipient is required")
	}
	if strings.TrimSpace(a.Text) == "" {
		return sendUsage(st, "the message text is empty")
	}
	if n := len(a.Text); n > busproto.MaxBodyBytes {
		e := badUsage(fmt.Sprintf("the message is %d bytes; the cap is %d", n, busproto.MaxBodyBytes),
			st.cmd(`flopwire send 0b7e2c1a --ref 4c19e0d2/28672 -- "The failing test output is at the ref."`, `flopwire_send to="0b7e2c1a" refs=["4c19e0d2/28672"] message="The failing test output is at the ref."`))
		e.Fix = "keep the message to what the recipient must act on, and attach longer material by its archive address with " + st.cmd("--ref ADDRESS", "refs") + " (the recipient reads it with flopwire read)"
		return e
	}
	if _, err := busproto.ParseIntent(a.Intent); err != nil {
		e := badUsage(fmt.Sprintf("intent %q: want request (expects a reply), inform (the default: no reply expected) or done (closes the thread; never answered)", a.Intent),
			st.cmd("flopwire send 0b7e2c1a --intent request -- TEXT", `flopwire_send to="0b7e2c1a" intent="request" message="…"`))
		return e
	}
	self, ok := c.caller(ctx)
	if !ok {
		return errNoCaller("send", st)
	}
	req := busproto.SendRequest{FromSession: self.SessionID, FromAgent: string(self.Agent), To: strings.TrimSpace(a.To), Body: a.Text,
		Intent: a.Intent, ReplyTo: strings.TrimSpace(a.ReplyTo), Refs: a.Refs, Repo: a.Repo}
	if req.Repo != "" && req.Repo != "*" {
		req.Repo = local.ResolveRepo(req.Repo)
	}
	resp, err := c.callRetry(ctx, agent.Request{Op: "send", Send: &req})
	if err != nil {
		var be *busproto.Error
		if errors.As(err, &be) {
			return refusal(be, req, st)
		}
		return asBusErr(err)
	}
	if resp.Sent == nil {
		return &busErr{Code: codeAgentError, Detail: "the device agent answered the send without an outcome", Fix: "check the agent", Example: "flopwire agent status"}
	}
	resp.Sent.Sent, resp.Sent.ExpiresAt = resp.Sent.Sent.UTC(), resp.Sent.ExpiresAt.UTC()
	r := sendJSON{Kind: "send_receipt", SendResponse: *resp.Sent, From: callerJSON{Session: self.SessionID, Agent: string(self.Agent)}, Arrives: arrival(*resp.Sent), Outcome: sendOutcome(*resp.Sent)}
	return st.emit(w, r, func() error {
		_, err := io.WriteString(w, r.Outcome+"\n")
		return err
	})
}

func sendUsage(st busStyle, why string) *busErr {
	if st.MCP {
		return badUsage(why+"; flopwire_send takes to (a session id prefix from flopwire_peers, or @user) and message", `flopwire_send to="0b7e2c1a" message="Heads-up: the list endpoint now returns a cursor."`)
	}
	e := badUsage(why, `flopwire send 0b7e2c1a -- "Heads-up: the list endpoint now returns a cursor."`)
	e.Fix = "Usage: flopwire send <to> [--intent request|inform|done] [--reply-to ID] [--ref ADDRESS]... [--repo R] -- <text | ->"
	return e
}

// arrival says when the message arrives (the arrive* values).
func arrival(r busproto.SendResponse) string {
	switch {
	case r.State == busproto.StateHeld:
		return arriveAccepted
	case r.To.Live && r.To.Busy:
		return arriveNextToolCall
	case r.To.Live:
		return arriveNextPrompt
	case r.To.Session != "":
		return arriveIfResumed
	}
	return arriveNextSession
}

// expiry is a time as the outcome line prints it: UTC to the minute.
func expiry(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }

// sendOutcome is the one line a send prints with --text (plan §3), so the
// sender never polls: where the message went and when it arrives.
func sendOutcome(r busproto.SendResponse) string {
	to := r.To
	var line string
	switch {
	case r.State == busproto.StateHeld:
		who := "@" + shortUser(to.User)
		if to.Session != "" {
			who = fmt.Sprintf("%s (%s %s %s)", format.ShortPrefix(to.Session, nil), shortUser(to.User), to.Agent, repoBranch(to.Repo, to.Branch))
		}
		line = fmt.Sprintf("held %s for %s: %s has not accepted messages from you; expires %s", r.ID, who, shortUser(to.User), expiry(r.ExpiresAt))
	case to.Session != "":
		head := fmt.Sprintf("sent %s to %s (%s %s %s)", r.ID, format.ShortPrefix(to.Session, nil), shortUser(to.User), to.Agent, repoBranch(to.Repo, to.Branch))
		switch {
		case to.Live && to.Busy:
			line = head + ": busy, arrives at its next tool call"
		case to.Live:
			line = head + ": idle, arrives with its human's next prompt"
		default:
			line = head + ": not running, arrives only if it resumes; expires " + expiry(r.ExpiresAt)
		}
	default:
		on := ""
		if to.Repo != "" {
			on = " on " + filepath.Base(to.Repo)
		}
		who := "@" + shortUser(to.User)
		switch {
		case to.Live && to.Busy:
			line = fmt.Sprintf("sent %s to %s: a busy session%s takes it, arrives at its next tool call", r.ID, who, on)
		case to.Live:
			line = fmt.Sprintf("sent %s to %s: an idle session%s takes it, arrives with its human's next prompt", r.ID, who, on)
		default:
			line = fmt.Sprintf("queued %s for %s: no live session%s; expires %s", r.ID, who, on, expiry(r.ExpiresAt))
		}
	}
	if n, rules := redactionCount(r.Redactions); n > 0 {
		line += fmt.Sprintf("; %d %s masked before it left this device (%s)", n, plural(n, "secret", "secrets"), rules)
	}
	return format.Clean(line)
}

func redactionCount(m map[string]int) (int, string) {
	n := 0
	var rules []string
	for rule, k := range m {
		n += k
		rules = append(rules, rule)
	}
	slices.Sort(rules)
	return n, strings.Join(rules, ", ")
}

// refusal turns a refused send into its code, cause, fix and a valid
// example.
func refusal(be *busproto.Error, req busproto.SendRequest, st busStyle) *busErr {
	e := asBusErr(be)
	sentList := st.cmd("flopwire inbox --sent", "flopwire_inbox sent=true")
	switch be.Code {
	case busproto.CodeUnknownRecipient:
		e.Fix = "find the session from history (" + st.cmd("flopwire sessions --repo R --branch B --json", "flopwire_sessions repo=R branch=B") + "), check it is live with " + st.cmd("flopwire peers --session ID", "flopwire_peers session=ID") + ", or address a person as @user (their email or its local part)"
		e.Example = st.cmd(`flopwire send @alex -- "TEXT"`, `flopwire_send to="@alex" message="…"`)
	case busproto.CodeAmbiguousRecipient:
		var lines []string
		var ids []string
		for _, cnd := range be.Candidates {
			ids = append(ids, cnd.Session)
		}
		first := ""
		for _, cnd := range be.Candidates {
			if cnd.Session == "" {
				lines = append(lines, "  @"+format.Clean(cnd.User))
				if first == "" {
					first = "@" + cnd.User
				}
				continue
			}
			p := format.ShortPrefix(cnd.Session, ids)
			if first == "" {
				first = p
			}
			state := "ended"
			if cnd.Live {
				state = "live"
			}
			fields := []string{p, format.Clean(shortUser(cnd.User)), format.Clean(cnd.Agent), state, repoBranch(cnd.Repo, cnd.Branch)}
			if t := quoted(cnd.Title, 60); t != "" {
				fields = append(fields, t)
			}
			lines = append(lines, "  "+strings.Join(fields, "  "))
		}
		if len(lines) > 0 {
			e.text = "; candidates:\n" + strings.Join(lines, "\n")
		}
		e.Fix = "use one of the candidates (the full session id in candidates is always unique)"
		e.Example = st.cmd(fmt.Sprintf(`flopwire send %s -- "TEXT"`, format.Clean(first)), fmt.Sprintf(`flopwire_send to=%q message="…"`, format.Clean(first)))
	case busproto.CodeReplyToDone:
		e.Fix = "do not answer it: done closes a thread. If there is new work, start a new thread without reply_to"
		e.Example = st.cmd(fmt.Sprintf(`flopwire send %s -- "TEXT"`, req.To), fmt.Sprintf(`flopwire_send to=%q message="…"`, req.To))
	case busproto.CodeThreadRate:
		e.Fix = "stop this exchange for now: the thread is looping. Settle what is left with your human, or wait an hour"
		e.Example = st.cmd("flopwire inbox --thread THREAD", `flopwire_inbox thread="THREAD"`) + " re-reads the thread"
	case busproto.CodeSessionRate, busproto.CodeDeviceRate, busproto.CodeUserRate:
		e.Fix = "send less: put what you have to say into one message, and wait before sending again"
		e.Example = sentList + " shows what is still undelivered"
	case busproto.CodeDuplicate:
		e.Fix = "do not resend: the first copy is on its way"
		e.Example = sentList + " shows its state"
	case busproto.CodeRecipientFull:
		e.Fix = "wait until the recipient reads what it has; do not resend"
		e.Example = sentList + " shows your undelivered messages"
	case busproto.CodeNotFound:
		e.Fix = "reply_to takes a message id this session sent or received"
		e.Example = st.cmd("flopwire inbox", "flopwire_inbox") + " lists them"
	case busproto.CodeSessionNotOnDevice:
		if strings.Contains(be.Detail, "path rule") {
			e.Fix = "messaging is not available from this session: its transcripts stay on this device, so nothing about it may reach the team server"
			e.Example = "ask your human to send it, or send from a session in another repo"
		} else {
			e.Fix = "the device agent has not seen this session yet; try again in a few seconds"
			e.Example = "flopwire agent status shows how many live sessions the agent reports"
		}
	default:
		u := sendUsage(st, "")
		e.Fix, e.Example = "check the arguments", u.Example
	}
	if be.MessageID != "" {
		e.text += fmt.Sprintf("\nThe refused message %s is listed in %s.", be.MessageID, sentList)
	}
	return e
}

// --- inbox ---

// inboxEntry is one message: busproto.InboxItem (the envelope, direction
// sent or received, state, refuse_reason, delivered_at, read_at) and
// whether it replies to an earlier message. A sent message's state is its
// delivery only: an answer is a separate received entry whose reply_to
// names it.
type inboxEntry struct {
	busproto.InboxItem
	IsReply bool `json:"is_reply"`
}

// inboxJSON is inbox's answer: one page, newest first. More is true when
// another page follows; Next is then the cursor that reads it.
type inboxJSON struct {
	Kind     string       `json:"kind"` // "inbox"
	Session  string       `json:"session"`
	Messages []inboxEntry `json:"messages"`
	More     bool         `json:"more"`
	Next     string       `json:"next,omitempty"`
	Hint     string       `json:"hint,omitempty"`
	// budgetCut: the output budget, not the page size, ended the page.
	budgetCut bool
}

// runInbox lists the calling session's messages, newest first.
func runInbox(ctx context.Context, c *busClient, a inboxArgs, w io.Writer, st busStyle) error {
	if a.Limit < 0 || a.Limit > busproto.InboxMaxLimit {
		return badUsage(fmt.Sprintf("limit: 1 to %d (default %d)", busproto.InboxMaxLimit, inboxDefaultLimit), st.cmd("flopwire inbox --limit 50", "flopwire_inbox limit=50"))
	}
	self, ok := c.caller(ctx)
	if !ok {
		return errNoCaller("inbox", st)
	}
	if a.Limit == 0 {
		a.Limit = inboxDefaultLimit
	}
	q := busproto.InboxQuery{Session: self.SessionID, Agent: string(self.Agent), SentOnly: a.Sent, Thread: strings.TrimSpace(a.Thread), Limit: a.Limit, Before: a.Cursor}
	resp, err := c.callRetry(ctx, agent.Request{Op: "inbox", Inbox: &q})
	if err != nil {
		e := asBusErr(err)
		if e.Code == busproto.CodeBadRequest {
			e.Fix, e.Example = "a cursor comes from an earlier page's next", st.cmd("flopwire inbox", "flopwire_inbox")
		}
		return e
	}
	out := inboxJSON{Kind: "inbox", Session: self.SessionID, Messages: []inboxEntry{}}
	if resp.Inbox != nil {
		for _, m := range resp.Inbox.Messages {
			m.Sent, m.ExpiresAt = m.Sent.UTC(), m.ExpiresAt.UTC()
			for _, t := range []**time.Time{&m.DeliveredAt, &m.ReadAt} {
				if *t != nil {
					u := (*t).UTC()
					*t = &u
				}
			}
			out.Messages = append(out.Messages, inboxEntry{InboxItem: m, IsReply: m.ReplyTo != ""})
		}
		out.Next = resp.Inbox.Next
	}
	// Within the budget, whole messages only: the cursor then starts
	// after the last one shown.
	for len(out.Messages) > 1 && !st.fits(out) {
		out.Messages = out.Messages[:len(out.Messages)*3/4]
		last := out.Messages[len(out.Messages)-1]
		out.Next, out.budgetCut = last.Sent.UTC().Format(time.RFC3339Nano)+"|"+last.ID, true
	}
	out.More = out.Next != ""
	if out.More {
		out.Hint = "pass next as " + st.cmd("--cursor", "cursor") + " for the next page"
		if out.budgetCut {
			out.Hint = fmt.Sprintf("output budget of %d bytes reached; %s", st.Budget, out.Hint)
		}
	}
	return st.emit(w, out, func() error { return writeInbox(w, out, a, st) })
}

// writeInbox is inbox's --text form: one entry per message, a header line,
// then the body indented. A list shows each body's first line; a thread
// (--thread) shows whole bodies and refs. Bodies are written by other
// agents and people: indented, they cannot pass for a header or the
// footer.
func writeInbox(w io.Writer, in inboxJSON, a inboxArgs, st busStyle) error {
	var b strings.Builder
	full := a.Thread != ""
	shown := 0
	threadHint := false
	for _, m := range in.Messages {
		var e strings.Builder
		state := string(m.State)
		if m.RefuseReason != "" {
			state += " (" + m.RefuseReason + ")"
		}
		var peer string
		if m.Direction == "received" {
			peer = fmt.Sprintf("from %s (%s %s %s)", format.ShortPrefix(m.From, nil), shortUser(m.User), m.FromAgent, repoBranch(m.Repo, m.Branch))
		} else if m.Addressed == "user" {
			peer = "to @" + shortUser(m.ToUser)
			if m.ToSession != "" {
				peer += " → " + format.ShortPrefix(m.ToSession, nil)
			}
		} else {
			peer = fmt.Sprintf("to %s (%s %s)", format.ShortPrefix(m.ToSession, nil), shortUser(m.ToUser), m.ToAgent)
		}
		fields := []string{m.ID, m.Direction, m.Sent.UTC().Format("2006-01-02 15:04Z"), format.Clean(peer), string(m.Intent), state}
		if m.Sender == busproto.SenderTeammate && m.Direction == "received" {
			fields = append(fields, "from a teammate")
		}
		if m.ThreadID != "" && m.ThreadID != m.ID {
			fields = append(fields, "thread "+m.ThreadID)
		}
		if m.ReplyTo != "" && m.ReplyTo != m.ThreadID {
			fields = append(fields, "re "+m.ReplyTo)
		}
		e.WriteString(strings.Join(fields, "  ") + "\n")
		body := format.Clean(strings.TrimRight(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n"))
		lines := strings.Split(body, "\n")
		if full {
			for _, l := range lines {
				e.WriteString("    " + l + "\n")
			}
			for _, r := range m.Refs {
				e.WriteString("    ref: " + format.Clean(r) + "\n")
			}
		} else {
			first := format.ClipAround(lines[0], 0, 160)
			more := ""
			if n := len(lines) - 1; n > 0 {
				more = fmt.Sprintf("  (+%d %s)", n, plural(n, "line", "lines"))
			}
			if len(m.Refs) > 0 {
				more += fmt.Sprintf("  (%d %s)", len(m.Refs), plural(len(m.Refs), "ref", "refs"))
			}
			if more != "" || first != lines[0] {
				threadHint = true
			}
			e.WriteString("    " + first + more + "\n")
		}
		if st.Budget > 0 && shown > 0 && b.Len()+e.Len() > st.Budget-300 {
			break
		}
		b.WriteString(e.String())
		shown++
	}
	cut := shown < len(in.Messages)
	next := in.Next
	if cut {
		last := in.Messages[shown-1]
		next = last.Sent.UTC().Format(time.RFC3339Nano) + "|" + last.ID
	}
	what := "messages"
	switch {
	case a.Thread != "":
		what = "messages in thread " + a.Thread
	case a.Sent:
		what = "sent messages"
	}
	switch {
	case len(in.Messages) == 0 && a.Cursor != "":
		fmt.Fprintf(&b, "[no more %s past this cursor]\n", what)
	case len(in.Messages) == 0 && a.Thread != "":
		fmt.Fprintf(&b, "[no %s for this session; thread ids come from %s]\n", what, st.cmd("flopwire inbox", "flopwire_inbox"))
	case len(in.Messages) == 0:
		fmt.Fprintf(&b, "[no %s for this session]\n", what)
	case next != "":
		budget := ""
		if cut || in.budgetCut {
			budget = fmt.Sprintf("output budget of %d bytes reached; ", st.Budget)
		}
		fmt.Fprintf(&b, "[%d %s shown, newest first, more follow; %snext: %s]\n", shown, what, budget, st.cmd("--cursor '"+next+"'", fmt.Sprintf("cursor=%q", next)))
	default:
		fmt.Fprintf(&b, "[%d %s, newest first, end of list]\n", shown, what)
	}
	if threadHint && !full {
		fmt.Fprintf(&b, "[bodies show their first line; %s shows a thread's whole text]\n", st.cmd("flopwire inbox --text --thread THREAD", `flopwire_inbox thread="THREAD"`))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// --- CLI ---

// errReported is returned once a command has written its own error (JSON
// on stderr): main exits 1 without printing it again.
var errReported = errors.New("error reported")

// busCmd is the CLI entry of peers, send and inbox. In JSON mode (the
// default) a failure is written to stderr as one JSON object and
// errReported is returned; with --text the error is returned for main to
// print.
func busCmd(ctx context.Context, verb string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	o, err := parseArgs(verb, args)
	st := busStyle{JSON: true}
	if err == nil {
		if o.on["help"] {
			_, err := io.WriteString(stdout, toolHelp[verb])
			return err
		}
		st.JSON = !o.on["text"]
		err = runBusVerb(ctx, verb, o, stdin, stdout, &st)
	} else {
		// The parser stopped early: --text may not have been read yet.
		st.JSON = !slices.Contains(textFlags(args), "--text")
		err = badUsage(err.Error(), "flopwire "+verb+" --help")
	}
	if err == nil || !st.JSON {
		return err
	}
	if werr := writeOut(stderr, errorJSON{Kind: "error", Error: asBusErr(err)}); werr != nil {
		return err
	}
	return errReported
}

// textFlags is args up to "--": the flags and positionals, not the text.
func textFlags(args []string) []string {
	if i := slices.Index(args, "--"); i >= 0 {
		return args[:i]
	}
	return args
}

func runBusVerb(ctx context.Context, verb string, o *opts, stdin io.Reader, stdout io.Writer, st *busStyle) error {
	socket := o.vals["socket"]
	if socket == "" {
		var err error
		if socket, err = defaultSocket(); err != nil {
			return err
		}
	}
	c := &busClient{socket: socket, caller: busDetect(), retry: busRetry}
	n, err := o.int("max-bytes")
	if err != nil {
		return badUsage(err.Error(), "--max-bytes 24000")
	}
	st.Budget = n
	limit, err := o.int("limit")
	if err != nil {
		return badUsage(err.Error(), "--limit 50")
	}
	switch verb {
	case "peers":
		if len(o.pos) > 0 {
			return badUsage(fmt.Sprintf("peers takes no arguments; got %q", strings.Join(o.pos, " ")), "flopwire peers --repo . --session 0b7e2c1a")
		}
		return runPeers(ctx, c, peersArgs{Repo: o.vals["repo"], User: o.vals["user"], Agent: o.vals["agent"], Session: o.vals["session"], Limit: limit}, stdout, *st)
	case "send":
		if len(o.pos) == 0 {
			return sendUsage(*st, "send needs a recipient and a text")
		}
		a := sendArgs{To: o.pos[0], Intent: o.vals["intent"], ReplyTo: o.vals["reply-to"], Refs: o.list["ref"], Repo: o.vals["repo"]}
		text := o.pos[1:]
		if len(text) == 1 && text[0] == "-" {
			raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				return badUsage("read the message from stdin: "+err.Error(), `echo "TEXT" | flopwire send 0b7e2c1a -- -`)
			}
			a.Text = strings.TrimRight(string(raw), "\r\n")
		} else {
			a.Text = strings.Join(text, " ")
		}
		return runSend(ctx, c, a, stdout, *st)
	case "inbox":
		if len(o.pos) > 0 {
			return badUsage(fmt.Sprintf("inbox takes no arguments; got %q", strings.Join(o.pos, " ")), "flopwire inbox --thread m1a2b3c4d5e6f7a8")
		}
		return runInbox(ctx, c, inboxArgs{Sent: o.on["sent"], Thread: o.vals["thread"], Cursor: o.vals["cursor"], Limit: limit}, stdout, *st)
	}
	return fmt.Errorf("unknown verb %q", verb)
}

// busDetect returns how the CLI finds the calling session; busRetry is
// the wait before a retry. Tests replace both.
var (
	busDetect = func() func(context.Context) (local.Caller, bool) { return local.NewDetector().Detect }
	busRetry  = defaultBusRetry
)

// busMain runs a bus verb as `flopwire VERB`, with the process's stdin,
// stdout and stderr.
func busMain(ctx context.Context, verb string, args []string) error {
	return busCmd(ctx, verb, args, os.Stdin, os.Stdout, os.Stderr)
}

// discovery is how an agent finds whom to message (issue #55): history
// first, then presence.
const discovery = `Find the recipient from history, then presence: flopwire sessions --repo R --branch B
--json (and flopwire read SESSION --outline: the commits it made) names the session
behind a change; flopwire peers --session ID shows whether that exact session is live;
then send to that id. Do not pick a recipient by a peer's title or current branch
alone: the title is its original task, and it may have switched branches since.`

func init() {
	toolHelp["peers"] = `flopwire peers — live agent sessions you can message, as JSON (yours first)

  flopwire peers --session 0b7e2c1a       is this exact session live? (from history)
  flopwire peers --repo .                 live sessions on this repo
  flopwire peers --text                   readable rows

` + discovery + `

JSON: {"kind":"peers","peers":[{"session":FULL ID,"agent","user","user_id","device",
"repo","branch","title","busy","own","seen_at"}…],"total":N,"more":bool,"limit":N,
"caller":{"session","agent"},"hint"}. Your own session (caller) is left out. busy: a turn
is running, so a message arrives at its next tool call; idle: it waits for that
session's human. more=true: the limit cut the list; hint says how to narrow it.
--text: SESSION  user  agent  live busy|idle  repo@branch  "title"

Filters  --session PREFIX  --repo .|NAME|/PATH  --user EMAIL|NAME  --agent claude|codex|devin
Output   --limit N (50, max 500)  --text  --max-bytes N (text)  (--json: the default)
Errors   JSON on stderr: {"kind":"error","error":{"code","detail","fix","example",…}}; exit 1
Socket   --socket PATH (default <config dir>/agent.sock: the device agent answers)
`
	toolHelp["send"] = `flopwire send — message another agent session, or a person's next session

  flopwire send 0b7e2c1a-0000-4000-8000-000000000001 -- "Heads-up: the list endpoint now returns a cursor."
  flopwire send @alex --intent request -- "Can you rebase api on main before 3pm UTC?"
  git diff --stat | flopwire send 4c19e0d2 --reply-to m1a2b3c4d5e6f7a8 -- -

<to> is a session id (or a unique prefix of one), or @user (an email or its local part):
the person's live session on --repo (default: yours), else their next session.
` + discovery + `

Write for a reader who knows nothing of your session: say what, why, and what you need.
The first line is the preview a human sees. At most 4000 bytes; attach longer material
with --ref ADDRESS (an archive address from grep, search or read).
--intent  request: expects a reply; inform (default): no reply; done: closes the
          thread and must not be answered.
Do not poll peers or send "are you done?": the receipt says when the message arrives,
and a reply arrives in your own context. Never ask a peer to do something your own
session was denied.

JSON: a receipt, never a reply: {"kind":"send_receipt","id","thread_id","state":
"queued"|"held","to":{"session","agent","user","repo","branch","live","busy"},
"sender","intent","sent","expires_at","redactions","from":{"session","agent"},
"arrives":"next_tool_call"|"next_prompt"|"when_accepted"|"next_session"|
"only_if_resumed","outcome":TEXT}. A refusal is {"kind":"error","error":{"code":
"thread_rate"|"session_rate"|"device_rate"|"user_rate"|"duplicate"|"recipient_full"|
"reply_to_done"|"unknown_recipient"|"ambiguous_recipient"|…,"detail","fix","example",
"message_id","candidates"}} on stderr, exit 1.
--text: one line, e.g. sent m7f3a to 0b7e2c1a (alex claude api@main): busy, arrives at
its next tool call

Flags    --intent request|inform|done  --reply-to ID  --ref ADDRESS (repeat)
         --repo .|NAME|*  --text  (--json: the default)
Text     after --; "-" reads it from stdin
Sender   the calling agent session (or FLOPWIRE_SESSION_ID); a send without one is refused
Socket   --socket PATH (default <config dir>/agent.sock: the device agent sends it)
`
	toolHelp["inbox"] = `flopwire inbox — this session's messages, received and sent, newest first, as JSON

  flopwire inbox                       received and sent messages
  flopwire inbox --sent                what you sent and its delivery state
  flopwire inbox --thread m1a2b3c4d5e6f7a8   one thread

JSON: {"kind":"inbox","session":FULL ID,"messages":[{"id","thread_id","reply_to","from",
"agent","user","repo","branch","sender","intent","body","refs","sent","expires_at",
"to_session","to_agent","to_user","addressed","direction":"sent"|"received","is_reply",
"state","refuse_reason","delivered_at","read_at"}…],"more":bool,"next":CURSOR}.
direction says who wrote it. A sent message's state is its delivery (queued, held,
claimed, delivered, read, expired, refused): delivered is not answered. An answer is a
received message whose reply_to names yours. more=true: pass next as --cursor.
--text: ID  received|sent  TIME  from|to WHO  intent  state, then the text indented
(a list shows first lines; --thread shows whole texts).
Delivery does not depend on calling inbox: messages arrive in your context on their own.

Output   --limit N (20, max 200)  --cursor C (next)  --text  --max-bytes N (text)
Errors   JSON on stderr, exit 1
Socket   --socket PATH (default <config dir>/agent.sock)
`
}
