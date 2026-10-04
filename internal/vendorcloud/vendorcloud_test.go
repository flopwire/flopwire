package vendorcloud

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The fixtures below are synthetic, in the shapes recorded in
// notes/message-bus/cloud-2026-10-03.md.

func TestClaudeListKeepsActiveCloudSessions(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		if r.URL.Path != "/v1/code/sessions" || r.URL.Query().Get("statuses") != "active" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("cursor") {
		case "":
			io.WriteString(w, `{"data":[
				{"id":"cse_01AAAA","title":"local bridge","status":"active","worker_status":"running","environment_kind":"bridge"},
				{"id":"cse_01BBBB","title":"cloud running","status":"active","worker_status":"running","environment_kind":"anthropic_cloud",
				 "config":{"sources":[{"type":"git_repository","url":"https://github.com/acme/api"}]},
				 "external_metadata":{"current_branches":{"acme/api":"claude/fix-x"}}}
			],"next_cursor":"c2"}`)
		case "c2":
			io.WriteString(w, `{"data":[
				{"id":"cse_01CCCC","title":"cloud idle","status":"active","worker_status":"idle","environment_kind":"anthropic_cloud","config":{"sources":[]}},
				{"id":"cse_01DDDD","title":"archived","status":"archived","worker_status":"idle","environment_kind":"anthropic_cloud"}
			],"next_cursor":""}`)
		}
	}))
	defer srv.Close()
	c := &Claude{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{
		{Agent: "claude", ID: "session_01BBBB", Title: "cloud running", Repo: "acme/api", Branch: "claude/fix-x", Running: true},
		{Agent: "claude", ID: "session_01CCCC", Title: "cloud idle"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
	if len(auth) != 2 || auth[0] != "Bearer tok" {
		t.Fatalf("auth headers = %q", auth)
	}
}

func TestClaudeListFailsOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"type":"error"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Claude{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("List error = %v, want the 401", err)
	}
}

func TestClaudePush(t *testing.T) {
	var gotArgs []string
	c := &Claude{Bin: "claude", Run: func(_ context.Context, bin string, args ...string) ([]byte, []byte, error) {
		gotArgs = args
		return []byte(`{"ok":true,"session_id":"session_01BBBB"}` + "\n"), nil, nil
	}}
	if _, err := c.Push(context.Background(), "session_01BBBB", "<flopwire-instructions>x"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"-p", "<flopwire-instructions>x", "--cloud", "session_01BBBB", "--output-format", "json"}; !slices.Equal(gotArgs, want) {
		t.Fatalf("args = %q, want %q", gotArgs, want)
	}

	// The session's own record decides whether it is gone, never the
	// CLI's text: a plugin hook's "not found" on stderr while the session
	// is listed running is a failed push.
	// The shapes as GET /v1/code/sessions/{id} answers live (2026-10-03):
	// the session under "response_shape"; a missing session a typed
	// not_found_error; an unknown route a plain-text 404, which is not a
	// sign the session is gone; a 5xx neither.
	body := map[string]string{
		"cse_01BBBB": `{"response_shape":{"id":"cse_01BBBB","status":"active","worker_status":"running"}}`,
		"cse_01ARCH": `{"response_shape":{"id":"cse_01ARCH","status":"archived","worker_status":"idle"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/code/sessions/")
		switch id {
		case "cse_01GONE":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"message":"Session cse_01GONE not found","resource_id":"cse_01GONE","resource_type":"session","type":"not_found_error"},"type":"error"}`)
		case "cse_01ROUTE":
			http.Error(w, "404 page not found", http.StatusNotFound)
		case "cse_01DOWN":
			http.Error(w, `{"type":"error","error":{"type":"api_error"}}`, http.StatusBadGateway)
		default:
			io.WriteString(w, body[id])
		}
	}))
	defer srv.Close()
	c.BaseURL, c.Token = srv.URL, func(context.Context) (string, error) { return "tok", nil }
	c.Run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("SessionEnd hook [node x.mjs] failed: module not found\n"), errors.New("exit status 1")
	}
	if _, err := c.Push(context.Background(), "session_01BBBB", "x"); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("hook stderr \"not found\" on a running session: err = %v, want a push failure", err)
	}
	c.Run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("Error: cloud session is archived\n"), errors.New("exit status 1")
	}
	for _, id := range []string{"session_01ARCH", "session_01GONE"} {
		if _, err := c.Push(context.Background(), id, "x"); !errors.Is(err, ErrGone) {
			t.Fatalf("%s: err = %v, want ErrGone", id, err)
		}
	}
	for _, id := range []string{"session_01ROUTE", "session_01DOWN"} {
		if _, err := c.Push(context.Background(), id, "x"); err == nil || errors.Is(err, ErrGone) {
			t.Fatalf("%s: err = %v, want a push failure, not ErrGone", id, err)
		}
	}
	c.Run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return nil, []byte("Error: network unreachable\n"), errors.New("exit status 1")
	}
	if _, err := c.Push(context.Background(), "session_01BBBB", "x"); err == nil || errors.Is(err, ErrGone) || !strings.Contains(err.Error(), "network unreachable") {
		t.Fatalf("failure: err = %v, want the CLI's reason and not ErrGone", err)
	}
	c.Run = func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte(`{"ok":false}`), nil, nil
	}
	if _, err := c.Push(context.Background(), "session_01BBBB", "x"); err == nil {
		t.Fatal(`{"ok":false} was taken as delivered`)
	}
}

// Seen reads the events route as it answers live: newest first, with
// sequence_num as a string.
func TestClaudeSeen(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 10, 3, 18, 0, s, 0, time.UTC) }
	ev := func(seq int, typ, content string) string {
		return fmt.Sprintf(`{"sequence_num":"%d","created_at":%q,"payload":{"type":%q,"message":{"role":"x","content":%s}}}`, seq, at(seq).Format(time.RFC3339Nano), typ, content)
	}
	pushed, _ := json.Marshal("<flopwire-instructions>\n...\n</flopwire-instructions>\n\n<flopwire-message id=\"m1\" from=\"s\">\nhi\n</flopwire-message>\n\n<flopwire-message id=\"m2\" from=\"s\">\nyo\n</flopwire-message>")
	events := []string{ // newest first, as the route returns them
		ev(9, "assistant", `[{"type":"text","text":"quoting <flopwire-message id=\"m3\" mid-line"}]`),
		ev(8, "user", `[{"type":"text","text":"x <flopwire-message id=\"m3\""}]`),
		ev(7, "assistant", `[{"type":"text","text":"ok"}]`),
		`{"sequence_num":"6","created_at":"2026-10-03T18:00:06Z","payload":{"type":"tool_progress"}}`,
		ev(5, "user", `[{"type":"tool_result","content":"done"}]`),
		ev(4, "user", string(pushed)),
		ev(3, "assistant", `[{"type":"tool_use"}]`),
		// Events without a usable sequence number are skipped, not fatal.
		`{"sequence_num":null,"created_at":"2026-10-03T18:00:02Z","payload":{"type":"assistant","message":{"role":"assistant","content":[]}}}`,
		`{"sequence_num":"x","created_at":"2026-10-03T18:00:01Z","payload":{"type":"system"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/code/sessions/cse_01BBBB/events" || r.URL.Query().Get("sort_order") != "desc" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"data":[`+strings.Join(events, ",")+`],"next_cursor":null}`)
	}))
	defer srv.Close()
	c := &Claude{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil }}
	got, err := c.Seen(context.Background(), "session_01BBBB", []string{"m1", "m2", "m3", "m4"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Time{"m1": at(7), "m2": at(7)}
	if len(got) != len(want) || !got["m1"].Equal(want["m1"]) || !got["m2"].Equal(want["m2"]) {
		t.Fatalf("Seen = %v, want %v (m3 is only quoted mid-line, m4 never pushed)", got, want)
	}
}

func TestRepoFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/api":     "acme/api",
		"https://github.com/acme/api.git": "acme/api",
		"https://github.com/":             "",
		"":                                "",
	} {
		if got := repoFromURL(in); got != want {
			t.Errorf("repoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeRelay is an in-process `devin acp --cloud`: it answers initialize,
// session/list from sessions, and session/prompt with the echo, an agent
// chunk, and (unless the session is exited) no answer until closed.
type fakeRelay struct {
	sessions []map[string]any
	exited   map[string]bool
	prompts  []string
	silent   bool // no echo
}

func (f *fakeRelay) start(context.Context) (io.WriteCloser, io.Reader, func(), error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		defer outW.Close()
		enc := json.NewEncoder(outW)
		sc := bufio.NewScanner(inR)
		for sc.Scan() {
			var m struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method == "" {
				continue
			}
			switch m.Method {
			case "initialize":
				enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"protocolVersion": 1}})
			case "session/list":
				enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{"sessions": f.sessions}})
			case "session/prompt":
				var p struct {
					SessionID string `json:"sessionId"`
					Prompt    []struct {
						Text string `json:"text"`
					} `json:"prompt"`
				}
				json.Unmarshal(m.Params, &p)
				if f.exited[p.SessionID] {
					enc.Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32000, "message": "Session already exited"}})
					continue
				}
				f.prompts = append(f.prompts, p.Prompt[0].Text)
				if f.silent {
					continue
				}
				// A request from the agent, which the client must answer.
				enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "session/request_permission", "params": map[string]any{}})
				up := func(kind, text string) {
					enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": p.SessionID,
						"update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}}})
				}
				up("agent_message_chunk", "before the echo: not a read")
				up("user_message_chunk", p.Prompt[0].Text)
				up("agent_thought_chunk", "Got it")
			}
		}
	}()
	return inW, outR, func() { inW.Close(); outR.Close() }, nil
}

func devinMeta(creator, status, statusEnum string, archived bool) map[string]any {
	return map[string]any{"cognition.ai/creatorUserId": creator, "cognition.ai/sessionStatus": status, "cognition.ai/statusEnum": statusEnum, "cognition.ai/isArchived": archived}
}

func TestDevinList(t *testing.T) {
	f := &fakeRelay{sessions: []map[string]any{
		{"sessionId": "devin-aaaa", "title": "mine working", "_meta": devinMeta("user-me", "running", "working", false)},
		{"sessionId": "devin-bbbb", "title": "mine waiting", "_meta": devinMeta("user-me", "running", "blocked", false)},
		{"sessionId": "devin-cccc", "title": "mine suspended", "_meta": devinMeta("user-me", "suspended", "finished", false)},
		{"sessionId": "devin-dddd", "title": "teammate's", "_meta": devinMeta("user-other", "running", "working", false)},
		{"sessionId": "devin-eeee", "title": "archived", "_meta": devinMeta("user-me", "suspended", "finished", true)},
		{"sessionId": "devin-ffff", "title": "exited", "_meta": devinMeta("user-me", "exited", "finished", false)},
	}}
	d := &Devin{Start: f.start, UserID: func(context.Context) (string, error) { return "user-me", nil }}
	got, err := d.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Session{
		{Agent: "devin", ID: "devin-aaaa", Title: "mine working", Running: true},
		{Agent: "devin", ID: "devin-bbbb", Title: "mine waiting"},
		{Agent: "devin", ID: "devin-cccc", Title: "mine suspended"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("List = %+v, want %+v", got, want)
	}
}

func TestDevinPush(t *testing.T) {
	f := &fakeRelay{exited: map[string]bool{"devin-gone": true}}
	d := &Devin{Start: f.start, ReadWait: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := d.Push(ctx, "devin-aaaa", "hello\nthere")
	if err != nil {
		t.Fatal(err)
	}
	if p.ReadAt.IsZero() {
		t.Fatal("the agent chunk after the echo was not taken as read")
	}
	if !slices.Equal(f.prompts, []string{"hello\nthere"}) {
		t.Fatalf("prompts = %q", f.prompts)
	}
	if _, err := d.Push(ctx, "devin-gone", "x"); !errors.Is(err, ErrGone) {
		t.Fatalf("exited: err = %v, want ErrGone", err)
	}
	f.silent = true
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := d.Push(short, "devin-aaaa", "x"); err == nil {
		t.Fatal("a push with no echo was taken as delivered")
	}
}

func TestParseDevinUserID(t *testing.T) {
	out := []byte("Logged in (via Devin).\n\nUser:\n  Name:              A B\n  User ID:           user-123abc\n\nAccount:\n")
	if got := parseDevinUserID(out); got != "user-123abc" {
		t.Fatalf("parseDevinUserID = %q", got)
	}
}

func TestWrapperIDs(t *testing.T) {
	got := wrapperIDs("<flopwire-message id=\"m1\" x>\nbody <flopwire-message id=\"m9\"\n<flopwire-message id=\"m2\">")
	if !slices.Equal(got, []string{"m1", "m2"}) {
		t.Fatalf("wrapperIDs = %q", got)
	}
}
