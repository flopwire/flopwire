package format

import (
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// Style picks how continuation hints read (CLI flags or MCP arguments)
// and the output budget.
type Style struct {
	MCP bool
	// Budget bounds the text of one answer, in bytes: grep, search and
	// sessions print whole hits until the next would pass it, read drops
	// the farthest neighbours, and the footer says where to go on. An
	// agent's host cuts long tool output itself (Claude Code keeps 30000
	// characters of a command's output and 25000 tokens of an MCP
	// result), which would lose the footer. 0 means no budget.
	Budget int
	// Flat prints grep and search hits one per line with their
	// attribution (--no-heading) instead of grouped under a header per
	// session.
	Flat bool
	// Now is the clock for "live, 4m ago"; default time.Now.
	Now func() time.Time
}

// MaxOutput is the budget of an MCP answer: about 6-8k tokens, under
// Claude Code's 10k-token MCP warning. The CLI has no budget unless
// --max-bytes sets one.
const MaxOutput = 24000

// fits reports whether n more bytes fit when used are spent; the first
// unit always fits.
func (s Style) fits(used, n int) bool {
	return s.Budget <= 0 || used == 0 || used+n <= s.Budget
}

func (s Style) offset(n int) string {
	if s.MCP {
		return "offset=" + strconv.Itoa(n)
	}
	return "--offset " + strconv.Itoa(n)
}

func (s Style) readAt(addr string, lineOffset int) string {
	if s.MCP {
		return fmt.Sprintf("flopwire_read address=%s line_offset=%d", addr, lineOffset)
	}
	return fmt.Sprintf("flopwire read %s --line-offset %d", addr, lineOffset)
}

func (s Style) read(addr string) string {
	if s.MCP {
		return "flopwire_read address=" + addr
	}
	return "flopwire read " + addr
}

// more is the hint for messages beyond the ones read shows: the address
// to go on from and the direction.
func (s Style) more(addr, dir string) string {
	if s.MCP {
		return fmt.Sprintf("flopwire_read address=%s %s=10", addr, dir)
	}
	flag := map[string]string{"before": "-B", "after": "-A"}[dir]
	return fmt.Sprintf("flopwire read %s %s 10", addr, flag)
}

// cursor is the argument that reads the next sessions page.
func (s Style) cursor(c string) string {
	if s.MCP {
		return "cursor=" + c
	}
	return "--cursor " + c
}

// outlineAt is the call reading an outline on after cursor c.
func (s Style) outlineAt(addr, c string) string {
	if s.MCP {
		return fmt.Sprintf("flopwire_read address=%s outline=true cursor=%s", addr, c)
	}
	return fmt.Sprintf("flopwire read %s --outline --cursor %s", addr, c)
}

func (s Style) flag(cli, mcp string) string {
	if s.MCP {
		return mcp
	}
	return cli
}

// errWriter keeps the first write error.
type errWriter struct {
	w   io.Writer
	err error
}

// printf cleans every string argument (decision D17): ids, addresses and
// agent names come from transcript data like the text does.
func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	for i, a := range args {
		if s, ok := a.(string); ok {
			args[i] = Clean(s)
		}
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

// stampLayout is how hits print a time; since and until accept it back.
const stampLayout = "2006-01-02 15:04Z"

// stamp renders a time in UTC to the minute, marked Z so an agent never
// takes it for local time.
func stamp(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(stampLayout)
}

func repoName(repo string) string {
	if repo == "" {
		return "-"
	}
	return path.Base(repo)
}

func kindLabel(kind, tool string) string {
	if tool != "" {
		return kind + "(" + Clean(tool) + ")"
	}
	return kind
}

// meta is a hit's bracketed attribution: agent, kind, time, repo, and the
// user when the server answered.
func meta(h *Hit) string {
	parts := []string{h.Agent, kindLabel(h.Kind, h.ToolName), stamp(h.TS), Clean(repoAt(h.Repo, h.Branches))}
	if h.User != "" {
		parts = append(parts, Clean(h.User))
	}
	if h.IsError {
		parts = append(parts, "error")
	}
	if h.Copies > 0 {
		parts = append(parts, fmt.Sprintf("+%d copies", h.Copies))
	}
	if h.Superseded {
		parts = append(parts, "superseded")
	}
	if h.OffPath {
		parts = append(parts, "off-path")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func oneLine(s string) string {
	return strings.ReplaceAll(Clean(strings.ReplaceAll(s, "\r\n", "\n")), "\n", "⏎")
}

func count(n int, exact bool) string {
	if exact {
		return strconv.Itoa(n)
	}
	return strconv.Itoa(n) + "+"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// notes prints what qualifies the hits, before them: why the page is
// partial, an any-term retry, an unindexed scan, the rank cap. An agent
// reads these before it weighs the hits.
func notes(e *errWriter, p *Page) {
	if p.Truncated && p.Reason != "" {
		e.printf("[%s]\n", Clean(p.Reason))
	}
	for _, n := range p.Notes {
		e.printf("[%s]\n", Clean(n))
	}
}

// excluded prints the closing line naming the left-out calling session.
func excluded(e *errWriter, note string) {
	if note != "" {
		e.printf("[%s]\n", Clean(note))
	}
}

// units renders each of n units into its own string with the budget
// applied: whole units until the next would pass it. It returns the units
// that fit.
func units(st Style, n int, render func(e *errWriter, i int)) ([]string, error) {
	var out []string
	used := 0
	for i := 0; i < n; i++ {
		var b strings.Builder
		e := &errWriter{w: &b}
		render(e, i)
		if e.err != nil {
			return nil, e.err
		}
		if !st.fits(used, b.Len()) {
			break
		}
		used += b.Len()
		out = append(out, b.String())
	}
	return out, nil
}

// budgetNote is the footer's clause when the budget cut the page.
func budgetNote(st Style) string {
	return fmt.Sprintf("output budget of %d bytes reached; ", st.Budget)
}

// WriteGrep renders a grep page. What qualifies the hits (truncation,
// notes) comes first. Content mode prints each matching line as
// ADDRESS:LINE: text, rg style, with the hit's attribution in brackets on
// its first matching line, context lines as ADDRESS-LINE- text, and "--"
// between separated groups. -l mode prints a line per session, -c mode
// SESSION:COUNT. A footer gives the totals and the next offset; the
// budget (Style.Budget) ends the page early and the footer says so.
func WriteGrep(w io.Writer, p *Page, mode string, st Style) error {
	e := &errWriter{w: w}
	notes(e, p)
	switch mode {
	case ModeSessions, ModeCount:
		lines, err := units(st, len(p.Sessions), func(e *errWriter, i int) {
			s := &p.Sessions[i]
			if mode == ModeCount {
				e.printf("%s:%d\n", s.Address, s.Hits)
				return
			}
			e.printf("%s\n", sessionLine(s))
		})
		if err != nil {
			return err
		}
		for _, l := range lines {
			e.printf("%s", l)
		}
		next, cut := p.Next, ""
		if len(lines) < len(p.Sessions) {
			next, cut = p.Offset+len(lines), budgetNote(st)
		}
		switch {
		case len(p.Sessions) == 0 && !p.Truncated:
			e.printf("[no matches]\n")
		case next > 0:
			e.printf("[showing %d-%d of %s sessions (%s %s); %snext: %s]\n", p.Offset+1, p.Offset+len(lines),
				count(p.TotalSessions, p.Exact), count(p.Total, p.Exact), plural(p.Total, "hit", "hits"), cut, st.offset(next))
		default:
			e.printf("[%s %s, %s %s]\n", count(p.TotalSessions, p.Exact), plural(p.TotalSessions, "session", "sessions"),
				count(p.Total, p.Exact), plural(p.Total, "hit", "hits"))
		}
		excluded(e, p.Excluded)
		return e.err
	}
	g := newGrouper(p, st)
	sep := false
	blocks, err := units(st, len(p.Hits), func(e *errWriter, i int) {
		h := &p.Hits[i]
		if !st.Flat {
			if g.open(e, h) {
				sep = false
			}
			groupedHit(e, h, sep)
			sep = hasContext(h)
			return
		}
		if i > 0 && sep {
			e.printf("--\n")
		}
		context := false
		metaDone := false
		prev := 0
		for _, l := range h.Lines {
			if prev > 0 && l.N > prev+1 {
				e.printf("--\n")
			}
			prev = l.N
			text := Clean(l.Text)
			if !l.Match {
				context = true
				e.printf("%s-%d- %s\n", h.Address, l.N, text)
				continue
			}
			if !metaDone {
				metaDone = true
				e.printf("%s:%d: %s %s\n", h.Address, l.N, meta(h), text)
				continue
			}
			e.printf("%s:%d: %s\n", h.Address, l.N, text)
		}
		if h.MoreLines > 0 {
			e.printf("%s: [+%d more matching %s; %s]\n", h.Address, h.MoreLines, plural(h.MoreLines, "line", "lines"), st.read(h.Address))
		}
		sep = context
	})
	if err != nil {
		return err
	}
	for _, b := range blocks {
		e.printf("%s", b)
	}
	shown, next, cut := len(blocks), p.Next, ""
	if shown < len(p.Hits) {
		next, cut = p.Offset+shown, budgetNote(st)
	}
	switch {
	case len(p.Hits) == 0 && !p.Truncated:
		if p.Offset > 0 {
			e.printf("[no hits past offset %d; %s hits in all]\n", p.Offset, count(p.Total, p.Exact))
		} else {
			e.printf("[no matches]\n")
		}
	case len(p.Hits) == 0:
	case next > 0:
		e.printf("[showing %d-%d of %s hits in %s %s; %snext: %s]\n", p.Offset+1, p.Offset+shown,
			count(p.Total, p.Exact), count(p.TotalSessions, p.Exact), plural(p.TotalSessions, "session", "sessions"), cut, st.offset(next))
	case p.Offset > 0:
		e.printf("[showing %d-%d of %s hits in %s %s]\n", p.Offset+1, p.Offset+shown, count(p.Total, p.Exact),
			count(p.TotalSessions, p.Exact), plural(p.TotalSessions, "session", "sessions"))
	default:
		e.printf("[%s %s in %s %s]\n", count(p.Total, p.Exact), plural(p.Total, "hit", "hits"),
			count(p.TotalSessions, p.Exact), plural(p.TotalSessions, "session", "sessions"))
	}
	excluded(e, p.Excluded)
	return e.err
}

// WriteSearch renders a search page: what qualifies the hits first, then
// one line per hit, ADDRESS:LINE, the attribution, and the snippet with
// its newlines shown as ⏎, then the footer.
func WriteSearch(w io.Writer, p *Page, st Style) error {
	e := &errWriter{w: w}
	notes(e, p)
	g := newGrouper(p, st)
	lines, err := units(st, len(p.Hits), func(e *errWriter, i int) {
		h := &p.Hits[i]
		addr := h.Address
		if !st.Flat {
			g.open(e, h)
			addr = ordinalOf(addr)
		}
		if h.TextLine > 0 {
			addr += ":" + strconv.Itoa(h.TextLine)
		}
		if !st.Flat {
			e.printf("%s %s: %s\n", addr, hitLabel(h), oneLine(h.Snippet))
			return
		}
		e.printf("%s: %s %s\n", addr, meta(h), oneLine(h.Snippet))
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		e.printf("%s", l)
	}
	shown, next, cut := len(lines), p.Next, ""
	if shown < len(p.Hits) {
		next, cut = p.Offset+shown, budgetNote(st)
	}
	switch {
	case len(p.Hits) == 0 && !p.Truncated:
		e.printf("[no matches]\n")
	case len(p.Hits) == 0:
	case next > 0:
		e.printf("[showing %d-%d; %snext: %s]\n", p.Offset+1, p.Offset+shown, cut, st.offset(next))
	default:
		e.printf("[%d %s]\n", p.Offset+shown, plural(p.Offset+shown, "hit", "hits"))
	}
	excluded(e, p.Excluded)
	return e.err
}

// titleField is the title: field, cut to 100 bytes; "" when there is
// no title.
func titleField(t string) string {
	if t == "" {
		return ""
	}
	return quotedField("title", ClipAround(t, 0, 100))
}

// sessionLine is grep -l's line for a session with matches: its labeled
// address, agent, last match time, repo, branch, hit count and title.
func sessionLine(s *ConversationInfo) string {
	parts := []string{field("session", s.Address), field("agent", s.Agent)}
	if s.LastActivityAt != nil {
		parts = append(parts, field("active", isoStamp(s.LastActivityAt)))
	}
	if s.Repo != "" {
		parts = append(parts, field("repo", repoName(s.Repo)))
	}
	if b := branchLabel(s.Branches); b != "" {
		parts = append(parts, field("branch", b))
	}
	parts = append(parts, field("hits", strconv.Itoa(s.Hits)))
	if t := titleField(s.Title); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, fieldSep)
}

// isoStampLayout is how a header prints a time: UTC to the minute, one
// token; since and until accept it back.
const isoStampLayout = "2006-01-02T15:04Z"

func isoStamp(t *time.Time) string {
	return t.UTC().Format(isoStampLayout)
}

// WriteSessions renders a sessions page: one line per session with its
// address, agent, last activity, repo, message count and title.
func WriteSessions(w io.Writer, s *Sessions, st Style) error {
	e := &errWriter{w: w}
	for _, n := range s.Notes {
		e.printf("[%s]\n", Clean(n))
	}
	now := st.now()
	lines, err := units(st, len(s.Sessions), func(e *errWriter, i int) {
		c := &s.Sessions[i]
		extra := []string{field("msgs", strconv.Itoa(c.Messages))}
		if c.ParentSession != "" {
			extra = append(extra, field("parent", c.ParentSession))
		} else if c.Depth > 0 {
			extra = append(extra, field("parent", "unknown"))
		}
		e.printf("%s\n", summary(c, now, extra...))
		if c.Digest != nil && c.Digest.Last != "" {
			e.printf("    last: %s\n", quoteClip(c.Digest.Last, 170))
		}
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		e.printf("%s", l)
	}
	shown, next, cut := len(lines), s.Next, ""
	if shown < len(s.Sessions) {
		next, cut = SessionCursor(s.Sessions[shown-1]), budgetNote(st)
	}
	switch {
	case len(s.Sessions) == 0:
		e.printf("[no sessions]\n")
	case next != "":
		e.printf("[%d %s shown, more follow; %snext: %s]\n", shown, plural(shown, "session", "sessions"), cut, st.cursor(next))
	default:
		e.printf("[%d %s, end of list]\n", shown, plural(shown, "session", "sessions"))
	}
	excluded(e, s.Excluded)
	return e.err
}

// WriteRead renders read's answer: a header naming the session and its
// span, then each message with its address, time and kind, the focus
// marked ">>" and its text numbered by line (the addressed line marked
// ">"), neighbours indented. Clipped text says which lines it shows and
// how to read on. The budget keeps the focus and the nearest neighbours;
// the hints name the address to go on from.
func WriteRead(w io.Writer, cx *Context, st Style) error {
	e := &errWriter{w: w}
	header := "# " + readHeader(&cx.Conversation) + "\n"
	e.printf("%s", header)
	if cx.Outline != nil || cx.OutlineMore {
		return writeOutline(e, cx, st, len(header))
	}
	// Under a budget, the header and the two "more messages" hints are
	// spent first; the focus gets the rest (its lines cut to fit) and the
	// neighbours what the focus leaves.
	room := 0
	if st.Budget > 0 {
		hint := 0
		for i := range cx.Messages {
			hint = max(hint, len(st.more(cx.Messages[i].Address, "before")))
		}
		room = max(st.Budget-len(header)-2*(hint+len("   [earlier messages: ]\n")), 1)
	}
	blocks := make([]string, len(cx.Messages))
	at := 0
	for i := range cx.Messages {
		m := &cx.Messages[i]
		if m.ID == cx.Focus {
			at = i
		}
		var b strings.Builder
		readMessage(&errWriter{w: &b}, cx, m, st, room)
		blocks[i] = b.String()
	}
	// The focus, then neighbours nearest first, alternating sides, while
	// they fit.
	lo, hi := at, at
	used := 0
	if len(blocks) > 0 {
		used = len(blocks[at])
		fits := func(n int) bool { return room <= 0 || used+n <= room }
		for grew := true; grew; {
			grew = false
			if lo > 0 && fits(len(blocks[lo-1])) {
				lo--
				used += len(blocks[lo])
				grew = true
			}
			if hi < len(blocks)-1 && fits(len(blocks[hi+1])) {
				hi++
				used += len(blocks[hi])
				grew = true
			}
		}
	}
	if len(blocks) > 0 && (cx.MoreBefore || lo > 0) {
		e.printf("   [earlier messages: %s]\n", st.more(cx.Messages[lo].Address, "before"))
	}
	for i := lo; i <= hi && i < len(blocks); i++ {
		e.printf("%s", blocks[i])
	}
	if len(blocks) > 0 && (cx.MoreAfter || hi < len(blocks)-1) {
		e.printf("   [later messages: %s]\n", st.more(cx.Messages[hi].Address, "after"))
	}
	return e.err
}

// readHeader is read's header line as labeled fields (see field): the
// full session id, agent, repo (or cwd), branch, device, user, parent,
// first and last activity (UTC), message count and, last, the title.
func readHeader(c *ConversationInfo) string {
	head := []string{field("session", c.SessionID), field("agent", c.Agent)}
	if c.Repo != "" {
		head = append(head, field("repo", c.Repo))
	} else if c.Cwd != "" {
		head = append(head, field("cwd", c.Cwd))
	}
	if b := branchLabel(c.Branches); b != "" {
		head = append(head, field("branch", b))
	}
	if c.Device != "" {
		head = append(head, field("device", c.Device))
	}
	if c.User != "" {
		head = append(head, field("user", c.User))
	}
	if c.ParentSession != "" {
		head = append(head, field("parent", c.ParentSession))
	}
	if c.StartedAt != nil {
		head = append(head, field("start", isoStamp(c.StartedAt)))
	}
	if c.LastActivityAt != nil {
		head = append(head, field("active", isoStamp(c.LastActivityAt)))
	}
	if c.Messages > 0 {
		head = append(head, field("msgs", strconv.Itoa(c.Messages)))
	}
	if t := titleField(c.Title); t != "" {
		head = append(head, t)
	}
	return strings.Join(head, fieldSep)
}

// readMessage renders one message of read's answer. room > 0 bounds the
// focus message's rendered bytes, its line-number prefixes and clip hint
// included: lines that would pass it are left for line_offset.
func readMessage(e *errWriter, cx *Context, m *Message, st Style, room int) {
	focus := m.ID == cx.Focus
	mark := "  "
	if focus {
		mark = ">>"
	}
	flags := ""
	if m.IsError {
		flags += " error"
	}
	if m.Superseded {
		flags += " superseded"
	}
	if m.OffPath {
		flags += " off-path"
	}
	head := fmt.Sprintf("%s %s  %s  %s%s\n", mark, m.Address, stamp(m.TS), kindLabel(m.Kind, m.ToolName), flags)
	e.printf("%s", head)
	text := Clean(m.Text)
	lineTo, clipped := m.LineTo, m.Clipped
	if focus {
		// The clip hint is reserved: it prints whenever a line is left out.
		hint := len(clipHint(m, st, m.Lines, m.Lines+1))
		left := room - len(head) - hint
		var body strings.Builder
		for j, l := range strings.Split(text, "\n") {
			n := m.LineFrom + j
			if m.LineFrom == 0 {
				n = j + 1
			}
			lm := " "
			if cx.Line > 0 && n == cx.Line {
				lm = ">"
			}
			line := fmt.Sprintf("%s%5d  %s\n", lm, n, l)
			if room > 0 && body.Len()+len(line) > left {
				if j == 0 {
					// A first line longer than the room is cut inside it,
					// at a rune boundary, as Cut does.
					pre := fmt.Sprintf("%s%5d  ", lm, n)
					body.WriteString(pre + clip(l, max(left-len(pre)-1, 0)) + "\n")
					lineTo = n
				} else {
					lineTo = n - 1
				}
				clipped = true
				break
			}
			body.WriteString(line)
		}
		e.printf("%s", body.String())
	} else {
		e.printf("    %s\n", strings.ReplaceAll(text, "\n", "\n    "))
	}
	if clipped {
		e.printf("%s", clipHint(m, st, lineTo, max(m.Lines, lineTo)))
	}
}

// clipHint is the line under clipped text: which lines it shows and how
// to read on.
func clipHint(m *Message, st Style, lineTo, lines int) string {
	capped := ""
	if m.StoredLen > 0 && m.StoredLen < m.TextLen {
		capped = fmt.Sprintf("; the index stores %d of its %d bytes, %s for the transcript record", m.StoredLen, m.TextLen, st.flag("--raw", "raw=true"))
	}
	from := max(m.LineFrom, 1)
	if lineTo < lines {
		return fmt.Sprintf("    [lines %d-%d of %d%s; more: %s]\n", from, lineTo, lines, capped, st.readAt(m.Address, lineTo+1))
	}
	return fmt.Sprintf("    [lines %d-%d of %d%s]\n", from, lineTo, lines, capped)
}

// grouper tracks the grouped layout's headers: a session's hits run under
// a header line; a session met again after another's hits gets a short
// header naming it.
type grouper struct {
	info map[string]*ConversationInfo
	seen map[string]bool
	cur  string
	now  time.Time
}

func newGrouper(p *Page, st Style) *grouper {
	g := &grouper{info: map[string]*ConversationInfo{}, seen: map[string]bool{}, now: st.now()}
	for i := range p.SessionInfo {
		g.info[p.SessionInfo[i].SessionID] = &p.SessionInfo[i]
	}
	return g
}

// open prints h's session header when h starts a run of its session's
// hits, and reports whether it did.
func (g *grouper) open(e *errWriter, h *Hit) bool {
	if h.SessionID == g.cur {
		return false
	}
	g.cur = h.SessionID
	switch c := g.info[h.SessionID]; {
	case g.seen[h.SessionID]:
		e.printf("## %s\n", field("session", sessionOf(h.Address)))
	case c != nil:
		e.printf("%s\n", header(c, g.now))
	default:
		parts := []string{field("session", sessionOf(h.Address))}
		if h.User != "" {
			parts = append(parts, field("who", h.User))
		}
		parts = append(parts, field("agent", h.Agent))
		if h.Repo != "" {
			parts = append(parts, field("repo", repoName(h.Repo)))
		}
		if b := branchLabel(h.Branches); b != "" {
			parts = append(parts, field("branch", b))
		}
		e.printf("## %s\n", strings.Join(parts, fieldSep))
	}
	g.seen[h.SessionID] = true
	return true
}

func hasContext(h *Hit) bool {
	for _, l := range h.Lines {
		if !l.Match {
			return true
		}
	}
	return false
}

// groupedHit prints a grep hit under its session header: ORDINAL:LINE
// kind/tool: text on its first matching line, ORDINAL:LINE: text on the
// others, ORDINAL-LINE- text for context.
func groupedHit(e *errWriter, h *Hit, sep bool) {
	if sep {
		e.printf("--\n")
	}
	ord := ordinalOf(h.Address)
	labelled := false
	prev := 0
	for _, l := range h.Lines {
		if prev > 0 && l.N > prev+1 {
			e.printf("--\n")
		}
		prev = l.N
		text := Clean(l.Text)
		switch {
		case !l.Match:
			e.printf("%s-%d- %s\n", ord, l.N, text)
		case !labelled:
			labelled = true
			e.printf("%s:%d %s: %s\n", ord, l.N, hitLabel(h), text)
		default:
			e.printf("%s:%d: %s\n", ord, l.N, text)
		}
	}
	if h.MoreLines > 0 {
		e.printf("%s: [+%d more matching %s; flopwire read %s]\n", ord, h.MoreLines, plural(h.MoreLines, "line", "lines"), h.Address)
	}
}

// writeOutline prints read --outline after the header: the full digest,
// then one line per prompt (with its time) and per tool call (indented),
// failed calls marked, spawned subagents named, within the budget; the
// footer says how to read on.
func writeOutline(e *errWriter, cx *Context, st Style, used int) error {
	var b strings.Builder
	writeDigest(&errWriter{w: &b}, &cx.Conversation)
	e.printf("%s", b.String())
	used += b.Len()
	room := 0
	if st.Budget > 0 {
		room = max(st.Budget-used-120, 1)
	}
	lines, err := units(Style{Budget: room}, len(cx.Outline), func(e *errWriter, i int) {
		o := &cx.Outline[i]
		if o.Kind != "tool_call" {
			e.printf("%s  %s  %s: %s\n", o.Address, stamp(o.TS), o.Kind, oneLine(o.Text))
			return
		}
		line := "  " + o.Address + "  " + Clean(o.Tool) + "(" + oneLine(o.Text) + ")"
		if o.Error {
			line += "  error"
		}
		if len(o.Subagents) > 0 {
			line += "  → sub " + strings.Join(o.Subagents, ", ")
		}
		e.printf("%s\n", line)
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		e.printf("%s", l)
	}
	shown := len(lines)
	next := cx.OutlineNext
	cut := ""
	if shown < len(cx.Outline) {
		next, cut = OutlineCursor(cx.Outline[shown-1]), budgetNote(st)
	}
	session := cx.Conversation.Address
	if session == "" {
		session = cx.Conversation.SessionID
	}
	switch {
	case len(cx.Outline) == 0:
		e.printf("[no prompts or tool calls]\n")
	case next != "":
		e.printf("[outline: %d %s shown, more follow; %snext: %s]\n", shown, plural(shown, "entry", "entries"), cut, st.outlineAt(session, next))
	default:
		e.printf("[outline: %d %s, end of outline]\n", shown, plural(shown, "entry", "entries"))
	}
	return e.err
}
