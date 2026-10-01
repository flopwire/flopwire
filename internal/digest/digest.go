// Package digest is a conversation's digest: a small, deterministic
// summary computed when the conversation is indexed (locally and on the
// server) and stored with it, so sessions can say what each session did
// without a read per session.
//
// Two kinds of fields make it up:
//
//   - Fold reads the messages a batch writes, in order: the intent (first
//     user prompt), the last assistant message, files edited, PRs, commits
//     and issues. Re-folding a message is harmless: these are sets, or keep
//     the lowest (intent) or highest (last) ordinal seen.
//   - Set takes what the store counts over the conversation's live rows
//     (messages by kind, tool calls by tool, failed calls, subagents,
//     tokens) and the conversation row (cwd, repo, branches, duration), so
//     a re-parse never double counts.
//
// The stored form is Digest's JSON. docs/search.md documents the fields.
package digest

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Caps keep a digest to a few hundred bytes typically, a few KB at most.
const (
	IntentLen = 120
	LastLen   = 160
	maxFiles  = 25
	maxPRs    = 10
	maxCommit = 20
	maxIssues = 10
	maxRepos  = 4
	// maxPending bounds the tool calls awaiting their result (a gh pr or
	// git commit whose output names the PR or commit).
	maxPending = 16
)

// Tokens is summed API token usage, where the harness records it (Claude:
// each API message once, with its final usage).
type Tokens struct {
	Input         int64 `json:"input,omitempty"`
	Output        int64 `json:"output,omitempty"`
	CacheRead     int64 `json:"cache_read,omitempty"`
	CacheCreation int64 `json:"cache_creation,omitempty"`
}

// Digest is a conversation's summary. Paths in FilesEdited are relative to
// the repo root (or cwd) when under it.
type Digest struct {
	Intent      string         `json:"intent,omitempty"`
	Repos       []string       `json:"repos,omitempty"`
	Branches    []string       `json:"branches,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	DurationS   int64          `json:"duration_s,omitempty"`
	Messages    map[string]int `json:"messages,omitempty"`
	Subagents   int            `json:"subagents,omitempty"`
	FilesEdited []string       `json:"files_edited,omitempty"`
	// FilesMore is set when more files were edited than FilesEdited keeps.
	FilesMore bool           `json:"files_more,omitempty"`
	Commands  int            `json:"commands,omitempty"`
	Tools     map[string]int `json:"tools,omitempty"`
	PRs       []string       `json:"prs,omitempty"`
	Commits   []string       `json:"commits,omitempty"`
	Issues    []string       `json:"issues,omitempty"`
	// More names the lists that hit their cap: "prs", "commits",
	// "issues" (the lists keep the first ones seen).
	More   []string `json:"more,omitempty"`
	Failed int      `json:"failed,omitempty"`
	Last   string   `json:"last,omitempty"`
	Tokens *Tokens  `json:"tokens,omitempty"`

	// State is the fold's bookkeeping; renderers ignore it.
	State *State `json:"state,omitempty"`
}

// State is what Fold keeps between batches.
type State struct {
	// First is the first user prompt, Long the first that is no weak
	// prompt (see weak), with their ordinals; Update picks the intent.
	First    string `json:"f,omitempty"`
	FirstOrd *int64 `json:"fo,omitempty"`
	Long     string `json:"g,omitempty"`
	LongOrd  *int64 `json:"go,omitempty"`
	LastOrd  *int64 `json:"lo,omitempty"`
	// Pending maps a tool call id to what its output may name: "pr",
	// "commit", "issue" (comma-joined).
	Pending map[string]string `json:"p,omitempty"`
	// Usage is the last API message whose usage the token counts hold
	// (see Append).
	Usage *UsageMark `json:"u,omitempty"`
}

// UsageMark names an API message (Claude's message.id; "" when the row
// has none) and the usage counted for it: the usage of its line with the
// most output tokens so far.
type UsageMark struct {
	Msg    string `json:"m,omitempty"`
	Tokens Tokens `json:"t"`
}

// Conv is what the digest takes from the conversation row.
type Conv struct {
	Title    string // the harness's title, if any
	Cwd      string
	RepoRoot string
	Remote   string // origin, normalized (host/owner/name), when known
	Branches []string
	Started  time.Time
	Last     time.Time
}

// Counts are aggregates over the conversation's live rows.
type Counts struct {
	Messages  map[string]int // rows by kind
	Tools     map[string]int // tool_call rows by tool name
	Failed    int            // distinct tool calls marked failed
	Subagents int
	Tokens    Tokens
	// Usage is the conversation's last row with usage (a full count
	// sets it so later appends continue its API message).
	Usage *UsageMark
}

// Parse decodes a stored digest; nil or bad JSON is an empty digest.
func Parse(b []byte) *Digest {
	d := &Digest{}
	if len(b) > 0 {
		if json.Unmarshal(b, d) != nil {
			d = &Digest{}
		}
	}
	return d
}

// Marshal encodes d for storage.
func (d *Digest) Marshal() []byte {
	b, _ := json.Marshal(d)
	return b
}

// Update folds msgs into the stored digest prev, sets the conversation
// facts and counts, and returns the new stored form.
func Update(prev []byte, c Conv, msgs []*transcript.Message, n Counts) []byte {
	d := Parse(prev)
	d.setConv(c)
	for _, m := range msgs {
		d.Fold(m)
	}
	d.SetCounts(n)
	if n.Usage != nil || d.State != nil {
		d.state().Usage = n.Usage
	}
	d.Intent = d.intent(c.Title)
	d.compact()
	return d.Marshal()
}

// Append is Update for a batch that only added rows (none replaced, none
// superseded, no path changed): instead of a recount over the whole
// conversation, it adds msgs to the stored counts. failed is the number
// of failed tool calls msgs add (calls with no earlier failed row),
// subagents the current count. A line repeating the usage of the API
// message counted last adds only its growth (Claude repeats a message's
// usage on its lines, which follow one another).
func Append(prev []byte, c Conv, msgs []*transcript.Message, failed, subagents int) []byte {
	return AppendRows(prev, c, msgs, msgs, failed, subagents)
}

// AppendRows is Append for a batch that also rewrote existing rows
// without changing anything the counts depend on (a re-parse that only
// refreshed them): every row of fold is folded, and only the rows of
// count, the new ones, add to the counts.
func AppendRows(prev []byte, c Conv, fold, count []*transcript.Message, failed, subagents int) []byte {
	d := Parse(prev)
	n := Counts{Messages: map[string]int{}, Tools: map[string]int{}, Failed: d.Failed + failed, Subagents: subagents}
	for k, v := range d.Messages {
		n.Messages[k] = v
	}
	for k, v := range d.Tools {
		n.Tools[k] = v
	}
	if d.Tokens != nil {
		n.Tokens = *d.Tokens
	}
	if d.State != nil {
		n.Usage = d.State.Usage
	}
	for _, m := range count {
		if m.Superseded {
			continue
		}
		if m.OnActivePath == nil || *m.OnActivePath {
			n.Messages[m.Kind.String()]++
			if m.Kind == transcript.KindToolCall && m.ToolName != "" {
				n.Tools[m.ToolName]++
			}
		}
		u, ok := UsageOf(m)
		if !ok {
			continue
		}
		mid, _ := m.Enrichment["message_id"].(string)
		switch {
		case n.Usage != nil && mid != "" && n.Usage.Msg == mid:
			if u.Output > n.Usage.Tokens.Output {
				n.Tokens = addTokens(n.Tokens, u, n.Usage.Tokens)
				n.Usage = &UsageMark{Msg: mid, Tokens: u}
			}
		default:
			n.Tokens = addTokens(n.Tokens, u, Tokens{})
			n.Usage = &UsageMark{Msg: mid, Tokens: u}
		}
	}
	d.setConv(c)
	for _, m := range fold {
		d.Fold(m)
	}
	d.SetCounts(n)
	if n.Usage != nil || d.State != nil {
		d.state().Usage = n.Usage
	}
	d.Intent = d.intent(c.Title)
	d.compact()
	return d.Marshal()
}

// addTokens is t plus add minus sub.
func addTokens(t, add, sub Tokens) Tokens {
	return Tokens{Input: t.Input + add.Input - sub.Input, Output: t.Output + add.Output - sub.Output,
		CacheRead: t.CacheRead + add.CacheRead - sub.CacheRead, CacheCreation: t.CacheCreation + add.CacheCreation - sub.CacheCreation}
}

// UsageOf reads a row's API usage from its enrichment (the parser's map,
// or the same decoded from JSON).
func UsageOf(m *transcript.Message) (Tokens, bool) {
	raw, ok := m.Enrichment["usage"]
	if !ok {
		return Tokens{}, false
	}
	get := func(k string) int64 {
		switch u := raw.(type) {
		case map[string]int64:
			return u[k]
		case map[string]any:
			switch v := u[k].(type) {
			case float64:
				return int64(v)
			case int64:
				return v
			case json.Number:
				n, _ := v.Int64()
				return n
			}
		}
		return 0
	}
	return Tokens{Input: get("input_tokens"), Output: get("output_tokens"), CacheRead: get("cache_read_input_tokens"),
		CacheCreation: get("cache_creation_input_tokens")}, true
}

// compact drops what the stored form need not repeat: the prompts the
// intent was chosen from once the first prompt is the intent (their
// ordinals stay), and a cwd equal to the first repo.
func (d *Digest) compact() {
	if st := d.State; st != nil && st.First != "" && !weak(st.First) {
		st.First, st.Long = "", ""
	}
	if st := d.State; st != nil && st.Long != "" && st.Long == st.First {
		st.Long = ""
	}
	if len(d.Repos) > 0 && d.Repos[0] == d.Cwd {
		d.Cwd = ""
	}
}

// intent is the first user prompt; when that is a weak prompt (short, a
// continuation, a pasted error), the harness title, else the first prompt
// that is not weak.
func (d *Digest) intent(title string) string {
	if d.State == nil || d.State.First == "" && d.State.FirstOrd != nil {
		return d.Intent // a first prompt that was no weak prompt (see compact)
	}
	if d.State.First != "" && !weak(d.State.First) {
		return clip(d.State.First, IntentLen)
	}
	if t := oneLine(title); t != "" && !weak(t) {
		return clip(t, IntentLen)
	}
	if d.State.Long != "" {
		return clip(d.State.Long, IntentLen)
	}
	return clip(d.State.First, IntentLen)
}

// weakPrefixes open a prompt that does not say what the session is for:
// continuations, and pasted output or errors.
var weakPrefixes = []string{"continue", "go on", "go ahead", "keep going", "proceed", "resume", "carry on", "yes", "ok", "okay",
	"this session is being continued", "traceback", "panic:", "error", "fatal:", "fail", "exception", "```", "{", "[", "<",
	"$ ", "> ", "at ", "npm err", "diff --git", "--- ", "+++ "}

// weak reports a prompt too short to say what a session is for (under 20
// characters), a continuation, or a paste.
func weak(t string) bool {
	if utf8.RuneCountInString(t) < 20 {
		return true
	}
	l := strings.ToLower(t)
	for _, p := range weakPrefixes {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

func (d *Digest) setConv(c Conv) {
	d.Cwd = c.Cwd
	d.Branches = append([]string(nil), c.Branches...)
	var repos []string
	for _, r := range []string{c.RepoRoot, c.Remote} {
		if r != "" && !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	if len(repos) == 0 && c.Cwd != "" {
		repos = []string{c.Cwd}
	}
	d.Repos = repos
	if len(d.Repos) > maxRepos {
		d.Repos = d.Repos[:maxRepos]
	}
	d.DurationS = 0
	if !c.Started.IsZero() && c.Last.After(c.Started) {
		d.DurationS = int64(c.Last.Sub(c.Started) / time.Second)
	}
}

// SetCounts replaces the counted fields.
func (d *Digest) SetCounts(n Counts) {
	d.Messages = nonEmpty(n.Messages)
	d.Tools = nonEmpty(n.Tools)
	d.Commands = 0
	for t, k := range n.Tools {
		if IsShell(t) {
			d.Commands += k
		}
	}
	d.Failed, d.Subagents = n.Failed, n.Subagents
	d.Tokens = nil
	if n.Tokens != (Tokens{}) {
		t := n.Tokens
		d.Tokens = &t
	}
}

func nonEmpty(m map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range m {
		if k != "" && v > 0 {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// root is the directory edited files are shown relative to.
func (d *Digest) root() string {
	if len(d.Repos) > 0 && strings.HasPrefix(d.Repos[0], "/") {
		return d.Repos[0]
	}
	return d.Cwd
}

// Fold reads one message into the digest.
func (d *Digest) Fold(m *transcript.Message) {
	if m.Superseded {
		return
	}
	switch m.Kind {
	case transcript.KindUser:
		if t := oneLine(m.Text); t != "" && !noise(t) {
			st := d.state()
			t = clip(t, IntentLen)
			// An equal ordinal is a newer version of the row (a re-parse):
			// its text replaces the old.
			if st.FirstOrd == nil || m.Ordinal <= *st.FirstOrd {
				st.First, st.FirstOrd = t, ptr(m.Ordinal)
			}
			if !weak(t) && (st.LongOrd == nil || m.Ordinal <= *st.LongOrd) {
				st.Long, st.LongOrd = t, ptr(m.Ordinal)
			}
		}
		d.textRefs(m.Text)
	case transcript.KindAssistant:
		if t := oneLine(m.Text); t != "" {
			if d.State == nil || d.State.LastOrd == nil || m.Ordinal >= *d.State.LastOrd {
				d.state().LastOrd = ptr(m.Ordinal)
				d.Last = clip(t, LastLen)
			}
		}
		d.textRefs(m.Text)
	case transcript.KindToolCall:
		for _, p := range EditedPaths(m) {
			d.addFile(p)
		}
		if IsShell(m.ToolName) || len(commandsOf(m)) > 0 {
			if cls := classify(Command(m)); cls != "" && m.ToolCallID != "" {
				p := d.state().pendingMap()
				if len(p) < maxPending {
					p[m.ToolCallID] = cls
				}
			}
		}
	case transcript.KindToolResult:
		if d.State == nil || m.ToolCallID == "" {
			return
		}
		cls, ok := d.State.Pending[m.ToolCallID]
		if !ok {
			return
		}
		delete(d.State.Pending, m.ToolCallID)
		if len(d.State.Pending) == 0 {
			d.State.Pending = nil
		}
		if m.IsError {
			return
		}
		for _, c := range strings.Split(cls, ",") {
			switch c {
			case "pr":
				for _, u := range prURL.FindAllStringSubmatch(m.Text, -1) {
					d.add("prs", &d.PRs, u[1]+"#"+u[2], maxPRs)
				}
			case "commit":
				for _, sm := range commitOut.FindAllStringSubmatch(m.Text, -1) {
					d.add("commits", &d.Commits, sm[1], maxCommit)
				}
			case "issue":
				for _, u := range issueURL.FindAllStringSubmatch(m.Text, -1) {
					d.add("issues", &d.Issues, u[1]+"#"+u[2], maxIssues)
				}
			}
		}
	}
}

// textRefs records the PR and issue URLs a prompt or reply names.
func (d *Digest) textRefs(text string) {
	if !strings.Contains(text, "github.com/") {
		return
	}
	for _, u := range prURL.FindAllStringSubmatch(text, -1) {
		d.add("prs", &d.PRs, u[1]+"#"+u[2], maxPRs)
	}
	for _, u := range issueURL.FindAllStringSubmatch(text, -1) {
		d.add("issues", &d.Issues, u[1]+"#"+u[2], maxIssues)
	}
}

func (d *Digest) state() *State {
	if d.State == nil {
		d.State = &State{}
	}
	return d.State
}

func (s *State) pendingMap() map[string]string {
	if s.Pending == nil {
		s.Pending = map[string]string{}
	}
	return s.Pending
}

func (d *Digest) addFile(p string) {
	p = strings.TrimSpace(p)
	if p == "" {
		return
	}
	if r := d.root(); r != "" && strings.HasPrefix(p, r+"/") {
		p = p[len(r)+1:]
	}
	p = shortPath(path.Clean(p))
	if slices.Contains(d.FilesEdited, p) {
		return
	}
	if len(d.FilesEdited) >= maxFiles {
		d.FilesMore = true
		return
	}
	d.FilesEdited = append(d.FilesEdited, p)
}

// shortPath keeps the last four elements of a long absolute path (one
// outside the session's repo), marked "…/".
func shortPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.Count(p, "/") <= 4 {
		return p
	}
	parts := strings.Split(p, "/")
	return "…/" + strings.Join(parts[len(parts)-4:], "/")
}

// add adds s to the set list named name, keeping the first n and
// recording in More that there were more.
func (d *Digest) add(name string, list *[]string, s string, n int) {
	if slices.Contains(*list, s) {
		return
	}
	if len(*list) >= n {
		if !slices.Contains(d.More, name) {
			d.More = append(d.More, name)
		}
		return
	}
	*list = append(*list, s)
}

// Truncated reports whether the list named name ("prs", "commits",
// "issues") hit its cap.
func (d *Digest) Truncated(name string) bool { return slices.Contains(d.More, name) }

var (
	prURL     = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)
	issueURL  = regexp.MustCompile(`https://github\.com/([\w.-]+/[\w.-]+)/issues/(\d+)`)
	commitOut = regexp.MustCompile(`\[[^\]\s]+(?: \([^)\n]*\))? ([0-9a-f]{7,40})\] `)
	prCmd     = regexp.MustCompile(`\bgh\s+pr\s+(?:create|edit|merge|view|ready|comment|review|close|reopen)\b`)
	issueCmd  = regexp.MustCompile(`\bgh\s+issue\s+(?:create|edit|view|comment|close|reopen)\b`)
	commitCmd = regexp.MustCompile(`\bgit\b[^|;&\n]*?\bcommit\b`)
)

// classify says what a shell command's output may name.
func classify(cmd string) string {
	var out []string
	if prCmd.MatchString(cmd) {
		out = append(out, "pr")
	}
	if commitCmd.MatchString(cmd) {
		out = append(out, "commit")
	}
	if issueCmd.MatchString(cmd) {
		out = append(out, "issue")
	}
	return strings.Join(out, ",")
}

// shellTools are the tools that run a shell command, lowercased.
var shellTools = map[string]bool{"bash": true, "exec_command": true, "shell": true, "local_shell": true, "shell_command": true,
	"container.exec": true, "run_command": true, "exec": true}

// IsShell reports whether a tool runs shell commands.
func IsShell(tool string) bool { return shellTools[strings.ToLower(tool)] }

// commandsOf returns the commands a tool call's enrichment records (Codex
// events, Devin).
func commandsOf(m *transcript.Message) []string {
	v, ok := m.Enrichment["commands"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var cs []struct {
		Cmd string `json:"cmd"`
	}
	if json.Unmarshal(b, &cs) != nil {
		return nil
	}
	var out []string
	for _, c := range cs {
		if c.Cmd != "" {
			out = append(out, c.Cmd)
		}
	}
	return out
}

// Command is the shell command a tool call runs: its arguments' command
// or cmd (a string, or an argv joined by spaces), else the commands its
// enrichment records, else "".
func Command(m *transcript.Message) string {
	if args := jsonArgs(m.Text); args != nil {
		for _, k := range []string{"command", "cmd"} {
			if s := argString(args[k]); s != "" {
				return unwrapShell(s)
			}
		}
	}
	return strings.Join(commandsOf(m), "; ")
}

// unwrapShell drops a "bash -lc " style wrapper from an argv joined by
// spaces.
func unwrapShell(s string) string {
	for _, p := range []string{"bash -lc ", "bash -c ", "sh -c ", "zsh -lc ", "zsh -c ", "/bin/bash -lc ", "/bin/zsh -lc ", "/bin/sh -c "} {
		if strings.HasPrefix(s, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}
	return s
}

func jsonArgs(text string) map[string]any {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "{") {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(t), &args) != nil {
		return nil
	}
	return args
}

func argString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, p := range x {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// editTools are Claude's file-editing tools, by the argument naming the
// file.
var editTools = map[string]string{"Edit": "file_path", "Write": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path"}

// EditedPaths are the files a tool call edits: Claude's Edit, Write,
// MultiEdit and NotebookEdit; the changed paths Codex and Devin record;
// else the files an apply_patch patch names.
func EditedPaths(m *transcript.Message) []string {
	if key, ok := editTools[m.ToolName]; ok {
		if args := jsonArgs(m.Text); args != nil {
			if s := argString(args[key]); s != "" {
				return []string{s}
			}
		}
		return nil
	}
	if v, ok := m.Enrichment["changed_paths"]; ok {
		switch x := v.(type) {
		case []string:
			return x
		case []any:
			var out []string
			for _, p := range x {
				if s, ok := p.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	if strings.Contains(m.Text, "*** Begin Patch") {
		return PatchPaths(m.Text)
	}
	return nil
}

var patchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+)$|^\*\*\* Move to: (.+)$`)

// PatchPaths are the files an apply_patch patch adds, updates, deletes or
// moves to. The patch may sit JSON-escaped inside arguments.
func PatchPaths(text string) []string {
	if args := jsonArgs(text); args != nil {
		for _, k := range []string{"input", "patch"} {
			if s := argString(args[k]); s != "" {
				text = s
			}
		}
	}
	var out []string
	for _, sm := range patchFile.FindAllStringSubmatch(text, -1) {
		p := strings.TrimSpace(sm[1] + sm[2])
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// noise is a user row that is no prompt: an interruption marker.
func noise(t string) bool {
	return strings.HasPrefix(t, "[Request interrupted")
}

// oneLine collapses whitespace runs, newlines included, to one space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clip cuts s to at most n bytes at a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "…"
}

func ptr(v int64) *int64 { return &v }

// Mask hides redacted text in a stored digest (a message redaction):
// lines are the hidden lines of a message's text (for a whole message,
// all of them). A text field or list entry that a line holds becomes
// mask of it; a line inside a field is masked there, as is the start of a
// line that a field's clipped end holds.
func Mask(b []byte, lines []string, mask func(string) string) []byte {
	if len(b) == 0 {
		return b
	}
	d := Parse(b)
	var needles []string
	for _, l := range lines {
		if l = oneLine(l); l != "" {
			needles = append(needles, l)
		}
	}
	hide := func(f string) string {
		core := strings.TrimSuffix(f, "…")
		if core == "" {
			return f
		}
		for _, n := range needles {
			if strings.Contains(n, core) {
				return mask(core) + f[len(core):]
			}
		}
		for _, n := range needles {
			core = strings.ReplaceAll(core, n, mask(n))
			for k := min(len(n)-1, len(core)); k >= 8; k-- {
				if strings.HasSuffix(core, n[:k]) {
					core = core[:len(core)-k] + mask(n[:k])
					break
				}
			}
		}
		return core + f[len(strings.TrimSuffix(f, "…")):]
	}
	d.Intent, d.Last = hide(d.Intent), hide(d.Last)
	if st := d.State; st != nil {
		st.First, st.Long = hide(st.First), hide(st.Long)
	}
	for _, list := range []*[]string{&d.FilesEdited, &d.PRs, &d.Commits, &d.Issues} {
		for i := range *list {
			(*list)[i] = hide((*list)[i])
		}
	}
	return d.Marshal()
}
