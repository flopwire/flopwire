package agent

import (
	"context"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Read receipts (issue #65). A message the hook printed enters the
// session's context as hook context, which each harness records in the
// transcript (transcript.HookContext). When the agent indexes such a row,
// the wrappers in it are sightings of those messages by that session: the
// devicebus marks the delivered ones read and reports them to the server.
//
// Only hook context counts. A prompt, a reply, a tool call or a tool
// result can carry any text, a wrapper included (an agent reading another
// session's transcript sees its wrappers in tool output), so those rows
// are never looked at.

// wrapperStart begins every message `flopwire hook` prints
// (busrender.Render). A body cannot contain it: busrender escapes "<".
const wrapperStart = `<flopwire-message id="`

// hookOutputStarts begin the text of one hook's output: the standing
// instruction (session start) or the first message.
var hookOutputStarts = []string{"<flopwire-instructions>", wrapperStart}

// wrapperIDs returns the ids of the message wrappers in hook output, in
// order: each wrapperStart at the start of a line, its id up to the
// closing quote. An id with a character a message id never has is
// skipped.
func wrapperIDs(text string) []string {
	var out []string
	for rest, at := text, 0; ; {
		i := strings.Index(rest, wrapperStart)
		if i < 0 {
			return out
		}
		lineStart := at+i == 0 || text[at+i-1] == '\n'
		rest, at = rest[i+len(wrapperStart):], at+i+len(wrapperStart)
		end := strings.IndexByte(rest, '"')
		if !lineStart || end <= 0 || end > 64 {
			continue
		}
		if id := rest[:end]; validID(id) {
			out = append(out, id)
		}
	}
}

func validID(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return id != ""
}

// readSightings collects the sightings of one parse.
type readSightings struct {
	agent transcript.Agent
	reads []devicebus.Read
}

// note records the wrappers of a hook context row. The kind is checked
// before the text, so no other row's text is searched.
func (s *readSightings) note(m *transcript.Message) {
	if !transcript.HookContext(s.agent, m) || !strings.Contains(m.Text, wrapperStart) {
		return
	}
	if s.agent == transcript.AgentDevin && !startsWithAny(m.Text, hookOutputStarts) {
		// Devin's own system rows (system info, skills) are system rows
		// too: only one that is the hook's output counts.
		return
	}
	at := m.TS
	if at.IsZero() {
		at = time.Now()
	}
	for _, id := range wrapperIDs(m.Text) {
		s.reads = append(s.reads, devicebus.Read{Session: m.SessionID, Agent: string(s.agent), ID: id, At: at})
	}
}

func startsWithAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// markRead hands a parse's sightings to the message bus, after the parse
// was saved. A failure is logged: the sightings are not parsed again, and
// the messages stay delivered (never wrongly read).
func (a *Agent) markRead(ctx context.Context, s *readSightings) {
	if len(s.reads) == 0 {
		return
	}
	if a.onReads != nil {
		a.onReads(s.reads)
		return
	}
	if a.cfg.Bus == nil {
		return
	}
	if err := a.cfg.Bus.MarkRead(ctx, s.reads); err != nil && ctx.Err() == nil {
		a.log.Warn("agent: read receipts", "err", err)
	}
}
