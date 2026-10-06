package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Request is one control-socket request: a JSON object on one line.
type Request struct {
	// "flush", "pass", "status", "repin", "redact" or "ping"; for the
	// message bus "pending", "confirm", "held", "send", "peers", "inbox" or
	// "root" (the session a subagent's Session belongs to, BusRoot).
	Op      string `json:"op"`
	Path    string `json:"path,omitempty"`    // flush: the transcript path
	Session string `json:"session,omitempty"` // flush: or its session id; pending: the session asking
	// Event (flush from `flopwire hook`): the hook event that sent it. The
	// agent keeps the session's last one as its busy or idle state
	// (hookTurns); SessionEnd ends the session, and any other event of a
	// hook that started after an end resumes it (hookLifecycle).
	Event string `json:"event,omitempty"`
	// HookStart (flush, pending): when the asking hook process started,
	// unix ms.
	HookStart int64  `json:"hook_start,omitempty"`
	Index     string `json:"index,omitempty"` // pass: the index the caller means; refused if it is not this agent's
	// redact: the message address (ADDRESS[:L1-L2]) and whether every
	// identical copy goes too (notes/redaction.md).
	Address   string `json:"address,omitempty"`
	AllCopies bool   `json:"all_copies,omitempty"`

	// Message bus (devicebus). pending: Session and Agent (the harness,
	// when two share an id; "" for any). It leases the messages, and the
	// standing instruction when the session is owed it, to the caller;
	// confirm, with Session, the ids in IDs and Instruction, says the
	// caller printed them (devicebus.Bus.Confirm, ConfirmInstruction).
	// flush: Agent is the hook's harness, when it knows. send, peers, inbox: the request as
	// the server takes it; the agent sends it to the server, or answers it
	// on the device when no server is configured.
	Agent string `json:"agent,omitempty"`
	// pending: Limit and MaxBytes bound the messages taken, measured with
	// busrender.Size (JSON-encoded bytes); the rest stay queued for the
	// next call. Start is set
	// by a SessionStart hook (its source: startup, resume, clear,
	// compact): a source other than startup renews a confirmed standing
	// instruction (devicebus instruct.go).
	// Notice (pending): the caller can show the user a notice the model
	// does not see; the answer's Notice then names the held senders due
	// one (devicebus.HeldNotice).
	Notice   bool     `json:"notice,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	MaxBytes int      `json:"max_bytes,omitempty"`
	Start    string   `json:"start,omitempty"`
	IDs      []string `json:"ids,omitempty"` // confirm
	// Instruction (confirm): the standing instruction was printed too.
	Instruction bool                  `json:"instruction,omitempty"`
	Send        *busproto.SendRequest `json:"send,omitempty"`
	Peers       *busproto.PeersQuery  `json:"peers,omitempty"`
	Inbox       *busproto.InboxQuery  `json:"inbox,omitempty"`
}

// Response answers a Request.
type Response struct {
	Cowork     *CoworkStatus                 `json:"cowork,omitempty"`
	Extraction *transcript.ExtractionSummary `json:"extraction,omitempty"`
	OK         bool                          `json:"ok"`
	Path       string                        `json:"path,omitempty"` // flush: the source indexed
	Error      string                        `json:"error,omitempty"`
	// Sync is the upload state (status); nil when sync is off.
	Sync *devicesync.Status `json:"sync,omitempty"`
	// ServerCopies (status): sessions path rules removed locally whose
	// uploaded copies stay on the server (D18); nil when there are none.
	ServerCopies *ServerCopies `json:"server_copies,omitempty"`
	// Placements (status): stored session placements by what placed them
	// (localindex.PlacedBy*).
	Placements map[string]int `json:"placements,omitempty"`
	// Redacted (redact): rows masked in the local index.
	Redacted int `json:"redacted,omitempty"`

	// Message bus. Messages (pending): the session's undelivered messages,
	// oldest first, now leased to the caller; each one's Attempt above 1
	// marks a redelivery. Held (pending, held): senders
	// waiting for the user's acceptance, for the user-visible notice.
	// Sent, Peers, Inbox: the answers to send, peers and inbox. BusError: a
	// refusal with its code (and candidates or the refused message id);
	// Call returns it as the error. Bus (status): the bus state.
	Failures []busproto.DeliveryFailure `json:"failures,omitempty"`
	Messages []busproto.Envelope        `json:"messages,omitempty"`
	Held     []busproto.HeldSender      `json:"held,omitempty"`
	// Notice (pending with Notice): the held senders to tell the user
	// about now, at most once a day each; Console the web console page
	// for them.
	Notice   []busproto.HeldSender   `json:"notice,omitempty"`
	Console  string                  `json:"console,omitempty"`
	Sent     *busproto.SendResponse  `json:"sent,omitempty"`
	Peers    *busproto.PeersResponse `json:"peers,omitempty"`
	Inbox    *busproto.InboxResponse `json:"inbox,omitempty"`
	BusError *busproto.Error         `json:"bus_error,omitempty"`
	Bus      *devicebus.Status       `json:"bus,omitempty"`
	// Credential (status): the server credential the agent uses.
	Credential *Credential `json:"credential,omitempty"`
	// Instruct (pending): print the standing instruction before the
	// messages, then confirm it. The session is owed it until a hook
	// confirms it, and it is leased to one hook at a time, so a session
	// whose harness runs two hook configs (Devin runs
	// .claude/settings.json hooks too) sees it once, and a session whose
	// SessionStart hook was killed gets it at its next hook.
	Instruct bool `json:"instruct,omitempty"`
	// Excerpts (pending) maps a ref address in Messages to a short excerpt
	// from the local index; an address it cannot find is left out.
	Excerpts map[string]string `json:"excerpts,omitempty"`
	// Root (root): the session the request's session belongs to, itself
	// when it is not a subagent.
	Root string `json:"root,omitempty"`
}

// SocketPath is the control socket beside the client config: <dir of
// FLOPWIRE_CONFIG or the user config dir>/flopwire/agent.sock.
func SocketPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "agent.sock")
}

// Serve answers control requests on a unix socket at path until ctx ends.
// The socket is created mode 0600 in a 0700 directory; only the owner can
// ask for a flush.
func (a *Agent) Serve(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if c, err := net.DialTimeout("unix", path, 200*time.Millisecond); err == nil {
		c.Close()
		return fmt.Errorf("agent: another agent is listening on %s", path)
	}
	os.Remove(path) // stale socket from a crashed agent
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	defer os.Remove(path)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go a.serveConn(ctx, c)
	}
}

// untilClientLeaves returns ctx cancelled also when the client closes c.
// A client sends one request line and then only reads, so any read
// returning (EOF, reset, or the connection's deadline) means it is gone.
// stop releases the watcher; serveConn closing c ends its read.
func untilClientLeaves(ctx context.Context, c net.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		var b [1]byte
		c.Read(b[:])
		cancel()
	}()
	return ctx, cancel
}

// testHookStatus runs at the start of a status request, with its context.
var testHookStatus func(context.Context)

func (a *Agent) serveConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	var req Request
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err == nil || len(line) > 0 {
		err = json.Unmarshal(line, &req)
	}
	resp := Response{}
	switch {
	case err != nil:
		resp.Error = "bad request: " + err.Error()
	case req.Op == "ping":
		resp.OK = true
	case req.Op == "pass":
		c.SetDeadline(time.Time{}) // a pass takes as long as the changed sources need
		if req.Index != "" && !sameFile(req.Index, a.store.Path()) {
			err = fmt.Errorf("this agent indexes %s, not %s", a.store.Path(), req.Index)
		} else {
			err = a.Pass(ctx)
		}
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	case req.Op == "status":
		// The index summary scans every source: stop it if the client
		// gives up, rather than hold a read connection for nobody.
		ctx, stop := untilClientLeaves(ctx, c)
		defer stop()
		if testHookStatus != nil {
			testHookStatus(ctx)
		}
		resp.OK = true
		if st, ok := a.cfg.Sync.(interface{ Status() devicesync.Status }); ok {
			v := st.Status()
			resp.Sync = &v
		}
		resp.ServerCopies = a.serverCopiesNotice()
		resp.Placements = a.placementCounts()
		resp.Cowork = a.coworkStatus()
		if a.cfg.Bus != nil {
			st := a.cfg.Bus.Status(ctx)
			resp.Bus = &st
		}
		if a.cfg.Credential != nil {
			c := a.cfg.Credential()
			resp.Credential = &c
		}
		var err error
		resp.Extraction, err = a.store.ExtractionSummary(ctx)
		if err != nil {
			resp.OK = false
			resp.Error = err.Error()
		}
	case req.Op == "repin":
		// `flopwire login` saved a server pin: a sync stopped by a pin
		// mismatch re-reads it and resumes.
		resp.OK = true
		if rc, ok := a.cfg.Sync.(interface{ Recheck() }); ok {
			rc.Recheck()
		}
		if a.cfg.Bus != nil {
			a.cfg.Bus.Recheck()
		}
	case req.Op == "redact":
		resp.Redacted, err = RedactLocal(ctx, a.store, req.Address, req.AllCopies)
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	case req.Op == "flush":
		a.noteHookEvent(req.Agent, req.Session, req.Event, hookStart(req, a.now()))
		a.hookLifecycle(ctx, req)
		if d := a.storeOf(transcript.AgentOpencode); d != nil && req.Agent == string(transcript.AgentOpencode) {
			// opencode's store holds every session: poll it (at most once a
			// second while it changes).
			a.pollStore(ctx, d, false, false)
			resp.OK = true
			break
		}
		resp.Path, err = a.FlushPath(ctx, req.Path, req.Session)
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	case req.Op == "root":
		resp.Root, resp.OK = a.BusRoot(ctx, req.Agent, req.Session), true
	case req.Op == "pending", req.Op == "confirm", req.Op == "held", req.Op == "send", req.Op == "peers", req.Op == "inbox":
		a.serveBus(ctx, req, &resp)
	default:
		resp.Error = "unknown op " + req.Op
	}
	b, _ := json.Marshal(resp)
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, werr := c.Write(append(b, '\n'))
	if werr != nil && req.Op == "pending" && resp.Instruct {
		// The hook gave up before the answer: the instruction is owed as
		// before, for the session's next hook.
		if rerr := a.cfg.Bus.ReturnInstruction(ctx, req.Session); rerr != nil {
			a.log.Warn("agent: the instruction taken by a hook that left waits for its lease to end", "session", req.Session, "err", rerr)
		}
	}
	if werr != nil && req.Op == "pending" && len(resp.Messages) > 0 {
		// The hook gave up before the answer (its budget ran out) and
		// will not print these messages: queue them again for its
		// session's next hook now, unmarked, rather than when the lease
		// ends.
		ids := make([]string, len(resp.Messages))
		for i, m := range resp.Messages {
			ids[i] = m.ID
		}
		if rerr := a.cfg.Bus.Requeue(ctx, ids); rerr != nil {
			a.log.Warn("agent: messages taken by a hook that left wait for their lease to end", "ids", ids, "err", rerr)
		}
	}
}

// busCallTimeout bounds a send, peers or inbox request to the server.
const busCallTimeout = 20 * time.Second

// serveBus answers the message bus requests. pending, confirm and held
// use only local state, so a hook never waits on the network.
func (a *Agent) serveBus(ctx context.Context, req Request, resp *Response) {
	b := a.cfg.Bus
	if b == nil {
		resp.Error = "messaging is off in this agent"
		return
	}
	var err error
	switch req.Op {
	case "pending":
		b.Nudge(req.Session, req.Agent) // a new session: report it now
		resp.Failures, err = b.TakeFailures(ctx, req.Session, req.Agent)
		if err != nil {
			break
		}
		budget := req.MaxBytes
		if len(resp.Failures) > 0 && budget > 2000 {
			budget -= 2000
		}
		lim := devicebus.Limit{Count: req.Limit, Bytes: budget, Sep: busrender.SepLen, Size: busrender.Size}
		ins := &devicebus.Instruction{Source: req.Start, HookStart: hookStart(req, a.now()),
			Bytes: busrender.EncodedLen(busrender.StandingInstruction) + busrender.SepLen}
		resp.Instruct, resp.Messages, err = b.TakeWith(ctx, req.Session, req.Agent, lim, ins)
		resp.Held = b.Held()
		if req.Notice {
			if resp.Notice, _ = b.HeldNotice(ctx); len(resp.Notice) > 0 {
				resp.Console = a.cfg.Console
			}
		}
		resp.Excerpts = a.refExcerpts(ctx, resp.Messages)
	case "confirm":
		err = b.Confirm(ctx, req.Session, req.IDs)
		if err == nil && req.Instruction {
			err = b.ConfirmInstruction(ctx, req.Session)
		}
	case "held":
		resp.Held = b.Held()
	default:
		cctx, cancel := context.WithTimeout(ctx, busCallTimeout)
		defer cancel()
		switch {
		case req.Op == "send" && req.Send != nil:
			var out busproto.SendResponse
			if out, err = b.Send(cctx, *req.Send); err == nil {
				resp.Sent = &out
			}
		case req.Op == "peers" && req.Peers != nil:
			var out busproto.PeersResponse
			if out, err = b.Peers(cctx, *req.Peers); err == nil {
				resp.Peers = &out
			}
		case req.Op == "inbox" && req.Inbox != nil:
			var out busproto.InboxResponse
			if out, err = b.Inbox(cctx, *req.Inbox); err == nil {
				resp.Inbox = &out
			}
		default:
			err = fmt.Errorf("%s: the request is missing", req.Op)
		}
	}
	resp.OK = err == nil
	if err != nil {
		resp.Error = err.Error()
		var be *busproto.Error
		if errors.As(err, &be) {
			resp.BusError = be
		}
	}
}

// placementCounts counts the stored placements by what placed them.
func (a *Agent) placementCounts() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]int{}
	for _, p := range a.places {
		out[p.how]++
		if p.cands != "" {
			out["ambiguous"]++ // also counted by how
		}
	}
	return out
}

// Pass runs one full pass on a running agent: a sweep of every source
// (and Devin's store), then waits until the queue drains and the index has
// committed. `flopwire agent run --once` asks for it over the control socket
// when this agent holds the index lock (decision D12).
//
// A pass asked while the agent starts waits for the first discovery pass,
// like a flush: run before load, its full merge would make load skip the
// stored gates.
func (a *Agent) Pass(ctx context.Context) error {
	if err := a.waitDiscovered(ctx); err != nil {
		return err
	}
	if err := a.sweep(ctx); err != nil {
		return err
	}
	a.pollStores(ctx, true, true)
	a.WaitIdle()
	return a.store.Sync(ctx)
}

func sameFile(a, b string) bool {
	fa, err1 := fsprobe.Stat(a)
	fb, err2 := fsprobe.Stat(b)
	if err1 != nil || err2 != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return os.SameFile(fa, fb)
}

// Call sends one request to the agent listening at path.
func Call(ctx context.Context, path string, req Request) (Response, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("agent not running (%s): %w", path, err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return resp, err
	}
	if !resp.OK {
		if resp.BusError != nil {
			return resp, resp.BusError
		}
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// RedactLocal applies a message redaction (ADDRESS[:L1-L2]) to a local
// index: the agent's, or the CLI's own when no agent runs.
func RedactLocal(ctx context.Context, store *localindex.Store, address string, allCopies bool) (int, error) {
	addr, from, to, err := format.SplitLineRange(address)
	if err != nil {
		return 0, err
	}
	a, err := format.ParseAddress(addr)
	if err != nil {
		return 0, err
	}
	r := localindex.LocalRedaction{From: from, To: to, AllCopies: allCopies}
	switch a.Kind {
	case format.AddrMessage:
		r.Session, r.Ordinal = a.Session, a.Ordinal
	case format.AddrPath:
		r.Path, r.Line = a.Path, a.PathNo
	default:
		return 0, fmt.Errorf("redact needs a message address (SESSION/ORDINAL or /path/file.jsonl:LINE), not %q", addr)
	}
	return store.RedactMessage(ctx, r)
}
