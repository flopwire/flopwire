package local

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const (
	agySession = "96323ff5-1947-4546-8118-be2e50fd50e9"
	agySecond  = "01a0711b-ba11-4321-a871-deadc0ffee00"
)

func TestAntigravityCallerRequiresUniqueAncestorLock(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "presence")
	otherDir := filepath.Join(home, "other", "presence")
	d := Detector{
		Home: home, Pid: 42,
		Getenv: func(key string) string {
			if key == "CODEX_THREAD_ID" {
				return "outer-session"
			}
			return ""
		},
		Proc: func(pid int) (int, string, bool) {
			if pid == 42 {
				return 2, "/usr/local/bin/agy", true
			}
			return 1, "codex", true
		},
	}
	for _, tc := range []struct {
		name string
		open []string
		ok   bool
	}{
		{"unique", []string{filepath.Join(dir, agySession+".lock"), filepath.Join(otherDir, agySecond+".lock")}, true},
		{"none", []string{dir, filepath.Join(otherDir, agySession+".lock")}, false},
		{"ambiguous", []string{filepath.Join(dir, agySession+".lock"), filepath.Join(dir, agySecond+".lock")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.OpenFiles = func(int) []string { return tc.open }
			got, ok := d.Detect(t.Context())
			if ok != tc.ok || ok && (got.Agent != transcript.AgentAntigravity || got.SessionID != agySession) {
				t.Fatalf("caller = %+v, found = %v", got, ok)
			}
			if d.UnderCodex() {
				t.Fatal("outer Codex was accepted through agy")
			}
		})
	}
}

func TestAntigravityHoldersFromFiles(t *testing.T) {
	dir := t.TempDir()
	path := func(id string) string { return filepath.Join(dir, id+".lock") }
	raw := fmt.Sprintf("p17\ncagy\nn%s\nn%s\np23\ncagy\nn%s\np29\ncnon-agy\nn%s\n",
		path(agySession), path(agySecond), path(agySecond), path(agySession))
	holders, ok := antigravityHoldersFromFiles(dir, []byte(raw))
	if !ok || len(holders) != 2 || holders[agySession] != 17 || holders[agySecond] != 0 {
		t.Fatalf("holders = %v, read = %v", holders, ok)
	}
}
