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

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Request is one control-socket request: a JSON object on one line.
type Request struct {
	Op      string `json:"op"`                // "flush", "pass", "status", "repin", "redact" or "ping"
	Path    string `json:"path,omitempty"`    // flush: the transcript path
	Session string `json:"session,omitempty"` // flush: or its session id
	Index   string `json:"index,omitempty"`   // pass: the index the caller means; refused if it is not this agent's
	// redact: the message address (ADDRESS[:L1-L2]) and whether every
	// identical copy goes too (notes/redaction.md).
	Address   string `json:"address,omitempty"`
	AllCopies bool   `json:"all_copies,omitempty"`
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
	case req.Op == "redact":
		resp.Redacted, err = RedactLocal(ctx, a.store, req.Address, req.AllCopies)
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	case req.Op == "flush":
		resp.Path, err = a.FlushPath(ctx, req.Path, req.Session)
		resp.OK = err == nil
		if err != nil {
			resp.Error = err.Error()
		}
	default:
		resp.Error = "unknown op " + req.Op
	}
	b, _ := json.Marshal(resp)
	c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	c.Write(append(b, '\n'))
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
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
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
