package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/busrender"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// startWindow is how long a SessionStart for one session and source counts
// as the same start: two hook configs that both run `flopwire hook` fire
// within milliseconds of each other, while a real second start (a resume,
// a compaction) is far later.
const startWindow = 10 * time.Second

// starts records the SessionStart hooks that took the standing
// instruction, by session and source.
type starts struct {
	mu sync.Mutex
	at map[string]time.Time
}

// claimStart reports whether this SessionStart hook is the first for the
// session and source within startWindow, and records it.
func (a *Agent) claimStart(session, source string) bool {
	s := &a.starts
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.at == nil {
		s.at = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range s.at {
		if now.Sub(t) >= startWindow {
			delete(s.at, k)
		}
	}
	k := session + "\x00" + source
	if _, ok := s.at[k]; ok {
		return false
	}
	s.at[k] = now
	return true
}

// releaseStart undoes claimStart when the hook never got the answer, so
// the other hook (or the next start) prints the instruction.
func (a *Agent) releaseStart(session, source string) {
	s := &a.starts
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.at, session+"\x00"+source)
}

// excerptBudget bounds the local index lookups for ref excerpts: they
// share the hook's 200 ms budget, and a ref without an excerpt still
// prints its address.
const excerptBudget = 50 * time.Millisecond

// refExcerpts looks up the message refs (SESSION/ORDINAL[:LINE]) of msgs
// in the local index and returns a short excerpt for each one it finds.
// Other address kinds, sessions the index does not hold or holds more
// than one match for, and lookups past excerptBudget get none.
func (a *Agent) refExcerpts(ctx context.Context, msgs []busproto.Envelope) map[string]string {
	if a.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, excerptBudget)
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
