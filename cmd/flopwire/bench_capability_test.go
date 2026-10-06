package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
			for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "FLOPWIRE_CONFIG", "FLOPWIRE_OPENCODE_DB", "OPENCODE_DB", "XDG_STATE_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "FLOPWIRE_TOKEN", "FLOPWIRE_SERVER", "FLOPWIRE_FINGERPRINT", "FLOPWIRE_CLOUD"} {
				t.Setenv(k, "/do-not-use-user-path")
			}
			flag := "db"
			if supporting {
				flag = "opencode-db"
			}
			exe := fakeBenchBinary(t, `printf '%s\n' "$*" >> "$BENCH_LAUNCH_LOG"
[ "$FLOPWIRE_CLOUD" = off ] || exit 91
case "$HOME:$XDG_CONFIG_HOME:$XDG_DATA_HOME:$FLOPWIRE_CONFIG:$FLOPWIRE_OPENCODE_DB:$OPENCODE_DB:$XDG_STATE_HOME:$CLAUDE_CONFIG_DIR:$CODEX_HOME:$FLOPWIRE_TOKEN:$FLOPWIRE_SERVER:$FLOPWIRE_FINGERPRINT" in
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

// The --once fake installs a real SQLite index before the idle launch fails.
func fakeBenchIdleBinary(t *testing.T, idleBody string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.db")
	db, err := sql.Open("sqlite", fixture)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE messages (id INTEGER PRIMARY KEY, content TEXT); INSERT INTO messages VALUES (1, 'synthetic benchmark message')")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "launches")
	// Paths are passed through environment variables, never interpreted as shell code.
	t.Setenv("BENCH_IDLE_FIXTURE", fixture)
	t.Setenv("BENCH_IDLE_LOG", log)
	exe := fakeBenchBinary(t, `if [ "$3" = '-h' ]; then
 echo 'Usage of agent run:' >&2
 echo '  -opencode-db string' >&2
 exit 2
fi
printf '%s\n' "$*" >> "$BENCH_IDLE_LOG"
once=false
while [ "$#" -gt 0 ]; do
 case "$1" in
 --db) shift; db="$1";;
 --once) once=true;;
 esac
 shift
done
if [ "$once" = true ]; then
 cp "$BENCH_IDLE_FIXTURE" "$db" || exit 92
 echo 'agent: pass done' >&2
 exit 0
fi
`+idleBody)
	return exe, log
}

func TestBenchIndexIdleExit(t *testing.T) {
	for _, code := range []int{2, 0} {
		t.Run(fmt.Sprintf("exit%d", code), func(t *testing.T) {
			exe, log := fakeBenchIdleBinary(t, fmt.Sprintf("echo '%s' >&2\necho 'flag provided but not defined: -unrelatedflag' >&2\nexit %d\n", strings.Repeat("x", 3000), code))
			b := &bench{exe: exe, scratch: t.TempDir(), opencodeDB: true}
			result, err := b.index(context.Background(), 200*time.Millisecond)
			if err == nil || result != nil || !strings.Contains(err.Error(), "agent idle") || !strings.Contains(err.Error(), "-unrelatedflag") || len(err.Error()) > 2100 {
				t.Fatalf("idle exit accepted or missing diagnostic: result=%+v err=%v", result, err)
			}
			db, err := sql.Open("sqlite", b.indexPath())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var rows int
			if err := db.QueryRow("SELECT count(*) FROM messages").Scan(&rows); err != nil || rows != 1 {
				t.Fatalf("--once fixture: rows=%d err=%v", rows, err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "\n") != 2 {
				t.Fatalf("measured launch retried: %s", data)
			}
		})
	}
}

func TestBenchIndexIdleShutdown(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		parentCancel bool
	}{
		{"signal", "exec sleep 5\n", false},
		{"graceful", "trap 'exit 0' TERM\nwhile :; do sleep 0.01; done\n", false},
		{"parentDeadline", "exec sleep 5\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exe, _ := fakeBenchIdleBinary(t, "echo 'agent: sweep cpu=1ms files=1' >&2\necho 'agent: sweep cpu=2ms files=1' >&2\n"+tc.body)
			b := &bench{exe: exe, scratch: t.TempDir(), opencodeDB: true}
			ctx := context.Background()
			idleAfter := 200 * time.Millisecond
			if tc.parentCancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
				idleAfter = 5 * time.Second
			}
			result, err := b.index(ctx, idleAfter)
			if tc.parentCancel {
				if !errors.Is(err, context.DeadlineExceeded) || result != nil {
					t.Fatalf("cancellation: result=%+v err=%v", result, err)
				}
			} else if err != nil || result == nil {
				t.Fatalf("planned shutdown: result=%+v err=%v", result, err)
			} else if len(result.SweepCPUMs) != 1 || result.SweepCPUMs[0] != 2 {
				t.Fatalf("sweeps were not drained: %+v", result)
			}
		})
	}
}

func TestBenchIndexIdleInheritedStderr(t *testing.T) {
	for _, mode := range []string{"exit", "signal", "parentDeadline"} {
		t.Run(mode, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "descendant.pid")
			t.Setenv("BENCH_IDLE_DESCENDANT_PID", pidFile)
			// Kill only the descendant this fixture records. In particular, the
			// harness must return while that process still holds stderr open.
			t.Cleanup(func() {
				data, err := os.ReadFile(pidFile)
				if err != nil {
					t.Error(err)
					return
				}
				pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
				if err != nil || pid <= 0 {
					t.Errorf("invalid fixture PID %q: %v", data, err)
					return
				}
				process, err := os.FindProcess(pid)
				if err == nil {
					err = process.Kill()
				}
				if err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("kill fixture descendant %d: %v", pid, err)
				}
			})
			body := `sleep 30 &
echo "$!" > "$BENCH_IDLE_DESCENDANT_PID"
echo 'agent: sweep cpu=1ms files=1' >&2
echo 'agent: sweep cpu=2ms files=1' >&2
`
			if mode == "exit" {
				body += "echo '" + strings.Repeat("x", 3000) + "' >&2\necho 'inherited stderr failure' >&2\nexit 2\n"
			} else {
				body += "exec sleep 30\n"
			}
			exe, _ := fakeBenchIdleBinary(t, body)
			b := &bench{exe: exe, scratch: t.TempDir(), opencodeDB: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			idleAfter := 10 * time.Second
			if mode == "signal" {
				idleAfter = 500 * time.Millisecond
			} else if mode == "parentDeadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 500*time.Millisecond)
				defer deadlineCancel()
			}
			type outcome struct {
				result *indexResult
				err    error
			}
			done := make(chan outcome, 1)
			start := time.Now()
			go func() {
				result, err := b.index(ctx, idleAfter)
				done <- outcome{result, err}
			}()
			select {
			case got := <-done:
				t.Logf("returned after %s", time.Since(start))
				switch mode {
				case "exit":
					var exit interface{ ExitCode() int }
					if got.result != nil || !errors.As(got.err, &exit) || exit.ExitCode() != 2 || !strings.Contains(got.err.Error(), "inherited stderr failure") || len(got.err.Error()) > 2100 {
						t.Fatalf("exit status/tail: result=%+v err=%v", got.result, got.err)
					}
				case "signal":
					if got.err != nil || got.result == nil || len(got.result.SweepCPUMs) != 1 || got.result.SweepCPUMs[0] != 2 {
						t.Fatalf("planned SIGTERM/sweep drain: result=%+v err=%v", got.result, got.err)
					}
				case "parentDeadline":
					if got.result != nil || !errors.Is(got.err, context.DeadlineExceeded) {
						t.Fatalf("parent deadline: result=%+v err=%v", got.result, got.err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("index did not return within 5s with inherited stderr")
			}
		})
	}
}

func TestBenchABIdleExit(t *testing.T) {
	for _, side := range []string{"A", "B"} {
		t.Run(side, func(t *testing.T) {
			failing, log := fakeBenchIdleBinary(t, "echo 'flag provided but not defined: -unrelatedflag' >&2\nexit 2\n")
			healthy := fakeBenchBinary(t, `case "$*" in
 *--sweep*) exec sleep 5;;
 *) exec "`+failing+`" "$@";;
esac
`)
			a, b := failing, healthy
			if side == "B" {
				a, b = healthy, failing
			}
			dir := t.TempDir()
			var out bytes.Buffer
			err := benchAB(context.Background(), []string{"--a", a, "--b", b, "--home", filepath.Join(dir, "home"), "--scratch", filepath.Join(dir, "scratch"), "--out", dir, "--only", "index", "--runs", "1", "--warm=false", "--idle-after", "200ms"}, &out)
			if side == "A" {
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
				if sum.Verdict != verdictBaselineFailed || !strings.Contains(sum.Error, "-unrelatedflag") {
					t.Fatalf("summary: %+v", sum)
				}
			} else if err == nil || !strings.Contains(err.Error(), "-unrelatedflag") {
				t.Fatalf("candidate failure: %v", err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			want := 2
			if side == "B" {
				want = 3
			} // A's --once, B's --once and failed idle.
			if strings.Count(string(data), "\n") != want {
				t.Fatalf("measured launch retried: %s", data)
			}
		})
	}
}
