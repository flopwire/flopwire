package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// hookStart is when the hook that sent req started (its HookStart), else
// now.
func hookStart(req Request, now time.Time) time.Time {
	if req.HookStart > 0 {
		return time.UnixMilli(req.HookStart)
	}
	return now
}

// hookLifecycle tells the bus what a hook event says about its session's
// life: SessionEnd ends it (devicebus.Bus.End); any other event of a hook
// that started after a recorded end means it was resumed (Revive). The
// hook's own start time decides, not the request's arrival: a Stop hook
// of a `claude -p` can reach the agent after its SessionEnd.
func (a *Agent) hookLifecycle(ctx context.Context, req Request) {
	if a.cfg.Bus == nil || req.Session == "" {
		return
	}
	at := hookStart(req, a.now())
	var err error
	switch req.Event {
	case "SessionEnd":
		err = a.cfg.Bus.End(ctx, devicebus.Ref{Agent: req.Agent, Session: req.Session}, at)
	case "SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop":
		err = a.cfg.Bus.Revive(ctx, req.Session, at)
	}
	if err != nil {
		a.log.Warn("agent: session end", "session", req.Session, "event", req.Event, "err", err)
	}
}

// hookBusyCap bounds how long a session counts as busy after the hook
// event that began or continued its turn: a turn that ended without a Stop
// hook (the harness was interrupted or killed) must not stay busy forever.
// A single tool call longer than this reads as idle until the next event.
const hookBusyCap = 15 * time.Minute

// hookTurns is each session's last hook event: UserPromptSubmit, PreToolUse
// and PostToolUse mean a turn is running; Stop, SessionStart and SessionEnd
// mean none is. Every event reaches the agent as the flush request of
// `flopwire hook`, so this costs nothing extra. Only Devin's presence uses
// it: Devin has no other read-only signal of a running turn.
type hookTurns struct {
	mu sync.Mutex
	m  map[string]hookTurn
}

type hookTurn struct {
	busy bool
	at   time.Time
}

// noteHookEvent records a session's hook event.
func (a *Agent) noteHookEvent(session, event string) {
	var busy bool
	switch event {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse":
		busy = true
	case "Stop", "SessionStart", "SessionEnd":
	default:
		return
	}
	if session == "" {
		return
	}
	now := a.now()
	t := &a.turns
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]hookTurn{}
	}
	for k, v := range t.m {
		if now.Sub(v.at) > hookBusyCap {
			delete(t.m, k)
		}
	}
	t.m[session] = hookTurn{busy: busy, at: now}
}

// hookBusy reports whether the session's last hook event says a turn is
// running, within hookBusyCap.
func (a *Agent) hookBusy(session string) bool {
	t := &a.turns
	t.mu.Lock()
	defer t.mu.Unlock()
	v, ok := t.m[session]
	return ok && v.busy && a.now().Sub(v.at) <= hookBusyCap
}

// ExcerptBudget bounds the local index lookups for ref excerpts: they
// share the hook's 200 ms budget, and a ref without an excerpt still
// prints its address. Tests under the race detector widen it.
var ExcerptBudget = 50 * time.Millisecond

// refExcerpts looks up the message refs (SESSION/ORDINAL[:LINE]) of msgs
// in the local index and returns a short excerpt for each one it finds.
// Other address kinds, sessions the index does not hold or holds more
// than one match for, and lookups past ExcerptBudget get none.
func (a *Agent) refExcerpts(ctx context.Context, msgs []busproto.Envelope) map[string]string {
	if a.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, ExcerptBudget)
	defer cancel()
	var out map[string]string
	for _, m := range msgs {
		for i, ref := range m.Refs {
			if i == busrender.MaxRefs || ctx.Err() != nil {
				break
			}
			if _, done := out[ref]; done {
				continue
			}
			if ex := a.excerpt(ctx, ref); ex != "" {
				if out == nil {
					out = map[string]string{}
				}
				out[ref] = ex
			}
		}
	}
	return out
}

func (a *Agent) excerpt(ctx context.Context, ref string) string {
	addr, err := format.ParseAddress(ref)
	if err != nil || addr.Kind != format.AddrMessage || len(addr.Session) < format.MinPrefix/2 {
		return ""
	}
	ids, err := a.store.SessionsWithPrefix(ctx, addr.Session, 2)
	if err != nil || len(ids) != 1 {
		return ""
	}
	row, err := a.store.MessageByOrdinal(ctx, ids[0], addr.Ordinal)
	if err != nil || row == nil || row.Text == "" {
		return ""
	}
	text := row.Text
	if addr.Line > 0 {
		lines := strings.Split(text, "\n")
		if addr.Line > len(lines) {
			return ""
		}
		text = lines[addr.Line-1]
	}
	text = strings.Join(strings.Fields(format.Clean(text)), " ")
	if len(text) > 4*busrender.MaxExcerptBytes {
		text = format.ClipAround(text, 0, 4*busrender.MaxExcerptBytes)
	}
	if row.Role != "" {
		text = row.Role + ": " + text
	}
	return text
}
