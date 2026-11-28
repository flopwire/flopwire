package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Only this fixed line enters the terminal. The existing hook carries the envelope.
const agyWakePrompt = "Check your Flopwire inbox."
const paneIdentity = "#{pane_pid} #{pane_current_command} #{pane_dead} #{pane_in_mode} #{synchronize-panes} #{alternate_on} #{cursor_flag}"

func wakeTmux(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tmux", append([]string{"-N"}, args...)...)
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Output()
}

// The PID must be the program tmux launched directly into the pane. A
// shell that runs agy and could regain its prompt cannot receive wake keys.
func antigravityPanes(ctx context.Context) map[int]string {
	panes := map[int]string{}
	raw, err := wakeTmux(ctx, "list-panes", "-a", "-F", "#{pane_id} "+paneIdentity)
	if err != nil {
		return panes
	}
	for _, row := range strings.Split(string(raw), "\n") {
		f := strings.Fields(row)
		if len(f) != 8 || strings.Join(f[2:7], " ") != "agy 0 0 0 1" || !strings.HasPrefix(f[0], "%") {
			continue
		}
		if pid, err := strconv.Atoi(f[1]); err == nil && pid > 1 {
			panes[pid] = f[0]
		}
	}
	return panes
}

var agyPlaceholder = regexp.MustCompile(`^\x1b\[94m>\x1b\[39m \x1b\[90m[^\x1b]+(?:\x1b\[39m)?$`)
var agySGR = regexp.MustCompile("\x1b\\[[0-9;]*m")
var errAgyComposer = errors.New("Antigravity pane is not at the expected composer")

// Capture only the four bottom rows. A draft may place the cursor at its
// start, so cursor position alone is insufficient. The placeholder is dim,
// unlike a draft containing the same words. Unknown layouts fail closed.
func checkAgyComposer(ctx context.Context, pane string, pid int, draft string) (string, error) {
	format := paneIdentity + " #{cursor_x} #{cursor_y} #{pane_width} #{pane_height}"
	raw, err := wakeTmux(ctx, "display-message", "-p", "-t", pane, format)
	if err != nil {
		return "", err
	}
	state := strings.TrimSpace(string(raw))
	var actual, x, y, w, h int
	var cmd string
	var dead, mode, sync, alt, cursor int
	_, err = fmt.Sscan(state, &actual, &cmd, &dead, &mode, &sync, &alt, &cursor, &x, &y, &w, &h)
	if err != nil || actual != pid || cmd != "agy" || dead != 0 || mode != 0 || sync != 0 || alt != 1 || cursor != 1 {
		return "", errAgyComposer
	}
	if h < 4 || y != h-3 || x != 2+len(draft) || w < 32 || w > 4096 {
		return "", errAgyComposer
	}
	raw, err = wakeTmux(ctx, "capture-pane", "-p", "-e", "-t", pane, "-S", strconv.Itoa(h-4), "-E", strconv.Itoa(h-1))
	if err != nil {
		return "", err
	}
	if !agyComposerRows(string(raw), w, draft) {
		return "", errAgyComposer
	}
	// Recheck the identity, mode, and cursor in tmux's command queue at
	// dispatch. Never submit after a pane replacement, resize or mode change.
	return "#{==:" + format + "," + state + "}", nil
}

func agyComposerRows(raw string, width int, draft string) bool {
	rows := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(rows) != 4 {
		return false
	}
	border := strings.Repeat("─", width)
	if agySGR.ReplaceAllString(rows[0], "") != border || agySGR.ReplaceAllString(rows[2], "") != border {
		return false
	}
	line := agySGR.ReplaceAllString(rows[1], "")
	footer := agySGR.ReplaceAllString(rows[3], "")
	if draft != "" {
		return line == "> "+draft && strings.HasPrefix(footer, " ")
	}
	if !strings.HasPrefix(footer, "? for shortcuts") {
		return false
	}
	return line == ">" || agyPlaceholder.MatchString(rows[1])
}

func guardedAgyKeys(ctx context.Context, pane, guard, keys string) error {
	// Both branches run inside tmux. Only a positive marker confirms dispatch.
	out, err := wakeTmux(ctx, "if-shell", "-F", "-t", pane, guard, "send-keys -t "+pane+" "+keys+" ; display-message -p submitted", "display-message -p changed")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "submitted" {
		return errAgyComposer
	}
	return nil
}

func (a *Agent) wakeAntigravity(ctx context.Context, s devicebus.Session) error {
	if busy, _ := a.hookBusy(transcript.AgentAntigravity, s.SessionID); busy {
		return errAgyComposer
	}
	// Bypass the presence cache when issuing actual input.
	var live local.AntigravityPresence
	holders, ok := live.Holders(ctx, a.cfg.AntigravityRoot)
	pid := uniqueAntigravityPID(holders, s.SessionID)
	if !ok || pid <= 1 {
		return errors.New("Antigravity conversation lock is no longer held")
	}
	pane := antigravityPanes(ctx)[pid]
	if pane == "" {
		return errors.New("Antigravity process has no eligible tmux pane")
	}
	guard, err := checkAgyComposer(ctx, pane, pid, "")
	if err != nil {
		return err
	}
	if err := guardedAgyKeys(ctx, pane, guard, "-l '"+agyWakePrompt+"'"); err != nil {
		return err
	}
	// The TUI renders asynchronously. Submit only after seeing our exact
	// notice as the entire draft, with the cursor at its end.
	for i := 0; i < 12; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		guard, err = checkAgyComposer(ctx, pane, pid, agyWakePrompt)
		if err == nil {
			return guardedAgyKeys(ctx, pane, guard, "Enter")
		}
	}
	return errAgyComposer
}

func uniqueAntigravityPID(holders map[string]int, session string) int {
	pid := holders[session]
	for id, other := range holders {
		if id != session && other == pid {
			return 0
		}
	}
	return pid
}
