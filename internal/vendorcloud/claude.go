package vendorcloud

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Claude reaches Claude Code cloud sessions (claude.ai/code) with the
// device's Claude login.
//
// List reads the session list the Claude Code CLI itself reads for
// `--teleport`: GET /v1/code/sessions?statuses=active (undocumented). It
// lists every active session of the account, local Remote Control
// sessions too (environment_kind "bridge"); only those that run in
// Anthropic's cloud (environment_kind "anthropic_cloud") are kept. The
// list names a session "cse_" plus a suffix; the CLI and claude.ai name it
// "session_" plus the same suffix, the form List returns.
//
// Push runs `claude -p TEXT --cloud ID --output-format json`, which posts
// the text into the session as a user message ({"ok":true}). A session
// running a turn takes it at its next tool call; an idle one starts a turn
// (the caller pushes only while one runs).
//
// Seen reads the session's events (GET /v1/code/sessions/{id}/events): a
// pushed message is recorded as a user event when it is posted, and the
// first assistant event after it is the first model call with it in
// context.
type Claude struct {
	// Bin is the claude executable.
	Bin string
	// BaseURL is the API origin; default https://api.anthropic.com.
	BaseURL string
	// Token returns the login's OAuth access token; default: the Claude
	// Code credential store (the macOS keychain, else
	// ~/.claude/.credentials.json).
	Token func(ctx context.Context) (string, error)
	// HTTP is the client for the API; default a client with a 20 s timeout.
	HTTP *http.Client
	// Run runs the CLI and returns its stdout; default exec. Tests replace it.
	Run func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)
}

// NewClaude returns the adapter for the claude executable at bin.
func NewClaude(bin string) *Claude { return &Claude{Bin: bin} }

func (c *Claude) Agent() string { return "claude" }

func (c *Claude) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "https://api.anthropic.com"
}

func (c *Claude) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// claudeSession is one entry of GET /v1/code/sessions, the fields used.
type claudeSession struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Status          string `json:"status"`        // active, archived
	WorkerStatus    string `json:"worker_status"` // running, idle, requires_action
	EnvironmentKind string `json:"environment_kind"`
	Config          struct {
		Sources []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"sources"`
	} `json:"config"`
	ExternalMetadata struct {
		CurrentBranches map[string]string `json:"current_branches"`
	} `json:"external_metadata"`
}

type claudeList struct {
	Data       []claudeSession `json:"data"`
	NextCursor string          `json:"next_cursor"`
}

// claudePages bounds the pages List reads (100 sessions each).
const claudePages = 10

// List returns the account's active Anthropic cloud sessions.
func (c *Claude) List(ctx context.Context) ([]Session, error) {
	var out []Session
	cursor := ""
	for range claudePages {
		q := url.Values{"statuses": {"active"}, "limit": {"100"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page claudeList
		if err := c.get(ctx, "/v1/code/sessions?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, s := range page.Data {
			if s.EnvironmentKind != "anthropic_cloud" || s.Status != "active" {
				continue
			}
			v := Session{Agent: "claude", ID: claudePublicID(s.ID), Title: s.Title, Running: s.WorkerStatus == "running"}
			for _, src := range s.Config.Sources {
				if src.Type == "git_repository" {
					v.Repo = repoFromURL(src.URL)
					break
				}
			}
			if v.Repo != "" {
				v.Branch = s.ExternalMetadata.CurrentBranches[v.Repo]
			}
			out = append(out, v)
		}
		if page.NextCursor == "" || len(page.Data) == 0 {
			return out, nil
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// claudePublicID is the session_ form of a cse_ id.
func claudePublicID(id string) string {
	if rest, ok := strings.CutPrefix(id, "cse_"); ok {
		return "session_" + rest
	}
	return id
}

// claudeListID is the cse_ form the /v1/code routes take.
func claudeListID(id string) string {
	if rest, ok := strings.CutPrefix(id, "session_"); ok {
		return "cse_" + rest
	}
	return id
}

// repoFromURL is "owner/name" of a git hosting URL.
func repoFromURL(u string) string {
	p, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(u), ".git"))
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(p.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func (c *Claude) get(ctx context.Context, path string, into any) error {
	token, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("vendorcloud: claude: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("vendorcloud: claude: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vendorcloud: claude: GET %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("vendorcloud: claude: GET %s: %w", strings.SplitN(path, "?", 2)[0], err)
	}
	return nil
}

func (c *Claude) token(ctx context.Context) (string, error) {
	if c.Token != nil {
		return c.Token(ctx)
	}
	return claudeToken(ctx)
}

// claudeToken reads the Claude Code login: the macOS keychain item Claude
// Code keeps it in, else its credentials file. An expired token is an
// error: Claude Code refreshes it the next time it runs; Flopwire never
// does (a refresh would rotate the login's refresh token under Claude
// Code).
func claudeToken(ctx context.Context) (string, error) {
	var raw []byte
	if runtime.GOOS == "darwin" {
		out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
		if err == nil {
			raw = out
		}
	}
	if raw == nil {
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".claude")
		}
		b, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
		if err != nil {
			return "", fmt.Errorf("vendorcloud: claude: no Claude login found (run claude and /login): %w", err)
		}
		raw = b
	}
	var cred struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &cred); err != nil || cred.OAuth.AccessToken == "" {
		return "", errors.New("vendorcloud: claude: the Claude login holds no claude.ai token (cloud sessions need a claude.ai login)")
	}
	if cred.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() >= cred.OAuth.ExpiresAt {
		return "", errors.New("vendorcloud: claude: the Claude login's token expired; running claude refreshes it")
	}
	return cred.OAuth.AccessToken, nil
}

func (c *Claude) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if c.Run != nil {
		return c.Run(ctx, c.Bin, args...)
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	// Not the agent's own directory: the CLI reads project settings from
	// its working directory.
	cmd.Dir = os.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Push posts text into the session with the Claude Code CLI.
func (c *Claude) Push(ctx context.Context, id, text string) (Pushed, error) {
	stdout, stderr, err := c.run(ctx, "-p", text, "--cloud", id, "--output-format", "json")
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	jerr := json.Unmarshal(bytes.TrimSpace(stdout), &res)
	if err == nil && jerr == nil && res.OK {
		return Pushed{}, nil
	}
	msg := strings.TrimSpace(string(stderr))
	if res.Error != "" {
		msg = res.Error
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "archived") || strings.Contains(low, "not found") {
		return Pushed{}, fmt.Errorf("%w: %s", ErrGone, msg)
	}
	if ctx.Err() != nil {
		return Pushed{}, fmt.Errorf("vendorcloud: claude push: %w", ctx.Err())
	}
	if msg == "" && err != nil {
		msg = err.Error()
	}
	if msg == "" {
		msg = "no confirmation in the CLI's output"
	}
	return Pushed{}, fmt.Errorf("vendorcloud: claude push: %s", msg)
}

// seqNum is a sequence number the route writes as a JSON string ("92");
// a number is taken too.
type seqNum int64

func (n *seqNum) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	if err != nil {
		return fmt.Errorf("sequence_num %s: %w", b, err)
	}
	*n = seqNum(v)
	return nil
}

// claudeEvent is one entry of the session events, the fields used.
type claudeEvent struct {
	Seq       seqNum    `json:"sequence_num"`
	CreatedAt time.Time `json:"created_at"`
	Payload   struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"payload"`
}

// text is a user event's text: a string, or its text blocks.
func (e claudeEvent) text() string {
	raw := e.Payload.Message.Content
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, x := range blocks {
		if x.Type == "text" {
			b.WriteString(x.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Seen reads the session's latest events (the newest 200) for the pushed
// messages' wrappers.
func (c *Claude) Seen(ctx context.Context, session string, ids []string) (map[string]time.Time, error) {
	var page struct {
		Data []claudeEvent `json:"data"`
	}
	if err := c.get(ctx, "/v1/code/sessions/"+url.PathEscape(claudeListID(session))+"/events?limit=200&sort_order=desc", &page); err != nil {
		return nil, err
	}
	return seenIn(page.Data, ids), nil
}

// seenIn finds, in events (any order), the user events that carry the
// wrappers of ids and the first assistant event after each.
func seenIn(events []claudeEvent, ids []string) map[string]time.Time {
	asc := slices.Clone(events)
	slices.SortFunc(asc, func(a, b claudeEvent) int { return cmp.Compare(a.Seq, b.Seq) })
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	out := map[string]time.Time{}
	var waiting []string
	for _, e := range asc {
		switch e.Payload.Type {
		case "user":
			for _, id := range wrapperIDs(e.text()) {
				if want[id] {
					if _, done := out[id]; !done {
						waiting = append(waiting, id)
					}
				}
			}
		case "assistant":
			for _, id := range waiting {
				if _, done := out[id]; !done {
					out[id] = e.CreatedAt
				}
			}
			waiting = nil
		}
	}
	return out
}
