package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/google/uuid"
)

// Native delivery uses the harness's input route, never writes harness files,
// changes permissions, launches an agent, or resumes a closed session.
func (a *Agent) wakeSupported(s devicebus.Session) bool {
	if s.Cloud {
		return false
	}
	switch s.Agent {
	case "claude":
		_, err := a.claudeWakeSocket(s.SessionID)
		return err == nil
	case "codex":
		if _, err := uuid.Parse(s.SessionID); err != nil {
			return false
		}
		// The queue CLI targets the user's existing daemon. Do not advertise
		// a wake route for a different indexed home or an absent daemon.
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return false
			}
			home = filepath.Join(userHome, ".codex")
		}
		if filepath.Clean(a.cfg.CodexHome) != filepath.Clean(home) {
			return false
		}
		fi, err := fsprobe.Stat(filepath.Join(home, "app-server-control", "app-server-control.sock"))
		if err != nil || fi.Mode()&os.ModeSocket == 0 {
			return false
		}
		_, err = exec.LookPath("codex")
		return err == nil
	}
	return false
}

func (a *Agent) claudeWakeSocket(session string) (string, error) {
	files, _ := fsprobe.Glob(filepath.Join(filepath.Dir(a.cfg.ClaudeProjects), "sessions", "*.json"))
	for _, f := range files {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil || pid <= 1 {
			continue
		}
		raw, err := fsprobe.ReadFile(f)
		if err != nil {
			continue
		}
		var v struct {
			SessionID string `json:"sessionId"`
			ProcStart string `json:"procStart"`
			Socket    string `json:"messagingSocketPath"`
		}
		if json.Unmarshal(raw, &v) != nil || v.SessionID != session || v.Socket == "" {
			continue
		}
		matches, known := a.sameProcess(pid, v.ProcStart)
		if !a.pidAlive(pid) || !known || !matches {
			continue
		}
		return v.Socket, nil
	}
	return "", fmt.Errorf("no live Claude inbox socket for session %s", session)
}

func (a *Agent) wakeSession(ctx context.Context, s devicebus.Session, text string) error {
	switch s.Agent {
	case "claude":
		socket, err := a.claudeWakeSocket(s.SessionID)
		if err != nil {
			return err
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			return err
		}
		defer conn.Close()
		deadline := time.Now().Add(3 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		conn.SetDeadline(deadline)
		return json.NewEncoder(conn).Encode(map[string]any{"type": "user", "session_id": s.SessionID, "from": "flopwire", "message": map[string]any{"role": "user", "content": text}})
	case "codex":
		if _, err := uuid.Parse(s.SessionID); err != nil {
			return err
		}
		// Queue only for a session still witnessed live. A closure racing this
		// command can leave the native item pending until manual resume.
		live, err := a.BusPresence(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, v := range live {
			if v.Agent == s.Agent && v.SessionID == s.SessionID {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("recipient ended before wake")
		}
		output, err := exec.CommandContext(ctx, "codex", "queue", "--thread", s.SessionID, "--message", text).CombinedOutput()
		if err != nil {
			return fmt.Errorf("codex queue: %w: %.500s", err, output)
		}
		return nil
	}
	return fmt.Errorf("native wake is not supported by %s", s.Agent)
}
