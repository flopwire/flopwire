package upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RestartLaunchd restarts the launchd device agent (com.flopwire.agent)
// when it is loaded and runs binary; it reports what it did, or what to
// fix when the agent runs another binary. No loaded agent is not an
// error.
func RestartLaunchd(ctx context.Context, binary string) (string, error) {
	svc := fmt.Sprintf("gui/%d/com.flopwire.agent", os.Getuid())
	out, err := exec.CommandContext(ctx, "launchctl", "print", svc).Output()
	if err != nil {
		return "", nil // not loaded
	}
	program := ""
	for _, line := range strings.Split(string(out), "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "program = "); ok {
			program = strings.TrimSpace(p)
			break
		}
	}
	if program != "" && program != binary {
		return fmt.Sprintf("the device agent (launchd com.flopwire.agent) runs %s, not %s; point its plist at the new binary and run launchctl kickstart -k %s", program, binary, svc), nil
	}
	if err := exec.CommandContext(ctx, "launchctl", "kickstart", "-k", svc).Run(); err != nil {
		return "", fmt.Errorf("launchctl kickstart -k %s: %w", svc, err)
	}
	return "restarted the device agent (launchd com.flopwire.agent)", nil
}
