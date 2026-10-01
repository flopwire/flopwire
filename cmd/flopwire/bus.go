package main

// The message bus verbs (notes/message-bus/plan.md §3): peers, send and
// inbox, the same three in the CLI and over MCP (flopwire_peers,
// flopwire_send, flopwire_inbox). They never call the server themselves:
// the device agent answers them over its control socket, through the team
// server or, without one, between this device's own sessions. Every
// request names the calling session, found by exact evidence only
// (local.Detector, or the MCP request's _meta); a send without one is
// refused rather than sent anonymously.

import (
	"context"
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

// busStyle picks how hints read (CLI flags or MCP arguments) and the
// output budget, as format.Style does for the retrieval tools.
type busStyle struct {
	MCP    bool
	Budget int
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

// defaultSocket is the agent's control socket beside the client config.
func defaultSocket() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agent.sock"), nil
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
		return resp, fmt.Errorf("the Flopwire device agent is not running (nothing answers on %s). Messages go through it. Fix: start it with flopwire agent run, or start the service the installer set up; flopwire agent status checks it", c.socket)
	case msg == "messaging is off in this agent":
		return resp, errors.New("the device agent runs with messaging off: its local inbox (bus.db) could not be opened. Fix: read the agent log, then restart the agent; flopwire agent status shows the state")
	case strings.HasPrefix(msg, "unknown op "):
		return resp, errors.New("the running device agent predates messaging. Fix: restart it (flopwire agent run) so it runs this version")
	case errors.Is(err, context.DeadlineExceeded):
		return resp, fmt.Errorf("the device agent did not answer within %s; the team server may be slow or unreachable. flopwire agent status shows the state", busTimeout)
	}
	return resp, fmt.Errorf("the device agent could not complete the request: %s. flopwire agent status shows the state", msg)
}

// retryable reports whether the agent refused the session only because it
// has not seen it yet (index lag, or the next presence push); a session a
// path rule keeps off the server is not retried.
func retryable(err error) bool {
	var be *busproto.Error
	return errors.As(err, &be) && be.Code == busproto.CodeSessionNotOnDevice && !strings.Contains(be.Detail, "path rule")
}

// callRetry is call with the one retry of a session the agent has not
// seen yet.
func (c *busClient) callRetry(ctx context.Context, req agent.Request) (agent.Response, error) {
	resp, err := c.call(ctx, req)
	if err == nil || !retryable(err) {
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
func errNoCaller(verb string, st busStyle) error {
	how := "set FLOPWIRE_SESSION_ID (and FLOPWIRE_AGENT: claude, codex or devin) to your session's id. Example: FLOPWIRE_SESSION_ID=0b7e2c1a-… flopwire " + verb + " …"
	if st.MCP {
		how = "set FLOPWIRE_SESSION_ID and FLOPWIRE_AGENT in this MCP server's environment, or call flopwire " + verb + " from your shell tool instead"
	}
	return fmt.Errorf("%s: cannot identify the calling session (no Claude Code session file, no Codex thread id, no Devin session lock, no FLOPWIRE_SESSION_ID), and messages always name the session they come from or belong to. Fix: run it from inside an agent session, or %s", verb, how)
}

// --- arguments ---

type peersArgs struct {
	Repo, User, Agent string
	JSON              bool
}

type sendArgs struct {
	To, Text, Intent, ReplyTo, Repo string
	Refs                            []string
	JSON                            bool
}

type inboxArgs struct {
	Sent           bool
	Thread, Cursor string
	Limit          int
	JSON           bool
}

// --- peers ---

// runPeers lists live sessions. The calling session is left out when it is
// known; without one the list is still useful, and says so.
func runPeers(ctx context.Context, c *busClient, a peersArgs, w io.Writer, st busStyle) error {
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
		return err
	}
	var peers []busproto.Peer
	if resp.Peers != nil {
		for _, p := range resp.Peers.Peers {
			if known && p.Session == self.SessionID {
				continue
			}
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
	if a.JSON {
		return writeJSON(w, busproto.PeersResponse{Peers: append([]busproto.Peer{}, peers...)})
	}
	return writePeers(w, peers, known, a, st)
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

func writePeers(w io.Writer, peers []busproto.Peer, known bool, a peersArgs, st busStyle) error {
	var b strings.Builder
	if !known {
		fmt.Fprintf(&b, "[your own session could not be identified, so it may be listed]\n")
	}
	ids := make([]string, len(peers))
	for i, p := range peers {
		ids[i] = p.Session
	}
	shown, own := 0, 0
	for _, p := range peers {
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
	switch {
	case len(peers) == 0 && (a.Repo != "" || a.User != "" || a.Agent != ""):
		fmt.Fprintf(&b, "[no other live session matches; %s lists every live session]\n", st.cmd("flopwire peers without filters", "flopwire_peers without arguments"))
	case len(peers) == 0:
		fmt.Fprintf(&b, "[no other live sessions; %s queues a message for a person's next session]\n", st.cmd("flopwire send @user -- TEXT", `flopwire_send to="@user"`))
	case shown < len(peers):
		fmt.Fprintf(&b, "[%d of %d live sessions shown; output budget of %d bytes reached; narrow with %s]\n", shown, len(peers), st.Budget, st.cmd("--repo, --user or --agent", "repo, user or agent"))
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

// runSend sends one message as the calling session and prints its outcome
// in one line.
func runSend(ctx context.Context, c *busClient, a sendArgs, w io.Writer, st busStyle) error {
	if strings.TrimSpace(a.To) == "" {
		return errors.New(sendUsage(st, "a recipient is required"))
	}
	if strings.TrimSpace(a.Text) == "" {
		return errors.New(sendUsage(st, "the message text is empty"))
	}
	if n := len(a.Text); n > busproto.MaxBodyBytes {
		return fmt.Errorf("the message is %d bytes; the cap is %d. Fix: keep the message to what the recipient must act on, and attach longer material by its archive address with %s (the recipient reads it with flopwire read). Example: %s",
			n, busproto.MaxBodyBytes, st.cmd("--ref ADDRESS", "refs"), st.cmd(`flopwire send 0b7e2c1a --ref 4c19e0d2/28672 -- "The failing test output is at the ref."`, `flopwire_send to="0b7e2c1a" refs=["4c19e0d2/28672"] message="The failing test output is at the ref."`))
	}
	if _, err := busproto.ParseIntent(a.Intent); err != nil {
		return fmt.Errorf("intent %q: want request (expects a reply), inform (the default: no reply expected) or done (closes the thread; never answered). Example: %s",
			a.Intent, st.cmd("flopwire send 0b7e2c1a --intent request -- TEXT", `flopwire_send to="0b7e2c1a" intent="request" message="…"`))
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
			return errors.New(refusal(be, req, st))
		}
		return err
	}
	if resp.Sent == nil {
		return errors.New("the device agent answered the send without an outcome; flopwire agent status shows the state")
	}
	if a.JSON {
		return writeJSON(w, resp.Sent)
	}
	_, err = io.WriteString(w, sendOutcome(*resp.Sent)+"\n")
	return err
}

func sendUsage(st busStyle, why string) string {
	if st.MCP {
		return why + `. flopwire_send takes to (a session id prefix from flopwire_peers, or @user) and message. Example: flopwire_send to="0b7e2c1a" message="Heads-up: the list endpoint now returns a cursor."`
	}
	return why + `. Usage: flopwire send <to> [--intent request|inform|done] [--reply-to ID] [--ref ADDRESS]... [--repo R] -- <text | ->. Example: flopwire send 0b7e2c1a -- "Heads-up: the list endpoint now returns a cursor."`
}

// expiry is a time as the outcome line prints it: UTC to the minute.
func expiry(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }

// sendOutcome is the one line a send prints (plan §3), so the sender never
// polls: where the message went and when it arrives.
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

// refusal turns a refused send into cause, fix and a valid example.
func refusal(be *busproto.Error, req busproto.SendRequest, st busStyle) string {
	cause := format.Clean(be.Detail)
	sentList := st.cmd("flopwire inbox --sent", "flopwire_inbox sent=true")
	var fix, example string
	switch be.Code {
	case busproto.CodeUnknownRecipient:
		fix = "address a live session by an id prefix from " + st.cmd("flopwire peers", "flopwire_peers") + ", or a person as @user (their email or its local part)"
		example = st.cmd(`flopwire send @alex -- "TEXT"`, `flopwire_send to="@alex" message="…"`)
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
			cause += "; candidates:\n" + strings.Join(lines, "\n") + "\n"
		}
		fix = "use one of the candidates as printed"
		example = st.cmd(fmt.Sprintf(`flopwire send %s -- "TEXT"`, format.Clean(first)), fmt.Sprintf(`flopwire_send to=%q message="…"`, format.Clean(first)))
	case busproto.CodeReplyToDone:
		fix = "do not answer it: done closes a thread. If there is new work, start a new thread without reply_to"
		example = st.cmd(fmt.Sprintf(`flopwire send %s -- "TEXT"`, req.To), fmt.Sprintf(`flopwire_send to=%q message="…"`, req.To))
	case busproto.CodeThreadRate:
		fix = "stop this exchange for now: the thread is looping. Settle what is left with your human, or wait an hour"
		example = st.cmd("flopwire inbox --thread THREAD", `flopwire_inbox thread="THREAD"`) + " to re-read the thread"
	case busproto.CodeSessionRate, busproto.CodeDeviceRate, busproto.CodeUserRate:
		fix = "send less: put what you have to say into one message, and wait before sending again"
		example = sentList + " shows what is still undelivered"
	case busproto.CodeDuplicate:
		fix = "do not resend: the first copy is on its way"
		example = sentList + " shows its state"
	case busproto.CodeRecipientFull:
		fix = "wait until the recipient reads what it has; do not resend"
		example = sentList + " shows your undelivered messages"
	case busproto.CodeNotFound:
		fix = "reply_to takes a message id this session sent or received"
		example = st.cmd("flopwire inbox", "flopwire_inbox") + " lists them"
	case busproto.CodeSessionNotOnDevice:
		if strings.Contains(be.Detail, "path rule") {
			fix = "messaging is not available from this session: its transcripts stay on this device, so nothing about it may reach the team server"
			example = "ask your human to send it, or send from a session in another repo"
		} else {
			fix = "the device agent has not seen this session yet; try again in a few seconds"
			example = "flopwire agent status shows how many live sessions the agent reports"
		}
	default:
		fix = "check the arguments"
		example = sendUsage(st, "")
		example = strings.TrimPrefix(example, ". ")
	}
	out := fmt.Sprintf("refused (%s): %s\nFix: %s.\nExample: %s", be.Code, strings.TrimRight(cause, "\n"), fix, example)
	if be.MessageID != "" {
		out += fmt.Sprintf("\nThe refused message %s is listed in %s.", be.MessageID, sentList)
	}
	return out
}

// --- inbox ---

// runInbox lists the calling session's messages, newest first.
func runInbox(ctx context.Context, c *busClient, a inboxArgs, w io.Writer, st busStyle) error {
	self, ok := c.caller(ctx)
	if !ok {
		return errNoCaller("inbox", st)
	}
	if a.Limit < 0 || a.Limit > busproto.InboxMaxLimit {
		return fmt.Errorf("limit: 1 to %d (default %d)", busproto.InboxMaxLimit, busproto.InboxDefaultLimit)
	}
	q := busproto.InboxQuery{Session: self.SessionID, Agent: string(self.Agent), SentOnly: a.Sent, Thread: strings.TrimSpace(a.Thread), Limit: a.Limit, Before: a.Cursor}
	resp, err := c.callRetry(ctx, agent.Request{Op: "inbox", Inbox: &q})
	if err != nil {
		var be *busproto.Error
		if errors.As(err, &be) {
			return fmt.Errorf("inbox refused (%s): %s", be.Code, format.Clean(be.Detail))
		}
		return err
	}
	in := busproto.InboxResponse{Messages: []busproto.InboxItem{}}
	if resp.Inbox != nil {
		in = *resp.Inbox
		if in.Messages == nil {
			in.Messages = []busproto.InboxItem{}
		}
	}
	if a.JSON {
		return writeJSON(w, in)
	}
	return writeInbox(w, in, a, st)
}

// writeInbox prints one entry per message: a header line, then the body
// indented. A list shows each body's first line; a thread (--thread)
// shows whole bodies and refs. Bodies are written by other agents and
// people: indented, they cannot pass for a header or the footer.
func writeInbox(w io.Writer, in busproto.InboxResponse, a inboxArgs, st busStyle) error {
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
		if cut {
			budget = fmt.Sprintf("output budget of %d bytes reached; ", st.Budget)
		}
		fmt.Fprintf(&b, "[%d %s shown, newest first, more follow; %snext: %s]\n", shown, what, budget, st.cmd("--cursor '"+next+"'", fmt.Sprintf("cursor=%q", next)))
	default:
		fmt.Fprintf(&b, "[%d %s, newest first, end of list]\n", shown, what)
	}
	if threadHint && !full {
		fmt.Fprintf(&b, "[bodies show their first line; %s shows a thread's whole text]\n", st.cmd("flopwire inbox --thread THREAD", `flopwire_inbox thread="THREAD"`))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// --- CLI ---

// busCmd is the CLI entry of peers, send and inbox.
func busCmd(ctx context.Context, verb string, args []string, stdin io.Reader, stdout io.Writer) error {
	o, err := parseArgs(verb, args)
	if err != nil {
		return err
	}
	if o.on["help"] {
		_, err := io.WriteString(stdout, toolHelp[verb])
		return err
	}
	socket := o.vals["socket"]
	if socket == "" {
		if socket, err = defaultSocket(); err != nil {
			return err
		}
	}
	c := &busClient{socket: socket, caller: busDetect(), retry: busRetry}
	n, err := o.int("max-bytes")
	if err != nil {
		return err
	}
	st := busStyle{Budget: n}
	switch verb {
	case "peers":
		if len(o.pos) > 0 {
			return fmt.Errorf("peers takes no arguments; got %q (filters: --repo, --user, --agent)", strings.Join(o.pos, " "))
		}
		return runPeers(ctx, c, peersArgs{Repo: o.vals["repo"], User: o.vals["user"], Agent: o.vals["agent"], JSON: o.on["json"]}, stdout, st)
	case "send":
		if len(o.pos) == 0 {
			return errors.New(sendUsage(st, "send needs a recipient and a text"))
		}
		a := sendArgs{To: o.pos[0], Intent: o.vals["intent"], ReplyTo: o.vals["reply-to"], Refs: o.list["ref"], Repo: o.vals["repo"], JSON: o.on["json"]}
		text := o.pos[1:]
		if len(text) == 1 && text[0] == "-" {
			raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
			if err != nil {
				return fmt.Errorf("read the message from stdin: %w", err)
			}
			a.Text = strings.TrimRight(string(raw), "\r\n")
		} else {
			a.Text = strings.Join(text, " ")
		}
		return runSend(ctx, c, a, stdout, st)
	case "inbox":
		if len(o.pos) > 0 {
			return fmt.Errorf("inbox takes no arguments; got %q (--thread ID shows one thread)", strings.Join(o.pos, " "))
		}
		limit, err := o.int("limit")
		if err != nil {
			return err
		}
		return runInbox(ctx, c, inboxArgs{Sent: o.on["sent"], Thread: o.vals["thread"], Cursor: o.vals["cursor"], Limit: limit, JSON: o.on["json"]}, stdout, st)
	}
	return fmt.Errorf("unknown verb %q", verb)
}

// busDetect returns how the CLI finds the calling session; busRetry is
// the wait before a retry. Tests replace both.
var (
	busDetect = func() func(context.Context) (local.Caller, bool) { return local.NewDetector().Detect }
	busRetry  = defaultBusRetry
)

// busMain runs a bus verb as `flopwire VERB`, with the process's stdin
// and stdout.
func busMain(ctx context.Context, verb string, args []string) error {
	return busCmd(ctx, verb, args, os.Stdin, os.Stdout)
}

func init() {
	toolHelp["peers"] = `flopwire peers — list live agent sessions you can message (yours first)

  flopwire peers                       every live session on the team (or this device)
  flopwire peers --repo .              sessions on this repo
  flopwire peers --user alex --agent codex

One row per live session, your own session left out:
  SESSION  user  agent  live busy|idle  repo@branch  "title"
busy: a turn is running, so a message arrives at its next tool call. idle: it waits
for that session's human to type. Address a session by its first column.

Filters  --repo .|NAME|/PATH  --user EMAIL|NAME  --agent claude|codex|devin
Output   --json  --max-bytes N
Socket   --socket PATH (default <config dir>/agent.sock: the device agent answers)
`
	toolHelp["send"] = `flopwire send — message another agent session, or a person's next session

  flopwire send 0b7e2c1a -- "Heads-up: the list endpoint now returns a cursor."
  flopwire send @alex --intent request -- "Can you rebase api on main before 3pm UTC?"
  git diff --stat | flopwire send 4c19e0d2 --reply-to m1a2b3c4d5e6f7a8 -- -

<to> is a session id prefix from flopwire peers, or @user (an email or its local part):
the person's live session on --repo (default: yours), else their next session.
Write for a reader who knows nothing of your session: say what, why, and what you need.
The first line is the preview a human sees. At most 4000 bytes; attach longer material
with --ref ADDRESS (an archive address from grep, search or read).
--intent  request: expects a reply; inform (default): no reply; done: closes the
          thread and must not be answered.
Do not poll: the one-line result says when the message arrives. Never ask a peer to do
something your own session was denied.

Flags    --intent request|inform|done  --reply-to ID  --ref ADDRESS (repeat)
         --repo .|NAME|*  --json
Text     after --; "-" reads it from stdin
Sender   the calling agent session (or FLOPWIRE_SESSION_ID); a send without one is refused
Socket   --socket PATH (default <config dir>/agent.sock: the device agent sends it)
`
	toolHelp["inbox"] = `flopwire inbox — this session's messages, received and sent, newest first

  flopwire inbox                       every thread's messages, first line of each
  flopwire inbox --sent                what you sent and its state
  flopwire inbox --thread m1a2b3c4d5e6f7a8   one thread, whole text

Each message prints ID  received|sent  TIME  from|to WHO  intent  state, then its
text indented. States: queued, held (the recipient's human has not accepted you),
claimed, delivered, read, expired, refused (with the reason). Delivery does not depend
on calling inbox: messages for this session arrive on their own.

Output   --limit N (50, max 200)  --cursor C (from the footer)  --json  --max-bytes N
Socket   --socket PATH (default <config dir>/agent.sock)
`
}
