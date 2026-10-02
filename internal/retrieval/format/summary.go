package format

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

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

// ago renders a duration coarsely: 0m, 4m, 2h, 3d.
func ago(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d/time.Minute), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// when is a live session's "live: AGE" (since its last activity), else
// "ended: DATE".
func when(c *ConversationInfo, now time.Time) string {
	if c.Live {
		if c.LastActivityAt == nil {
			return field("live", "yes")
		}
		return field("live", ago(now.Sub(*c.LastActivityAt)))
	}
	if c.LastActivityAt == nil {
		return ""
	}
	return field("ended", c.LastActivityAt.UTC().Format(time.DateOnly))
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

// outcome is the short digest's counts as labeled fields: files edited,
// the first PR (and how many more), commits, failed tool calls. A count
// the digest capped ends in "+".
func outcome(d *digest.Digest) []string {
	if d == nil {
		return nil
	}
	var out []string
	more := func(b bool) string {
		if b {
			return "+"
		}
		return ""
	}
	if n := len(d.FilesEdited); n > 0 {
		out = append(out, field("files", fmt.Sprintf("%d%s", n, more(d.FilesMore))))
	}
	if len(d.PRs) > 0 {
		pr := d.PRs[0]
		if m := prNum.FindStringSubmatch(pr); m != nil {
			pr = "#" + m[1]
		}
		out = append(out, field("pr", clip(pr, 60)))
		if len(d.PRs) > 1 || d.Truncated("prs") {
			out = append(out, field("prs", fmt.Sprintf("%d%s", len(d.PRs), more(d.Truncated("prs")))))
		}
	}
	if n := len(d.Commits); n > 0 {
		out = append(out, field("commits", fmt.Sprintf("%d%s", n, more(d.Truncated("commits")))))
	}
	if d.Failed > 0 {
		out = append(out, field("failed", strconv.Itoa(d.Failed)))
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
	MaxHeader   = 260
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

// summary is a session's one-line short digest as labeled fields (see
// field): session (its address), who, agent, live or ended, repo, branch,
// extra (the caller's fields), what it did as counts and ids, and last
// the intent. A session without a digest yet gets the first six. Empty
// fields are left out. The line stays within about MaxHeader bytes
// whatever the session holds: long names are cut, then the intent
// shrinks, then who, repo and branch go. The address is never cut: read
// takes it back with the hit's ORDINAL:LINE.
func summary(c *ConversationInfo, now time.Time, extra ...string) string {
	w := ""
	if s := who(c); s != "" {
		w = field("who", clip(Clean(s), 40))
	}
	var where []string
	if c.Repo != "" {
		where = append(where, field("repo", clip(Clean(repoName(c.Repo)), 50)))
	}
	if b := branchLabel(c.Branches); b != "" {
		where = append(where, field("branch", clip(Clean(b), 50)))
	}
	tail := outcome(c.Digest)
	build := func(w string, where []string, intent bool) string {
		parts := []string{sessionValue(sessionID(c))}
		if w != "" {
			parts = append(parts, w)
		}
		parts = append(parts, field("agent", clip(c.Agent, 12)))
		if s := when(c, now); s != "" {
			parts = append(parts, s)
		}
		parts = append(parts, where...)
		parts = append(parts, extra...)
		parts = append(parts, tail...)
		if intent {
			if i := intentOf(c); i != "" {
				line := strings.Join(parts, fieldSep)
				if room := min(IntentShort, MaxHeader-len(line)-len(` intent="…"`)); room >= 16 {
					parts = append(parts, quotedField("intent", shortIntent(i, room)))
				}
			}
		}
		return strings.Join(parts, fieldSep)
	}
	line := build(w, where, true)
	if len(line) > MaxHeader {
		line = build("", where, false)
	}
	if len(line) > MaxHeader {
		line = build("", nil, false)
	}
	return line
}

// fieldSep separates the fields of a header line.
const fieldSep = " "

// header is the grouped layout's line opening a session's hits.
func header(c *ConversationInfo, now time.Time) string {
	return "## " + summary(c, now)
}

// A header line is the session id, bare, then key=value fields, one
// space apart (fieldSep). A value prints bare when it is a plain token,
// else as a JSON string, so a header splits unambiguously however its
// values read: a quoted value may hold spaces, quotes or text that looks
// like another field. Text fields (intent, title) always quote
// (quotedField) and come last.

// sessionValue is a header's first value: the session id, bare unless it
// needs quoting.
func sessionValue(v string) string {
	v = Clean(v)
	if !bareValue(v) {
		return jsonQuote(oneLine(v))
	}
	return v
}

// sessionID is the id a header leads with: the full session id (an
// address prefix unique today may not be tomorrow), else the address.
func sessionID(c *ConversationInfo) string {
	if c.SessionID != "" {
		return c.SessionID
	}
	return c.Address
}

// hitSession is the id a header opening h's session leads with.
func hitSession(h *Hit) string {
	if h.SessionID != "" {
		return h.SessionID
	}
	return sessionOf(h.Address)
}

// field is one key=value field of a header line.
func field(key, v string) string {
	v = Clean(v)
	if !bareValue(v) {
		return quotedField(key, v)
	}
	return key + "=" + v
}

// quotedField is key="value", the value on one line (newlines as ⏎),
// control characters shown, quoted as a JSON string.
func quotedField(key, v string) string {
	return key + "=" + jsonQuote(oneLine(v))
}

// bareValue reports whether v can print unquoted: not empty, and no
// space (of any kind), quote, backslash, "=" or control character.
func bareValue(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r == '"' || r == '\\' || r == '=' || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// jsonQuote quotes s as a JSON string, leaving printable Unicode as is.
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x2028 || r == 0x2029 || !unicode.IsPrint(r) && r != ' ':
			if r > 0xffff {
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
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
