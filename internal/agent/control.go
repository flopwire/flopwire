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
	// message bus "pending", "held", "send", "peers" or "inbox".
	Op      string `json:"op"`
	Path    string `json:"path,omitempty"`    // flush: the transcript path
	Session string `json:"session,omitempty"` // flush: or its session id; pending: the session asking
	// Event (flush from `flopwire hook`): the hook event that sent it. The
	// agent keeps the session's last one as its busy or idle state
	// (hookTurns).
	Event string `json:"event,omitempty"`
	Index string `json:"index,omitempty"` // pass: the index the caller means; refused if it is not this agent's
	// redact: the message address (ADDRESS[:L1-L2]) and whether every
	// identical copy goes too (notes/redaction.md).
	Address   string `json:"address,omitempty"`
	AllCopies bool   `json:"all_copies,omitempty"`

	// Message bus (devicebus). pending: Session and Agent (the harness,
	// when two share an id; "" for any). send, peers, inbox: the request as
	// the server takes it; the agent sends it to the server, or answers it
	// on the device when no server is configured.
	Agent string `json:"agent,omitempty"`
	// pending: Limit and MaxBytes bound the messages taken, measured with
	// busrender.Size (JSON-encoded bytes); the rest stay queued for the
	// next call. Start is set
	// by a SessionStart hook (its source: startup, resume, clear,
	// compact): the answer's Instruct then says whether to print the
	// standing instruction.
	Limit    int                   `json:"limit,omitempty"`
	MaxBytes int                   `json:"max_bytes,omitempty"`
	Start    string                `json:"start,omitempty"`
	Send     *busproto.SendRequest `json:"send,omitempty"`
	Peers    *busproto.PeersQuery  `json:"peers,omitempty"`
	Inbox    *busproto.InboxQuery  `json:"inbox,omitempty"`
}

// Response answers a Request.
type Response struct {
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
	// oldest first, now marked delivered. Held (pending, held): senders
	// waiting for the user's acceptance, for the user-visible notice.
	// Sent, Peers, Inbox: the answers to send, peers and inbox. BusError: a
	// refusal with its code (and candidates or the refused message id);
	// Call returns it as the error. Bus (status): the bus state.
	Messages []busproto.Envelope     `json:"messages,omitempty"`
	Held     []busproto.HeldSender   `json:"held,omitempty"`
	Sent     *busproto.SendResponse  `json:"sent,omitempty"`
	Peers    *busproto.PeersResponse `json:"peers,omitempty"`
	Inbox    *busproto.InboxResponse `json:"inbox,omitempty"`
	BusError *busproto.Error         `json:"bus_error,omitempty"`
	Bus      *devicebus.Status       `json:"bus,omitempty"`
	// Instruct (pending with Start): print the standing instruction. Only
	// the first SessionStart hook for a session and source within
	// startWindow gets it, so a session whose harness runs two hook
	// configs (Devin runs .claude/settings.json hooks too) sees it once.
	Instruct bool `json:"instruct,omitempty"`
	// Excerpts (pending) maps a ref address in Messages to a short excerpt
	// from the local index; an address it cannot find is left out.
	Excerpts map[string]string `json:"excerpts,omitempty"`
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
		resp.OK = true
		if st, ok := a.cfg.Sync.(interface{ Status() devicesync.Status }); ok {
			v := st.Status()
			resp.Sync = &v
		}
		resp.ServerCopies = a.serverCopiesNotice()
		resp.Placements = a.placementCounts()
		if a.cfg.Bus != nil {
			st := a.cfg.Bus.Status(ctx)
			resp.Bus = &st
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
		a.noteHookEvent(req.Session, req.Event)
		resp.Path, err = a.FlushPath(ctx, req.Path, req.Session)
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	case req.Op == "pending", req.Op == "held", req.Op == "send", req.Op == "peers", req.Op == "inbox":
		a.serveBus(ctx, req, &resp)
	default:
		resp.Error = "unknown op " + req.Op
	}
	b, _ := json.Marshal(resp)
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, werr := c.Write(append(b, '\n'))
	if werr != nil && req.Op == "pending" && resp.Instruct {
		a.releaseStart(req.Session, req.Start)
	}
	if werr != nil && req.Op == "pending" && len(resp.Messages) > 0 {
		// The hook gave up before the answer (its budget ran out) and
		// will not print these messages: queue them again for its
		// session's next hook rather than lose them.
		ids := make([]string, len(resp.Messages))
		for i, m := range resp.Messages {
			ids[i] = m.ID
		}
		if rerr := a.cfg.Bus.Requeue(ctx, ids); rerr != nil {
			a.log.Warn("agent: messages taken by a hook that left are lost", "ids", ids, "err", rerr)
		}
	}
}

// busCallTimeout bounds a send, peers or inbox request to the server.
const busCallTimeout = 20 * time.Second

// serveBus answers the message bus requests. pending and held read only
// local state, so a hook never waits on the network.
func (a *Agent) serveBus(ctx context.Context, req Request, resp *Response) {
	b := a.cfg.Bus
	if b == nil {
		resp.Error = "messaging is off in this agent"
		return
	}
	var err error
	switch req.Op {
	case "pending":
		lim := devicebus.Limit{Count: req.Limit, Bytes: req.MaxBytes, Sep: busrender.SepLen, Size: busrender.Size}
		if req.Start != "" && req.Session != "" {
			resp.Instruct = a.claimStart(req.Session, req.Start)
			if resp.Instruct && lim.Bytes > 0 {
				lim.Bytes = max(1, lim.Bytes-busrender.EncodedLen(busrender.StandingInstruction)-busrender.SepLen)
			}
		}
		resp.Messages, err = b.Take(ctx, req.Session, req.Agent, lim)
		resp.Held = b.Held()
		if err != nil && resp.Instruct {
			a.releaseStart(req.Session, req.Start)
			resp.Instruct = false
		}
		resp.Excerpts = a.refExcerpts(ctx, resp.Messages)
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
	a.pollDevin(ctx, true, true)
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
