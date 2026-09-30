package format

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
)

// now is the style's clock.
func (s Style) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// who is user@device, the user, or a device other than the local one.
func who(c *ConversationInfo) string {
	dev := c.Device
	if dev == "local" {
		dev = ""
	}
	switch {
	case c.User != "" && dev != "":
		return c.User + "@" + dev
	case c.User != "":
		return c.User
	}
	return dev
}

// ago renders a duration coarsely: just now, 4m ago, 2h ago, 3d ago.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// when is "live, 4m ago" for a live session, else "ended DATE".
func when(c *ConversationInfo, now time.Time) string {
	if c.Live {
		if c.LastActivityAt == nil {
			return "live"
		}
		return "live, " + ago(now.Sub(*c.LastActivityAt))
	}
	if c.LastActivityAt == nil {
		return ""
	}
	return "ended " + c.LastActivityAt.UTC().Format(time.DateOnly)
}

// branchLabel is the branches a session ran on: one, a→b, or a→…→z.
func branchLabel(bs []string) string {
	switch len(bs) {
	case 0:
		return ""
	case 1:
		return bs[0]
	case 2:
		return bs[0] + "→" + bs[1]
	}
	return bs[0] + "→…→" + bs[len(bs)-1]
}

// repoAt is the repo name, @ the branch when there is one.
func repoAt(repo string, branches []string) string {
	r := repoName(repo)
	if b := branchLabel(branches); b != "" {
		return r + "@" + b
	}
	return r
}

var prNum = regexp.MustCompile(`#(\d+)$`)

// outcome is the short digest's counts: files edited, the first PR (and
// how many more), commits, failed tool calls.
func outcome(d *digest.Digest) []string {
	if d == nil {
		return nil
	}
	var out []string
	if n := len(d.FilesEdited); n > 0 {
		more := ""
		if d.FilesMore {
			more = "+"
		}
		out = append(out, fmt.Sprintf("%d%s %s", n, more, plural(n, "file", "files")))
	}
	if len(d.PRs) > 0 {
		pr := d.PRs[0]
		if m := prNum.FindStringSubmatch(pr); m != nil {
			pr = "#" + m[1]
		}
		s := "PR " + clip(pr, 60)
		if len(d.PRs) > 1 || d.Truncated("prs") {
			s += fmt.Sprintf(" +%d", len(d.PRs)-1)
			if d.Truncated("prs") {
				s += "+"
			}
		}
		out = append(out, s)
	}
	if n := len(d.Commits); n > 0 {
		more := ""
		if d.Truncated("commits") {
			more = "+"
		}
		out = append(out, fmt.Sprintf("%d%s %s", n, more, plural(n, "commit", "commits")))
	}
	if d.Failed > 0 {
		out = append(out, fmt.Sprintf("✗%d", d.Failed))
	}
	return out
}

// intentOf is what a session was for: the digest's intent, else the
// title.
func intentOf(c *ConversationInfo) string {
	if c.Digest != nil && c.Digest.Intent != "" {
		return c.Digest.Intent
	}
	return c.Title
}

// quoteClip quotes s on one line, cut to n bytes.
func quoteClip(s string, n int) string {
	if s == "" {
		return ""
	}
	return strconv.Quote(oneLine(ClipAround(s, 0, n)))
}

// Header bounds: the intent is at most IntentShort bytes (cut at a word,
// or its first sentence when shorter), the whole line about MaxHeader.
const (
	IntentShort = 80
	MaxHeader   = 200
)

// shortIntent is s's first sentence when that fits in n bytes, else s cut
// at the last word boundary within n, marked "…".
func shortIntent(s string, n int) string {
	s = oneLine(s)
	for i := 0; i+1 < len(s) && i < n; i++ {
		if (s[i] == '.' || s[i] == '?' || s[i] == '!') && s[i+1] == ' ' {
			return s[:i+1]
		}
	}
	if len(s) <= n {
		return s
	}
	cut := clip(s, n)
	cut = strings.TrimSuffix(cut, "…")
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

// summary is a session's one-line short digest: address, who, agent, when
// (live or ended), repo@branch, intent, and what it did, as counts and
// ids. A session without a digest yet gets the first five. The fields are
// separated by two spaces; empty ones are left out. The line stays within
// about MaxHeader bytes whatever the session holds: long names are cut,
// then the intent shrinks, then who and repo go. The address is never
// cut: read takes it back with the hit's ORDINAL:LINE.
func summary(c *ConversationInfo, now time.Time, extra ...string) string {
	w := ""
	if s := who(c); s != "" {
		w = clip(Clean(s), 40)
	}
	repo := ""
	if c.Repo != "" {
		repo = clip(Clean(repoAt(c.Repo, c.Branches)), 50)
	}
	tail := outcome(c.Digest)
	build := func(w, repo string, intent bool) string {
		parts := []string{c.Address}
		if w != "" {
			parts = append(parts, w)
		}
		parts = append(parts, clip(c.Agent, 12))
		if s := when(c, now); s != "" {
			parts = append(parts, s)
		}
		if repo != "" {
			parts = append(parts, repo)
		}
		parts = append(parts, extra...)
		if intent {
			if i := intentOf(c); i != "" {
				line := strings.Join(append(append([]string{}, parts...), tail...), "  ")
				if room := min(IntentShort, MaxHeader-len(line)-4); room >= 16 {
					parts = append(parts, strconv.Quote(Clean(shortIntent(i, room))))
				}
			}
		}
		return strings.Join(append(parts, tail...), "  ")
	}
	line := build(w, repo, true)
	if len(line) > MaxHeader {
		line = build("", repo, false)
	}
	if len(line) > MaxHeader {
		line = build("", "", false)
	}
	return line
}

// header is the grouped layout's line opening a session's hits.
func header(c *ConversationInfo, now time.Time) string {
	return "## " + summary(c, now)
}

// writeDigest prints a session's full digest, one "# " line per field
// group, for read --outline.
func writeDigest(e *errWriter, c *ConversationInfo) {
	d := c.Digest
	if d == nil {
		e.printf("# digest: not computed yet\n")
		return
	}
	line := func(label string, v ...string) {
		var kept []string
		for _, s := range v {
			if s != "" {
				kept = append(kept, s)
			}
		}
		if len(kept) > 0 {
			e.printf("# %s: %s\n", label, strings.Join(kept, "  "))
		}
	}
	line("intent", quoteClip(d.Intent, digest.IntentLen+10))
	var dur string
	if d.DurationS > 0 {
		dur = "duration " + (time.Duration(d.DurationS) * time.Second).String()
	}
	line("where", strings.Join(d.Repos, " "), branchLabel(d.Branches), cwdIfOther(d), dur)
	var kinds []string
	for _, k := range []string{"user", "assistant", "tool_call", "tool_result", "thinking", "agent_message", "system"} {
		if n := d.Messages[k]; n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
		}
	}
	line("messages", strings.Join(kinds, ", "))
	var activity []string
	if d.Subagents > 0 {
		activity = append(activity, fmt.Sprintf("%d %s", d.Subagents, plural(d.Subagents, "subagent", "subagents")))
	}
	if d.Commands > 0 {
		activity = append(activity, fmt.Sprintf("%d %s", d.Commands, plural(d.Commands, "command", "commands")))
	}
	if d.Failed > 0 {
		activity = append(activity, fmt.Sprintf("%d failed %s", d.Failed, plural(d.Failed, "call", "calls")))
	}
	line("activity", strings.Join(activity, ", "))
	line("tools", toolList(d.Tools))
	if len(d.FilesEdited) > 0 {
		more := ""
		if d.FilesMore {
			more = " (and more)"
		}
		line(fmt.Sprintf("files edited (%d)", len(d.FilesEdited)), strings.Join(d.FilesEdited, ", ")+more)
	}
	line("PRs", strings.Join(d.PRs, " "))
	line("commits", strings.Join(d.Commits, " "))
	line("issues", strings.Join(d.Issues, " "))
	if t := d.Tokens; t != nil {
		line("tokens", fmt.Sprintf("input %s, output %s, cache read %s, cache write %s", si(t.Input), si(t.Output), si(t.CacheRead), si(t.CacheCreation)))
	}
	line("last", quoteClip(d.Last, digest.LastLen+10))
}

// cwdIfOther is the digest's cwd when it is not its first repo.
func cwdIfOther(d *digest.Digest) string {
	if d.Cwd == "" || len(d.Repos) > 0 && d.Repos[0] == d.Cwd {
		return ""
	}
	return "cwd " + d.Cwd
}

// toolList is tool counts, most used first.
func toolList(m map[string]int) string {
	type tc struct {
		t string
		n int
	}
	var l []tc
	for t, n := range m {
		l = append(l, tc{t, n})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].n > l[j].n || l[i].n == l[j].n && l[i].t < l[j].t })
	var out []string
	for _, x := range l {
		out = append(out, fmt.Sprintf("%s %d", x.t, x.n))
	}
	return strings.Join(out, ", ")
}

// si renders a count as 1.2k, 3.4M.
func si(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// hitLabel is a hit's kind (kind/tool) and flags, for the grouped layout.
func hitLabel(h *Hit) string {
	l := h.Kind
	if h.ToolName != "" {
		l += "/" + Clean(h.ToolName)
	}
	if h.IsError {
		l += " error"
	}
	if h.Copies > 0 {
		l += fmt.Sprintf(" +%d copies", h.Copies)
	}
	if h.Superseded {
		l += " superseded"
	}
	if h.OffPath {
		l += " off-path"
	}
	return l
}

// ordinalOf is the part of an address after the session: ORDINAL.
func ordinalOf(addr string) string {
	if i := strings.LastIndexByte(addr, '/'); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// sessionOf is the session part of an address.
func sessionOf(addr string) string {
	if i := strings.LastIndexByte(addr, '/'); i >= 0 {
		return addr[:i]
	}
	return addr
}
