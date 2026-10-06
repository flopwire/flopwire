package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeBenchBinary(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBenchAgentCapability(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		want       bool
	}{
		{"legacy", "db", false}, {"supporting", "opencode-db", true},
		{"lookalike", "opencode-db-extra", false}, {"prefix", "opencode", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe := fakeBenchBinary(t, "echo 'Usage of agent run:' >&2\necho '  -"+tc.flag+" string' >&2\necho '    description mentions -opencode-db' >&2\nexit 2\n")
			b := &bench{exe: exe, scratch: t.TempDir()}
			got, err := b.probeAgent(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("probe = %v, %v", got, err)
			}
		})
	}
}

func TestBenchAgentProbeFailure(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty", "exit 0\n"}, {"malformed", "echo opencode-db\nexit 2\n"},
		{"flagFailureWithUsage", "echo 'flag provided but not defined: -h'\necho 'Usage of agent run:'\necho '  -db string'\nexit 2\n"},
		{"timeout", "exec sleep 5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &bench{exe: fakeBenchBinary(t, tc.body), scratch: t.TempDir()}
			if _, err := b.probeAgentTimeout(context.Background(), 30*time.Millisecond); err == nil {
				t.Fatal("probe accepted failure")
			}
		})
	}
	b := &bench{exe: filepath.Join(t.TempDir(), "missing"), scratch: t.TempDir()}
	if _, err := b.probeAgent(context.Background()); err == nil {
		t.Fatal("probe accepted failed start")
	}
}

func TestBenchCapabilityLaunches(t *testing.T) {
	for _, supporting := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "supporting"}[supporting], func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "launches")
			t.Setenv("BENCH_LAUNCH_LOG", log)
			for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "FLOPWIRE_CONFIG", "FLOPWIRE_OPENCODE_DB", "OPENCODE_DB"} {
				t.Setenv(k, "/do-not-use-user-path")
			}
			flag := "db"
			if supporting {
				flag = "opencode-db"
			}
			exe := fakeBenchBinary(t, `printf '%s\n' "$*" >> "$BENCH_LAUNCH_LOG"
case "$HOME:$XDG_CONFIG_HOME:$XDG_DATA_HOME:$FLOPWIRE_CONFIG:$FLOPWIRE_OPENCODE_DB:$OPENCODE_DB" in
 *do-not-use-user-path*) exit 90;;
esac
if [ "$3" = '-h' ]; then
 echo 'Usage of agent run:' >&2
 echo '  -`+flag+` string' >&2
 exit 2
fi
case "$*" in
 *--once*) exit 0;;
 *--sweep*) exec sleep 5;;
 *'agent run'*) exec sleep 5;;
esac
exit 0
`)
			out := filepath.Join(dir, "out")
			var buf bytes.Buffer
			err := benchAB(context.Background(), []string{"--a", exe, "--b", exe, "--home", filepath.Join(dir, "corpus"), "--scratch", filepath.Join(dir, "scratch"), "--out", out, "--only", "index", "--runs", "2", "--warm=false", "--idle-after", "1ms"}, &buf)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 9 || strings.Count(string(data), "agent run -h") != 1 {
				t.Fatalf("expected one probe and eight launches: %s", data)
			}
			for _, line := range lines[1:] {
				if strings.Contains(line, "--opencode-db -") != supporting {
					t.Fatalf("wrong flag: %s", line)
				}
			}
			// Reach the real freshness launch on a synthetic transcript, then cancel
			// before append measurements. This checks its separate argument path.
			projects := filepath.Join(dir, "projects", "project")
			if err := os.MkdirAll(projects, 0700); err != nil {
				t.Fatal(err)
			}
			transcript := `{"sessionId":"12345678-1234-1234-1234-123456789abc","cwd":"/synthetic"}` + "\n" + strings.Repeat(" ", 1<<20) + "\n"
			if err := os.WriteFile(filepath.Join(projects, "12345678-1234-1234-1234-123456789abc.jsonl"), []byte(transcript), 0600); err != nil {
				t.Fatal(err)
			}
			b := &bench{exe: exe, scratch: filepath.Join(dir, "fresh-scratch"), claude: filepath.Dir(projects), opencodeDB: supporting}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := b.fresh(ctx); err == nil {
				t.Fatal("expected canceled initial index")
			}
			data, err = os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			lines = strings.Split(strings.TrimSpace(string(data)), "\n")
			line := lines[len(lines)-1]
			if !strings.Contains(line, "--devin-db -") || strings.Contains(line, "--opencode-db -") != supporting {
				t.Fatalf("freshness args: %s", line)
			}
		})
	}
}

func TestBenchCapabilityUnrelatedFailure(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launches")
	t.Setenv("BENCH_LAUNCH_LOG", log)
	exe := fakeBenchBinary(t, `printf '%s\n' "$*" >> "$BENCH_LAUNCH_LOG"
if [ "$3" = '-h' ]; then
 echo 'Usage of agent run:' >&2
 echo '  -db string' >&2
 exit 2
fi
echo 'flag provided but not defined: -no-sync' >&2
exit 2
`)
	dir := t.TempDir()
	var buf bytes.Buffer
	err := benchAB(context.Background(), []string{"--a", exe, "--b", exe, "--scratch", filepath.Join(dir, "scratch"), "--out", dir, "--home", filepath.Join(dir, "home"), "--only", "index", "--warm=false"}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "ab.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sum abSummary
	if err := json.Unmarshal(data, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Verdict != verdictBaselineFailed || !strings.Contains(sum.Error, "-no-sync") {
		t.Fatalf("summary: %+v", sum)
	}
	data, err = os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "\n") != 2 {
		t.Fatalf("measured launch retried: %s", data)
	}
}
