// Package vendorcloud reaches the coding-agent sessions that run on a
// vendor's machines: Claude Code cloud sessions and Devin cloud sessions
// (notes/message-bus/plan.md §6, decision B6). It lists a person's cloud
// sessions and pushes text into one, through the vendor's own CLI login on
// the device. Each vendor sits behind Adapter, because the routes are the
// vendors' own and partly undocumented; notes/message-bus/cloud-2026-10-03.md
// records the request and response shapes as verified.
//
// The device agent (internal/devicebus) lists sessions for presence and
// pushes a message only while the session reports a running turn. Text
// pushed this way reaches the model as the account owner's own input, with
// no mark of where it came from; the caller frames it
// (busrender.CloudContext).
package vendorcloud

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Session is one cloud session as its vendor lists it.
type Session struct {
	// Agent is the harness, as transcripts name it ("claude", "devin").
	Agent string
	// ID is the id the vendor's CLI takes to reach the session.
	ID    string
	Title string
	// Repo is the repository the session works on ("owner/name"), when the
	// vendor names one; Branch its current branch.
	Repo   string
	Branch string
	// Running: a turn is running now, so a push arrives inside it.
	Running bool
}

// Pushed is the outcome of a push the vendor took.
type Pushed struct {
	// ReadAt is when the vendor's record showed the model working after
	// the pushed text, with the text in its context; zero when the push
	// did not see it (Reader may find it later).
	ReadAt time.Time
}

// Adapter is one vendor's route to its cloud sessions.
type Adapter interface {
	// Agent is the harness name of the vendor's sessions.
	Agent() string
	// List returns the device person's cloud sessions that can take a
	// message: not archived, not exited.
	List(ctx context.Context) ([]Session, error)
	// Push sends text into the session as its owner's input. It returns
	// once the vendor recorded the text in the session. ErrGone: the
	// session is archived or exited.
	Push(ctx context.Context, id, text string) (Pushed, error)
}

// Reader is an Adapter that can read a session's record afterwards.
type Reader interface {
	// Seen reports, for each message id whose wrapper
	// (<flopwire-message id="…") the session's record holds as pushed
	// input, when the model first worked with it in context: the first
	// model output recorded after that input. Ids not found are left out.
	Seen(ctx context.Context, session string, ids []string) (map[string]time.Time, error)
}

// ErrGone is a push to a session the vendor reports archived or exited.
var ErrGone = errors.New("vendorcloud: the cloud session is archived or exited")

// Default returns an adapter for each vendor whose CLI is installed on the
// device. FLOPWIRE_CLOUD=off returns none.
func Default() []Adapter {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("FLOPWIRE_CLOUD"))); v == "off" || v == "0" || v == "false" {
		return nil
	}
	var out []Adapter
	if p, err := exec.LookPath("claude"); err == nil {
		out = append(out, NewClaude(p))
	}
	if p, err := exec.LookPath("devin"); err == nil {
		out = append(out, NewDevin(p))
	}
	return out
}

// wrapperIDs returns the message ids of the wrappers that open a line of
// text: `<flopwire-message id="ID"`. Text a model quotes mid-line does not
// count.
func wrapperIDs(text string) []string {
	const open = `<flopwire-message id="`
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		rest, ok := strings.CutPrefix(line, open)
		if !ok {
			continue
		}
		if id, _, ok := strings.Cut(rest, `"`); ok && id != "" {
			out = append(out, id)
		}
	}
	return out
}
