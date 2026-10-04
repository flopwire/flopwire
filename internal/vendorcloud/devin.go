package vendorcloud

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Devin reaches Devin cloud sessions with the device's Devin CLI login,
// over the Agent Client Protocol relay `devin acp --cloud` (JSON-RPC, one
// message per line on stdio).
//
// List calls session/list. It lists the organization's sessions, so only
// those the device's person created are kept (_meta
// "cognition.ai/creatorUserId", matched to the "User ID" of `devin auth
// status`), and not archived or exited ones. A turn runs while _meta
// "cognition.ai/statusEnum" is "working".
//
// Push calls session/prompt and returns once the session echoes the text
// as a user_message_chunk. The prompt's answer comes only when the turn
// ends; closing the relay before then does not cancel it. The first agent
// message or thought chunk after the echo is the model working with the
// text in context (Pushed.ReadAt); Push waits ReadWait for it.
type Devin struct {
	// Bin is the devin executable.
	Bin string
	// Start opens a relay; default runs `Bin acp --cloud`. Tests replace it.
	Start func(ctx context.Context) (io.WriteCloser, io.Reader, func(), error)
	// UserID returns the person's Devin user id; default reads `Bin auth
	// status`.
	UserID func(ctx context.Context) (string, error)
	// ReadWait bounds how long Push waits after the echo for the model's
	// first output; default 8 s.
	ReadWait time.Duration

	mu   sync.Mutex
	user string
}

// NewDevin returns the adapter for the devin executable at bin.
func NewDevin(bin string) *Devin { return &Devin{Bin: bin} }

func (d *Devin) Agent() string { return "devin" }

func (d *Devin) start(ctx context.Context) (*acpConn, error) {
	start := d.Start
	if start == nil {
		start = func(ctx context.Context) (io.WriteCloser, io.Reader, func(), error) {
			cmd := exec.CommandContext(ctx, d.Bin, "acp", "--cloud")
			in, err := cmd.StdinPipe()
			if err != nil {
				return nil, nil, nil, err
			}
			out, err := cmd.StdoutPipe()
			if err != nil {
				return nil, nil, nil, err
			}
			cmd.WaitDelay = 2 * time.Second
			if err := cmd.Start(); err != nil {
				return nil, nil, nil, err
			}
			return in, out, func() {
				_ = in.Close()
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}, nil
		}
	}
	w, r, stop, err := start(ctx)
	if err != nil {
		return nil, fmt.Errorf("vendorcloud: devin: start the cloud relay: %w", err)
	}
	c := newACPConn(w, r, stop)
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]string{"name": "flopwire", "version": "1"},
	}, nil); err != nil {
		c.close()
		return nil, fmt.Errorf("vendorcloud: devin: initialize: %w", err)
	}
	return c, nil
}

func (d *Devin) userID(ctx context.Context) (string, error) {
	d.mu.Lock()
	user := d.user
	d.mu.Unlock()
	if user != "" {
		return user, nil
	}
	if d.UserID != nil {
		u, err := d.UserID(ctx)
		if err != nil {
			return "", err
		}
		user = u
	} else {
		out, err := exec.CommandContext(ctx, d.Bin, "auth", "status").Output()
		if err != nil {
			return "", fmt.Errorf("vendorcloud: devin: auth status: %w", err)
		}
		user = parseDevinUserID(out)
	}
	if user == "" {
		return "", errors.New("vendorcloud: devin: no user id in `devin auth status`: log in with devin auth login")
	}
	d.mu.Lock()
	d.user = user
	d.mu.Unlock()
	return user, nil
}

// parseDevinUserID finds the "User ID:" line of `devin auth status`.
func parseDevinUserID(out []byte) string {
	for line := range strings.SplitSeq(string(out), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "User ID:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// devinSession is one entry of session/list, the fields used.
type devinSession struct {
	SessionID string         `json:"sessionId"`
	Title     string         `json:"title"`
	Meta      map[string]any `json:"_meta"`
}

func (s devinSession) meta(k string) string {
	v, _ := s.Meta["cognition.ai/"+k].(string)
	return v
}

// devinPages bounds the session/list pages List reads.
const devinPages = 10

// List returns the person's Devin cloud sessions that are not archived or
// exited.
func (d *Devin) List(ctx context.Context) ([]Session, error) {
	user, err := d.userID(ctx)
	if err != nil {
		return nil, err
	}
	c, err := d.start(ctx)
	if err != nil {
		return nil, err
	}
	defer c.close()
	var out []Session
	cursor := ""
	for range devinPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "session/list", params, nil)
		if err != nil {
			return nil, fmt.Errorf("vendorcloud: devin: session/list: %w", err)
		}
		var page struct {
			Sessions   []devinSession `json:"sessions"`
			NextCursor string         `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("vendorcloud: devin: session/list: %w", err)
		}
		for _, s := range page.Sessions {
			archived, _ := s.Meta["cognition.ai/isArchived"].(bool)
			if s.meta("creatorUserId") != user || archived || s.meta("sessionStatus") == "exited" {
				continue
			}
			out = append(out, Session{Agent: "devin", ID: s.SessionID, Title: s.Title, Running: s.meta("statusEnum") == "working"})
		}
		if page.NextCursor == "" || len(page.Sessions) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// Push sends text into the session as a prompt.
func (d *Devin) Push(ctx context.Context, id, text string) (Pushed, error) {
	c, err := d.start(ctx)
	if err != nil {
		return Pushed{}, err
	}
	defer c.close()
	echoed := make(chan struct{})
	read := make(chan time.Time, 1)
	var once, readOnce sync.Once
	notify := func(method string, params json.RawMessage) {
		if method != "session/update" {
			return
		}
		var u struct {
			SessionID string `json:"sessionId"`
			Update    struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(params, &u) != nil || u.SessionID != id {
			return
		}
		switch u.Update.Kind {
		case "user_message_chunk":
			// The echo of the prompt (its first chunk, should the relay
			// split it).
			if t := strings.TrimSpace(u.Update.Content.Text); t != "" && strings.HasPrefix(strings.TrimSpace(text), t) {
				once.Do(func() { close(echoed) })
			}
		case "agent_message_chunk", "agent_thought_chunk":
			select {
			case <-echoed:
				readOnce.Do(func() { read <- time.Now() })
			default:
			}
		}
	}
	answer := c.start(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": text}}}, notify)
	select {
	case <-echoed:
	case r := <-answer:
		if r.err != nil {
			if strings.Contains(strings.ToLower(r.err.Error()), "exited") || strings.Contains(strings.ToLower(r.err.Error()), "not found") {
				return Pushed{}, fmt.Errorf("%w: %v", ErrGone, r.err)
			}
			return Pushed{}, fmt.Errorf("vendorcloud: devin push: %w", r.err)
		}
		// The turn ended before the echo was seen: the session took it.
		return Pushed{ReadAt: time.Now()}, nil
	case <-ctx.Done():
		return Pushed{}, fmt.Errorf("vendorcloud: devin push: no echo from the session: %w", ctx.Err())
	}
	wait := d.ReadWait
	if wait <= 0 {
		wait = 8 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl)-time.Second)
	}
	if wait <= 0 {
		return Pushed{}, nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case at := <-read:
		return Pushed{ReadAt: at}, nil
	case r := <-answer:
		if r.err == nil {
			return Pushed{ReadAt: time.Now()}, nil
		}
	case <-t.C:
	case <-ctx.Done():
	}
	return Pushed{}, nil
}

// acpConn is a JSON-RPC 2.0 connection over newline-delimited JSON. A
// request from the agent (the relay asking the client for something) is
// answered "method not found": Flopwire offers no client capabilities.
type acpConn struct {
	w    io.WriteCloser
	stop func()
	wmu  sync.Mutex // serializes writes; never held with mu
	mu   sync.Mutex
	next int64
	wait map[int64]chan rpcResult
	subs map[int64]func(string, json.RawMessage)
	done chan struct{}
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (%d)", e.Message, e.Code) }

func newACPConn(w io.WriteCloser, r io.Reader, stop func()) *acpConn {
	c := &acpConn{w: w, stop: stop, wait: map[int64]chan rpcResult{}, subs: map[int64]func(string, json.RawMessage){}, done: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *acpConn) close() {
	if c.stop != nil {
		c.stop()
	} else {
		_ = c.w.Close()
	}
}

func (c *acpConn) read(r io.Reader) {
	defer close(c.done)
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			c.mu.Lock()
			for id, ch := range c.wait {
				ch <- rpcResult{err: errors.New("the relay closed")}
				delete(c.wait, id)
			}
			c.mu.Unlock()
			return
		}
	}
}

func (c *acpConn) dispatch(line []byte) {
	var m struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch {
	case m.Method != "" && m.ID != nil:
		// Not from the reading goroutine: a relay that writes again
		// before it reads its stdin would block both ends.
		go c.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": rpcError{Code: -32601, Message: "not supported"}})
	case m.Method != "":
		c.mu.Lock()
		subs := make([]func(string, json.RawMessage), 0, len(c.subs))
		for _, f := range c.subs {
			subs = append(subs, f)
		}
		c.mu.Unlock()
		for _, f := range subs {
			f(m.Method, m.Params)
		}
	case m.ID != nil:
		var id int64
		if json.Unmarshal(*m.ID, &id) != nil {
			return
		}
		c.mu.Lock()
		ch, ok := c.wait[id]
		delete(c.wait, id)
		c.mu.Unlock()
		if !ok {
			return
		}
		if m.Error != nil {
			ch <- rpcResult{err: m.Error}
		} else {
			ch <- rpcResult{result: m.Result}
		}
	}
}

func (c *acpConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(append(b, '\n'))
	return err
}

// start sends a request; its answer comes on the channel. notify, when
// set, sees every notification until the connection closes.
func (c *acpConn) start(ctx context.Context, method string, params any, notify func(string, json.RawMessage)) <-chan rpcResult {
	ch := make(chan rpcResult, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.wait[id] = ch
	if notify != nil {
		c.subs[id] = notify
	}
	c.mu.Unlock()
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.wait, id)
		c.mu.Unlock()
		ch <- rpcResult{err: err}
	}
	return ch
}

// call sends a request and waits for its answer.
func (c *acpConn) call(ctx context.Context, method string, params any, notify func(string, json.RawMessage)) (json.RawMessage, error) {
	select {
	case r := <-c.start(ctx, method, params, notify):
		return r.result, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
