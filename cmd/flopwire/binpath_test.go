package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const codexPluginShim = "../../plugins/codex/flopwire/bin/flopwire-hook"

// TestHookShimsMatch: one shim for both plugins. Each harness installs a
// copy of its plugin directory, so the file cannot be a link.
func TestHookShimsMatch(t *testing.T) {
	a, err := os.ReadFile(filepath.Join(claudePluginDir, "bin", "flopwire-hook"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(codexPluginShim)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("the Codex plugin's bin/flopwire-hook differs from the Claude Code plugin's; copy %s/bin/flopwire-hook to %s", claudePluginDir, codexPluginShim)
	}
	for _, p := range []string{filepath.Join(claudePluginDir, "bin", "flopwire-hook"), codexPluginShim} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s: not executable (%v)", p, err)
		}
	}
}

// shimFixture is a machine for the shim: a home, a config directory, and
// fake flopwire binaries that log how they were run.
type shimFixture struct {
	t    *testing.T
	home string
	cfg  string // FLOPWIRE_CONFIG; "" for the default config directory
	path string // PATH
	log  string
	env  []string
}

func newShimFixture(t *testing.T) *shimFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shim is a POSIX sh script")
	}
	if knownFlopwireInstalled() {
		t.Skip("a flopwire in /opt/homebrew/bin or /usr/local/bin would be found")
	}
	d := t.TempDir()
	// A space in HOME, as in a macOS user name; the shim must quote it.
	f := &shimFixture{t: t, home: filepath.Join(d, "my home"), log: filepath.Join(d, "log"), path: "/usr/bin:/bin"}
	f.cfg = filepath.Join(d, "config dir", "config.json") // a space, as in Application Support
	return f
}

// fake writes an executable flopwire into dir that logs its name, how it
// was found and its arguments and stdin, and returns its path.
func (f *shimFixture) fake(dir string) string {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, "flopwire")
	body := "#!/bin/sh\nprintf '%s %s %s %s\\n' \"$0\" \"$FLOPWIRE_HOOK_VIA\" \"$*\" \"$(cat)\" >> '" + f.log + "'\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// age sets p's modification time d in the past: an older install.
func (f *shimFixture) age(p string, d time.Duration) {
	f.t.Helper()
	at := time.Now().Add(-d)
	if err := os.Chtimes(p, at, at); err != nil {
		f.t.Fatal(err)
	}
}

func (f *shimFixture) record(bin string) {
	f.t.Helper()
	dir := filepath.Dir(f.cfg)
	if f.cfg == "" {
		base, err := userConfigDirFor(f.home)
		if err != nil {
			f.t.Fatal(err)
		}
		dir = filepath.Join(base, "flopwire")
	}
	if _, err := recordBinary(filepath.Join(dir, binaryPathFile), bin); err != nil {
		f.t.Fatal(err)
	}
}

func (f *shimFixture) environ() []string {
	env := []string{"PATH=" + f.path, "HOME=" + f.home, "XDG_CONFIG_HOME="}
	if f.cfg != "" {
		env = append(env, "FLOPWIRE_CONFIG="+f.cfg)
	}
	return append(env, f.env...)
}

// run runs the shim with args and stdin.
func (f *shimFixture) run(stdin string, args ...string) (stdout, stderr string, code int) {
	f.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{codexPluginShim}, args...)...)
	cmd.Env = f.environ()
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	_ = cmd.Run()
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func (f *shimFixture) logged() string {
	b, _ := os.ReadFile(f.log)
	_ = os.Remove(f.log)
	return strings.TrimSpace(string(b))
}

// userConfigDirFor is os.UserConfigDir with HOME set to home and no
// XDG_CONFIG_HOME, which the shim must agree with.
func userConfigDirFor(home string) (string, error) {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support"), nil
	}
	return filepath.Join(home, ".config"), nil
}

// TestHookShimResolution: the shim runs the recorded binary when it is
// executable, else flopwire on PATH, else one in a known directory, with
// the original arguments and stdin, and says which in FLOPWIRE_HOOK_VIA.
// Go's resolveLikeShim (setup --check for opencode) agrees on each case.
func TestHookShimResolution(t *testing.T) {
	for _, c := range []struct {
		name     string
		setup    func(f *shimFixture) string // returns the binary that must run
		via      string
		defaults bool // no FLOPWIRE_CONFIG: the platform's config directory
	}{
		{"recorded beats an older PATH", func(f *shimFixture) string {
			want := f.fake(filepath.Join(f.home, "opt", "fw"))
			onPath := f.fake(filepath.Join(f.home, "onpath"))
			f.age(onPath, time.Hour)
			f.path = filepath.Dir(onPath) + ":" + f.path
			f.record(want)
			return want
		}, "recorded", false},
		// A stale recorded install (an old go install, a kept Homebrew
		// keg) loses to a newer flopwire on PATH (brew upgrade).
		{"newer PATH beats a stale recorded path", func(f *shimFixture) string {
			stale := f.fake(filepath.Join(f.home, "go", "bin"))
			f.age(stale, 24*time.Hour)
			f.record(stale)
			want := f.fake(filepath.Join(f.home, "onpath"))
			f.path = filepath.Dir(want) + ":" + f.path
			return want
		}, "path", false},
		// PATH reaches the recorded binary through a symlink (Homebrew's
		// bin/flopwire -> Cellar/...): the same file, so recorded.
		{"PATH symlink to the recorded binary", func(f *shimFixture) string {
			want := f.fake(filepath.Join(f.home, "Cellar", "flopwire", "1.0", "bin"))
			f.age(want, time.Hour)
			f.record(want)
			link := filepath.Join(f.home, "brew bin")
			if err := os.MkdirAll(link, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(want, filepath.Join(link, "flopwire")); err != nil {
				t.Fatal(err)
			}
			f.path = link + ":" + f.path
			return want
		}, "recorded", false},
		{"recorded path with spaces", func(f *shimFixture) string {
			want := f.fake(filepath.Join(f.home, "Application Support", "fw bin"))
			f.record(want)
			return want
		}, "recorded", true},
		{"recorded in the default config directory", func(f *shimFixture) string {
			want := f.fake(filepath.Join(f.home, "opt", "fw"))
			f.record(want)
			return want
		}, "recorded", true},
		{"recorded not executable", func(f *shimFixture) string {
			bad := f.fake(filepath.Join(f.home, "opt", "fw"))
			if err := os.Chmod(bad, 0o644); err != nil {
				t.Fatal(err)
			}
			f.record(bad)
			want := f.fake(filepath.Join(f.home, "onpath"))
			f.path = filepath.Dir(want) + ":" + f.path
			return want
		}, "path", false},
		{"recorded gone", func(f *shimFixture) string {
			f.record(filepath.Join(f.home, "Cellar", "flopwire", "0.1.0", "bin", "flopwire"))
			want := f.fake(filepath.Join(f.home, "onpath"))
			f.path = filepath.Dir(want) + ":" + f.path
			return want
		}, "path", false},
		{"go bin", func(f *shimFixture) string {
			f.fake(filepath.Join(f.home, ".local", "bin"))
			return f.fake(filepath.Join(f.home, "go", "bin"))
		}, "known", false},
		{"local bin", func(f *shimFixture) string {
			return f.fake(filepath.Join(f.home, ".local", "bin"))
		}, "known", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newShimFixture(t)
			if c.defaults {
				f.cfg = ""
			}
			want := c.setup(f)
			out, errb, code := f.run(`{"hook_event_name":"Stop"}`, "hook", "--socket", "/tmp/a b")
			if code != 0 || out != "" || errb != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
			}
			if got, w := f.logged(), want+" "+c.via+` hook --socket /tmp/a b {"hook_event_name":"Stop"}`; got != w {
				t.Fatalf("ran %q, want %q", got, w)
			}
			out, _, code = f.run("", "--which")
			if code != 0 || strings.TrimSpace(out) != c.via+" "+want {
				t.Fatalf("--which: exit %d, %q; want %q", code, out, c.via+" "+want)
			}
			if f.logged() != "" {
				t.Fatal("--which ran the binary")
			}
			env := map[string]string{}
			for _, kv := range f.environ() {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			if r := resolveLikeShim(func(k string) string { return env[k] }, runtime.GOOS); r.Path != want || r.Via != hookVia[c.via] {
				t.Fatalf("resolveLikeShim: %+v; the shim ran %s via %s", r, want, c.via)
			}
		})
	}
}

// TestHookShimNoBinary: nothing found: one stderr line naming the recorded
// path, the PATH searched and the fix, exit 1 (never 2), nothing on stdout.
func TestHookShimNoBinary(t *testing.T) {
	f := newShimFixture(t)
	f.record(filepath.Join(f.home, "gone", "flopwire"))
	for _, args := range [][]string{{"hook"}, {"--which"}} {
		out, errb, code := f.run(`{}`, args...)
		if code != 1 || out != "" || strings.Count(errb, "\n") != 1 {
			t.Fatalf("%v: exit %d, stdout %q, stderr %q; want exit 1 and one line", args, code, out, errb)
		}
		for _, want := range []string{filepath.Join(f.home, "gone", "flopwire"), filepath.Join(filepath.Dir(f.cfg), "binary-path"), "(" + f.path + ")", "fix: run flopwire setup"} {
			if !strings.Contains(errb, want) {
				t.Errorf("%v: stderr %q lacks %q", args, errb, want)
			}
		}
	}
	env := map[string]string{"PATH": f.path, "HOME": f.home, "FLOPWIRE_CONFIG": f.cfg}
	if r := resolveLikeShim(func(k string) string { return env[k] }, runtime.GOOS); r.Error == "" || r.Path != "" {
		t.Fatalf("resolveLikeShim found %+v", r)
	}
}

// TestRecordSelf: a command a person runs records this binary's path
// when none is recorded or the recorded binary is gone, writing only when
// it changed; hooks, the MCP server and the commands
// setup or a test run on other binaries leave it alone.
func TestRecordSelf(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	file := filepath.Join(dir, binaryPathFile)
	self, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"hook"}, {"mcp"}, {"version"}, {"probe", "tap"}, {"setup", "--check"}, {"agent", "flush"}, {"serve"}, {"nonsense"}, {}} {
		recordSelf(args)
		if _, err := os.Stat(file); err == nil {
			t.Fatalf("%v recorded the binary path", args)
		}
	}
	recordSelf([]string{"grep", "x"})
	if got := readRecordedBinary(file); got != self {
		t.Fatalf("recorded %q, want %q", got, self)
	}
	// Unchanged: no write.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	recordSelf([]string{"agent", "run"})
	if st, _ := os.Stat(file); !st.ModTime().Equal(old) {
		t.Fatal("rewrote an unchanged path")
	}
	// Another binary that still exists was recorded (the user's install,
	// and this is a scratch or CI build): left alone. Only setup replaces
	// a recorded binary that exists.
	other := filepath.Join(dir, "installed", "flopwire")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := recordBinary(file, other); err != nil {
		t.Fatal(err)
	}
	recordSelf([]string{"sessions"})
	if got := readRecordedBinary(file); got != other {
		t.Fatalf("a scratch build replaced the recorded install: %q, want %q", got, other)
	}
	// The recorded binary is gone (an upgrade moved it): refreshed.
	if err := os.WriteFile(file, []byte("/opt/homebrew/Cellar/flopwire/0.1.0/bin/flopwire\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordSelf([]string{"sessions"})
	if got := readRecordedBinary(file); got != self {
		t.Fatalf("after an upgrade: %q, want %q", got, self)
	}
	if changed, err := recordBinary(file, "/a\nb"); changed || err == nil {
		t.Fatal("recorded a path with a line break")
	}
}
