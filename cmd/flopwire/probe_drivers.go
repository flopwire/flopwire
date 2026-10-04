package main

// Headless harness sessions for `flopwire probe` (probe.go). Each runs one
// long-lived harness process, so the session stays live and idle between
// turns: a one-shot run (`codex exec`, `devin -p`) ends its session when it
// exits, and the bus then marks its queued messages undelivered, so the
// idle case and a message queued before a prompt could not be tested.
//
//   - Claude Code: `claude -p --input-format stream-json` (one JSON user
//     message per turn on stdin, `result` events on stdout).
//   - Codex: `codex app-server` (JSON-RPC on stdio: thread/start,
//     turn/start, turn/completed).
//   - Devin CLI: `devin acp` (Agent Client Protocol on stdio:
//     session/new, session/prompt).
//   - opencode: `opencode serve` (HTTP: POST /session, POST
//     /session/{id}/message).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// probeSession is one live headless harness session.
type probeSession interface {
	ID() string
	// Turn sends a prompt and waits until the turn ends; it returns the
	// model's final reply.
	Turn(ctx context.Context, prompt string) (string, error)
	// TurnAt is when the harness last showed a turn running (unix ms):
	// the idle case checks that none starts without a prompt.
	TurnAt() int64
	Close()
}

// probeProc is a harness process with its stdin, stdout and a log of its
// stderr.
type probeProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	errLog *os.File
	done   chan struct{}
	err    error
}

func startProc(ctx context.Context, dir string, env []string, errLog string, name string, args ...string) (*probeProc, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env = dir, env
	f, err := os.OpenFile(errLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd.Stderr = f
	in, err := cmd.StdinPipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	p := &probeProc{cmd: cmd, stdin: in, stdout: out, errLog: f, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		f.Close()
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.kill()
		case <-p.done:
		}
	}()
	return p, nil
}

// stop closes stdin, which ends a headless session cleanly, and kills the
// process if it has not exited within grace.
func (p *probeProc) stop(grace time.Duration) {
	p.stdin.Close()
	select {
	case <-p.done:
	case <-time.After(grace):
		p.kill()
		<-p.done
	}
}

func (p *probeProc) kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

// lines calls fn for every stdout line, until EOF.
func (p *probeProc) lines(fn func([]byte)) {
	sc := bufio.NewScanner(p.stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}

// --- Claude Code ---

// claudeEvent is the part of a stream-json line the probe reads.
type claudeEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
}

func parseClaudeLine(b []byte) (claudeEvent, bool) {
	var e claudeEvent
	if json.Unmarshal(b, &e) != nil || e.Type == "" {
		return e, false
	}
	return e, true
}

type claudeSession struct {
	p       *probeProc
	mu      sync.Mutex
	id      string
	results chan claudeEvent
	turnAt  atomic.Int64
}

func startClaude(ctx context.Context, dir string, env []string, errLog, model string) (*claudeSession, error) {
	p, err := startProc(ctx, dir, env, errLog, "claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--setting-sources", "project", "--model", model, "--allowedTools", "Bash,Agent,Task")
	if err != nil {
		return nil, err
	}
	s := &claudeSession{p: p, results: make(chan claudeEvent, 4)}
	go func() {
		p.lines(func(b []byte) {
			e, ok := parseClaudeLine(b)
			if !ok {
				return
			}
			s.mu.Lock()
			if e.SessionID != "" && s.id == "" {
				s.id = e.SessionID
			}
			s.mu.Unlock()
			switch e.Type {
			case "assistant", "user", "result":
				s.turnAt.Store(time.Now().UnixMilli())
			}
			if e.Type == "result" {
				s.results <- e
			}
		})
		close(s.results)
	}()
	return s, nil
}

func (s *claudeSession) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

func (s *claudeSession) TurnAt() int64 { return s.turnAt.Load() }

func (s *claudeSession) Turn(ctx context.Context, prompt string) (string, error) {
	for drained := false; !drained; { // a result of a turn nobody asked for
		select {
		case _, ok := <-s.results:
			drained = !ok
		default:
			drained = true
		}
	}
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": prompt}})
	if _, err := s.p.stdin.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	select {
	case e, ok := <-s.results:
		if !ok {
			return "", fmt.Errorf("claude exited: %v", s.p.err)
		}
		if e.IsError {
			return e.Result, fmt.Errorf("claude: turn ended with an error (%s): %s", e.Subtype, clip(e.Result, 200))
		}
		return e.Result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *claudeSession) Close() { s.p.stop(10 * time.Second) }

// --- JSON-RPC over stdio (Codex app-server, Devin ACP) ---

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// rpcConn is a JSON-RPC 2.0 client on a process's stdio. Notifications go
// to onNote; requests from the server go to onRequest, whose answer is
// sent back.
type rpcConn struct {
	p         *probeProc
	wmu       sync.Mutex
	mu        sync.Mutex
	next      int
	pending   map[string]chan rpcMsg
	onNote    func(rpcMsg)
	onRequest func(rpcMsg) any
	closed    chan struct{}
}

func newRPC(p *probeProc, onNote func(rpcMsg), onRequest func(rpcMsg) any) *rpcConn {
	c := &rpcConn{p: p, pending: map[string]chan rpcMsg{}, onNote: onNote, onRequest: onRequest, closed: make(chan struct{})}
	go func() {
		p.lines(func(b []byte) {
			var m rpcMsg
			if json.Unmarshal(b, &m) != nil {
				return
			}
			switch {
			case m.Method != "" && len(m.ID) > 0:
				res := c.onRequest(m)
				c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": res})
			case m.Method != "":
				c.onNote(m)
			case len(m.ID) > 0:
				c.mu.Lock()
				ch := c.pending[string(m.ID)]
				delete(c.pending, string(m.ID))
				c.mu.Unlock()
				if ch != nil {
					ch <- m
				}
			}
		})
		close(c.closed)
	}()
	return c
}

func (c *rpcConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.p.stdin.Write(append(b, '\n'))
	return err
}

func (c *rpcConn) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and decodes its result into out (nil: ignored).
func (c *rpcConn) call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id := fmt.Sprint(c.next)
	ch := make(chan rpcMsg, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": c.next, "method": method, "params": params}); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return fmt.Errorf("%s: %s (%d)", method, m.Error.Message, m.Error.Code)
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.closed:
		return fmt.Errorf("%s: the process exited (%v)", method, c.p.err)
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// --- Codex ---

// codexReview is an auto-review ("guardian") pass the app-server reported.
type codexReview struct {
	Start, End int64 // unix ms
	Status     string
}

type codexSession struct {
	rpc    *rpcConn
	p      *probeProc
	id     string
	turnAt atomic.Int64

	mu      sync.Mutex
	replies []string      // agent messages of the running turn
	done    chan string   // turn/completed: the turn's status
	reviews []codexReview // auto-review passes, in order
	onNote  func(rpcMsg)  // extra observer (the guardian case)
}

// codexNote is the part of an app-server notification the probe reads.
type codexNote struct {
	ThreadID    string `json:"threadId"`
	StartedAtMs int64  `json:"startedAtMs"`
	DoneAtMs    int64  `json:"completedAtMs"`
	Item        *struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Phase string `json:"phase"`
	} `json:"item"`
	Turn *struct {
		Status string `json:"status"`
	} `json:"turn"`
	Review *struct {
		Status string `json:"status"`
	} `json:"review"`
}

// codexStartOpts are the thread's approval settings.
type codexStartOpts struct {
	Model, ApprovalPolicy, Reviewer, Sandbox string
}

// startCodex starts `codex app-server` in CODEX_HOME (env), trusts the
// project's hooks in that home's config, and starts a thread.
func startCodex(ctx context.Context, dir, home string, env []string, errLog string, o codexStartOpts) (*codexSession, error) {
	p, err := startProc(ctx, dir, env, errLog, "codex", "app-server")
	if err != nil {
		return nil, err
	}
	s := &codexSession{p: p}
	s.rpc = newRPC(p, s.note, func(m rpcMsg) any {
		// Approvals reach the client only when no reviewer takes them:
		// the probe declines, so nothing runs unreviewed.
		return map[string]any{"decision": "decline"}
	})
	fail := func(err error) (*codexSession, error) { p.stop(5 * time.Second); return nil, err }
	if err := s.rpc.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{"name": "flopwire-probe", "version": version},
		"capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
		return fail(err)
	}
	if err := s.rpc.notify("initialized", nil); err != nil {
		return fail(err)
	}
	if err := codexTrustHooks(ctx, s.rpc, dir, home); err != nil {
		return fail(err)
	}
	params := map[string]any{"cwd": dir, "model": o.Model, "approvalPolicy": o.ApprovalPolicy, "sandbox": o.Sandbox}
	if o.Reviewer != "" {
		params["approvalsReviewer"] = o.Reviewer
	}
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.rpc.call(ctx, "thread/start", params, &r); err != nil {
		return fail(err)
	}
	s.id = r.Thread.ID
	return s, nil
}

// codexTrustHooks marks the project's hooks trusted in the probe's own
// CODEX_HOME config, as the TUI's review prompt does: hooks/list gives
// each hook's key and hash, config/batchWrite records them.
func codexTrustHooks(ctx context.Context, c *rpcConn, dir, home string) error {
	var list struct {
		Data []struct {
			Hooks []struct {
				Key, CurrentHash, TrustStatus, Source string
			} `json:"hooks"`
			Errors []json.RawMessage `json:"errors"`
		} `json:"data"`
	}
	if err := c.call(ctx, "hooks/list", map[string]any{"cwds": []string{dir}}, &list); err != nil {
		return err
	}
	var edits []map[string]any
	found := false
	for _, d := range list.Data {
		if len(d.Errors) > 0 {
			return fmt.Errorf("codex hooks: %s", d.Errors[0])
		}
		for _, h := range d.Hooks {
			found = found || h.Source == "project"
			if h.Source == "project" && h.TrustStatus != "trusted" {
				k, _ := json.Marshal(h.Key)
				edits = append(edits, map[string]any{"keyPath": "hooks.state." + string(k) + ".trusted_hash", "value": h.CurrentHash, "mergeStrategy": "upsert"})
			}
		}
	}
	switch {
	case !found:
		return errors.New("codex lists no project hooks for the probe project")
	case len(edits) == 0:
		return nil // trusted by an earlier session of this run
	}
	return c.call(ctx, "config/batchWrite", map[string]any{"edits": edits, "filePath": home + "/config.toml", "reloadUserConfig": true}, nil)
}

func (s *codexSession) note(m rpcMsg) {
	var n codexNote
	_ = json.Unmarshal(m.Params, &n)
	s.mu.Lock()
	extra := s.onNote
	if n.ThreadID != "" && s.id != "" && n.ThreadID != s.id {
		s.mu.Unlock()
		return // a subagent's thread
	}
	switch m.Method {
	case "turn/started":
		s.turnAt.Store(time.Now().UnixMilli())
	case "item/completed":
		if n.Item != nil && n.Item.Type == "agentMessage" {
			s.turnAt.Store(time.Now().UnixMilli())
			s.replies = append(s.replies, n.Item.Text)
		}
	case "item/autoApprovalReview/started":
		s.reviews = append(s.reviews, codexReview{Start: n.StartedAtMs, Status: "inProgress"})
	case "item/autoApprovalReview/completed":
		if k := len(s.reviews); k > 0 {
			s.reviews[k-1].End = n.DoneAtMs
			if n.Review != nil {
				s.reviews[k-1].Status = n.Review.Status
			}
		}
	case "turn/completed":
		s.turnAt.Store(time.Now().UnixMilli())
		if s.done != nil {
			st := ""
			if n.Turn != nil {
				st = n.Turn.Status
			}
			s.done <- st
			s.done = nil
		}
	}
	s.mu.Unlock()
	if extra != nil {
		extra(m)
	}
}

func (s *codexSession) ID() string    { return s.id }
func (s *codexSession) TurnAt() int64 { return s.turnAt.Load() }

func (s *codexSession) Turn(ctx context.Context, prompt string) (string, error) {
	done := make(chan string, 1)
	s.mu.Lock()
	s.replies, s.done = nil, done
	s.mu.Unlock()
	if err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": s.id,
		"input": []map[string]any{{"type": "text", "text": prompt}}}, nil); err != nil {
		return "", err
	}
	select {
	case st := <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		reply := ""
		if k := len(s.replies); k > 0 {
			reply = s.replies[k-1]
		}
		if st != "completed" {
			return reply, fmt.Errorf("codex: turn %s", st)
		}
		return reply, nil
	case <-s.rpc.closed:
		return "", fmt.Errorf("codex app-server exited: %v", s.p.err)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Reviews returns the auto-review passes seen so far.
func (s *codexSession) Reviews() []codexReview {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]codexReview(nil), s.reviews...)
}

func (s *codexSession) setObserver(fn func(rpcMsg)) {
	s.mu.Lock()
	s.onNote = fn
	s.mu.Unlock()
}

func (s *codexSession) Close() { s.p.stop(10 * time.Second) }

// --- Devin CLI ---

// devinUpdate is the part of an ACP session/update the probe reads.
type devinUpdate struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind    string `json:"sessionUpdate"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"update"`
}

type devinSession struct {
	rpc    *rpcConn
	p      *probeProc
	id     string
	turnAt atomic.Int64
	mu     sync.Mutex
	text   strings.Builder
}

func startDevin(ctx context.Context, dir string, env []string, errLog, model string) (*devinSession, error) {
	p, err := startProc(ctx, dir, env, errLog, "devin", "acp", "--model", model)
	if err != nil {
		return nil, err
	}
	s := &devinSession{p: p}
	s.rpc = newRPC(p, s.note, devinPermission)
	fail := func(err error) (*devinSession, error) { p.stop(5 * time.Second); return nil, err }
	if err := s.rpc.call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); err != nil {
		return fail(err)
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.rpc.call(ctx, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}}, &r); err != nil {
		return fail(err)
	}
	s.id = r.SessionID
	return s, nil
}

// devinPermission allows a tool call once: the probe's prompts run only
// sleep and echo in a scratch directory.
func devinPermission(m rpcMsg) any {
	if m.Method != "session/request_permission" {
		return map[string]any{}
	}
	var p struct {
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(m.Params, &p)
	for _, o := range p.Options {
		if strings.HasPrefix(o.Kind, "allow") {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

func (s *devinSession) note(m rpcMsg) {
	if m.Method != "session/update" {
		return
	}
	var u devinUpdate
	if json.Unmarshal(m.Params, &u) != nil || (u.SessionID != "" && u.SessionID != s.id) {
		return
	}
	switch u.Update.Kind {
	case "agent_message_chunk":
		s.mu.Lock()
		s.text.WriteString(u.Update.Content.Text)
		s.mu.Unlock()
		s.turnAt.Store(time.Now().UnixMilli())
	case "tool_call", "tool_call_update", "agent_thought_chunk", "user_message_chunk":
		s.turnAt.Store(time.Now().UnixMilli())
	}
}

func (s *devinSession) ID() string    { return s.id }
func (s *devinSession) TurnAt() int64 { return s.turnAt.Load() }

func (s *devinSession) Turn(ctx context.Context, prompt string) (string, error) {
	s.mu.Lock()
	s.text.Reset()
	s.mu.Unlock()
	var r struct {
		StopReason string `json:"stopReason"`
	}
	err := s.rpc.call(ctx, "session/prompt", map[string]any{"sessionId": s.id, "prompt": []map[string]any{{"type": "text", "text": prompt}}}, &r)
	s.mu.Lock()
	reply := s.text.String()
	s.mu.Unlock()
	if err == nil && r.StopReason != "end_turn" {
		err = fmt.Errorf("devin: turn stopped: %s", r.StopReason)
	}
	return reply, err
}

func (s *devinSession) Close() { s.p.stop(10 * time.Second) }

// --- opencode ---

// opencodeSession is one `opencode serve` process holding one session,
// driven over its HTTP API. A turn is POST /session/{id}/message, which
// answers when the session goes idle; /session/status shows a turn
// running.
type opencodeSession struct {
	p      *probeProc
	base   string
	id     string
	model  map[string]string
	turnAt atomic.Int64
	stop   chan struct{}
	tap    string // the probe's hook log
}

var opencodeListening = regexp.MustCompile(`listening on (http://[0-9.:a-z]+)`)

func startOpencode(ctx context.Context, dir string, env []string, errLog, model, tap string) (*opencodeSession, error) {
	provider, modelID, ok := strings.Cut(model, "/")
	if !ok {
		return nil, fmt.Errorf("opencode: model %q is not provider/model", model)
	}
	p, err := startProc(ctx, dir, env, errLog, "opencode", "serve", "--port", "0", "--hostname", "127.0.0.1")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*opencodeSession, error) { p.kill(); <-p.done; return nil, err }
	urls := make(chan string, 1)
	go p.lines(func(b []byte) {
		if m := opencodeListening.FindSubmatch(b); m != nil {
			select {
			case urls <- string(m[1]):
			default:
			}
		}
	})
	s := &opencodeSession{p: p, model: map[string]string{"providerID": provider, "modelID": modelID}, stop: make(chan struct{}), tap: tap}
	select {
	case s.base = <-urls:
	case <-p.done:
		return fail(errors.New("opencode serve exited before it listened"))
	case <-time.After(60 * time.Second):
		return fail(errors.New("opencode serve did not listen within 60s"))
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := s.do(ctx, "POST", "/session", map[string]any{}, &r); err != nil {
		return fail(err)
	}
	s.id = r.ID
	go s.watch()
	return s, nil
}

func (s *opencodeSession) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("opencode %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("opencode %s %s: %s: %s", method, path, resp.Status, clip(string(b), 300))
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// watch records when /session/status shows the session busy.
func (s *opencodeSession) watch() {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.p.done:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var st map[string]struct {
			Type string `json:"type"`
		}
		if s.do(ctx, "GET", "/session/status", nil, &st) == nil && st[s.id].Type != "" && st[s.id].Type != "idle" {
			s.turnAt.Store(time.Now().UnixMilli())
		}
		cancel()
	}
}

func (s *opencodeSession) ID() string    { return s.id }
func (s *opencodeSession) TurnAt() int64 { return s.turnAt.Load() }

// Turn sends the prompt and returns the text of every reply the turn
// wrote: a message delivered during the turn earns a reply of its own
// (docs/opencode.md), and the probe reads them all.
func (s *opencodeSession) Turn(ctx context.Context, prompt string) (string, error) {
	start := time.Now().UnixMilli()
	s.turnAt.Store(start)
	body := map[string]any{"model": s.model, "parts": []map[string]any{{"type": "text", "text": prompt}}}
	if err := s.do(ctx, "POST", "/session/"+s.id+"/message", body, nil); err != nil {
		return "", err
	}
	s.turnAt.Store(time.Now().UnixMilli())
	// The answer can come before the plugin has handled the session's
	// idle event: wait for its Stop hook, so a case never sees it late.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && ctx.Err() == nil; time.Sleep(100 * time.Millisecond) {
		all, _ := readTap(s.tap)
		if slices.ContainsFunc(all, func(e tapEntry) bool { return e.Event == "Stop" && e.Session == s.id && e.At >= start }) {
			break
		}
	}
	var msgs []struct {
		Info struct {
			Role string `json:"role"`
			Time struct {
				Created int64 `json:"created"`
			} `json:"time"`
			Error json.RawMessage `json:"error"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := s.do(ctx, "GET", "/session/"+s.id+"/message", nil, &msgs); err != nil {
		return "", err
	}
	var b strings.Builder
	var turnErr error
	for _, m := range msgs {
		if m.Info.Role != "assistant" || m.Info.Time.Created < start {
			continue
		}
		if len(m.Info.Error) > 0 && string(m.Info.Error) != "null" {
			turnErr = fmt.Errorf("opencode: the turn ended with %s", clip(string(m.Info.Error), 300))
		}
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				b.WriteString(p.Text + "\n")
			}
		}
	}
	return b.String(), turnErr
}

func (s *opencodeSession) Close() {
	close(s.stop)
	if s.p.cmd.Process != nil {
		_ = s.p.cmd.Process.Signal(syscall.SIGTERM)
	}
	s.p.stop(10 * time.Second)
}
