package agent

import (
	"fmt"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/transcript"
)

// callerCwd reads exact local placement evidence, including subagents and
// sync-only sessions. It neither guesses a parent nor queries the server.
func (a *Agent) callerCwd(harness, session string) (string, error) {
	if harness == "" || session == "" {
		return "", fmt.Errorf("caller-context needs an exact agent and session")
	}
	p, ok := a.storedPlace(placeKey{transcript.Agent(harness), session})
	if !ok || !filepath.IsAbs(p.pl.Cwd) {
		return "", fmt.Errorf("no directory recorded for this local session")
	}
	return p.pl.Cwd, nil
}
