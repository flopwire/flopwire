package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
	"golang.org/x/term"
)

func TestAntigravityComposerRows(t *testing.T) {
	const border = "────────"
	for _, tc := range []struct {
		name, input, footer, draft string
		want                       bool
	}{
		{"empty", ">", "? for shortcuts", "", true},
		{"placeholder", "\x1b[94m>\x1b[39m \x1b[90mAuto mode\x1b[39m", "? for shortcuts", "", true},
		{"typed placeholder", "> Auto mode", "? for shortcuts", "", false},
		{"draft", "> unfinished", "? for shortcuts", "", false},
		{"busy", ">", "esc to cancel", "", false},
		{"dialog", "> Confirm", "enter Confirm", "", false},
		{"expected notice", "> " + agyWakePrompt, "    auto", agyWakePrompt, true},
		{"turn started", "> " + agyWakePrompt, "esc to cancel", agyWakePrompt, false},
		{"edited notice", "> " + agyWakePrompt + "!", "", agyWakePrompt, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := agyComposerRows(strings.Join([]string{border, tc.input, border, tc.footer, ""}, "\n"), 8, tc.draft)
			if got != tc.want {
				t.Fatalf("accepted=%v want %v", got, tc.want)
			}
		})
	}
	if uniqueAntigravityPID(map[string]int{"a": 42, "b": 42}, "a") != 0 {
		t.Fatal("ambiguous lock holder")
	}
}

// The isolated tmux test launches this executable as agy. It holds a real
// conversation lock and implements only the four composer rows we inspect.
func TestAntigravityComposerProcess(t *testing.T) {
	root := os.Getenv("FLOPWIRE_TEST_AGY_COMPOSER")
	if root == "" {
		return
	}
	lock, err := os.OpenFile(filepath.Join(root, "presence", agySession+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	fd := int(os.Stdin.Fd())
	mode, err := term.MakeRaw(fd)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Restore(fd, mode)
	w, h, err := term.GetSize(fd)
	if err != nil {
		t.Fatal(err)
	}
	draw := func(draft string) {
		border := strings.Repeat("─", w)
		footer := "? for shortcuts"
		if draft != "" {
			footer = "    auto       "
		}
		fmt.Printf("\x1b[?1049h\x1b[%d;1H%s\x1b[%d;1H> %s\x1b[%d;1H%s\x1b[%d;1H%s\x1b[%d;%dH", h-3, border, h-2, draft, h-1, border, h, footer, h-2, 3+len(draft))
	}
	draw("")
	var draft bytes.Buffer
	var input [128]byte
	for {
		n, err := os.Stdin.Read(input[:])
		if err != nil {
			return
		}
		for _, c := range input[:n] {
			if c == '\r' {
				if err := os.WriteFile(filepath.Join(root, "submitted"), draft.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				return
			}
			draft.WriteByte(c)
		}
		draw(draft.String())
	}
}

func TestAntigravityWakeInPrivateTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	root, err := os.MkdirTemp("/tmp", "fw-agy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	for _, dir := range []string{"presence", "brain"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(root, "agy")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", exe, helper).CombinedOutput(); err != nil {
		t.Fatalf("copy helper: %v %s", err, out)
	}
	socket := filepath.Join(root, "tmux")
	t.Setenv("TMUX", socket+",0,0")
	t.Setenv("FLOPWIRE_TEST_AGY_COMPOSER", root)
	cmd := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "80", "-y", "24", helper, "-test.run=^TestAntigravityComposerProcess$")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private tmux: %v %s", err, out)
	}
	defer exec.Command("tmux", "-S", socket, "kill-server").Run()
	f := newFixture(t, "-")
	f.a.cfg.AntigravityRoot = filepath.Join(root, "brain")
	var target devicebus.Session
	target.SessionID = agySession
	lock := filepath.Join(root, "presence", agySession+".lock")
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(lock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture did not open lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := f.a.wakeAntigravity(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "submitted")
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(file); err == nil && string(raw) == agyWakePrompt {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected fixed notice and Enter")
}
