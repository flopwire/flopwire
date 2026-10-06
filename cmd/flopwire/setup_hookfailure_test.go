package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func receiptForTest() string {
	return "v1 " + time.Now().UTC().Format("2006-01-02T15:04:05Z") + " hook-exit 7\n"
}

func TestHookFailureReader(t *testing.T) {
	file := filepath.Join(t.TempDir(), hookFailureFile)
	valid := receiptForTest()
	for _, raw := range []string{valid, "", strings.Repeat("x", hookFailureMax+1), strings.Replace(valid, "hook-exit", "secret-body", 1), strings.Replace(valid, " 7\n", " 256\n", 1), strings.Replace(valid, " 7\n", " 0\n", 1), strings.Replace(valid, " 7\n", " 007\n", 1), strings.Replace(valid, " 7\n", " 7", 1), "v1 9999-01-01T00:00:00Z hook-exit 7\n", "v1 2026-02-30T00:00:00Z hook-exit 7\n", "v1 1960-01-01T00:00:00Z hook-exit 7\n", strings.Replace(valid, "hook-exit 7", "missing-binary 2", 1)} {
		if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got := readHookFailure(file)
		if (got != "") != (raw == valid) {
			t.Errorf("receipt %q: %q", raw, got)
		}
	}
	os.Remove(file)
	target := filepath.Join(t.TempDir(), "target")
	os.WriteFile(target, []byte(valid), 0o600)
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if got := readHookFailure(file); got != "" {
		t.Fatal(got)
	}
	os.Remove(file)
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Fatal(err)
	}
	// Keep the test bounded even if a future reader regresses to blocking open.
	done := make(chan string, 1)
	go func() { done <- readHookFailure(file) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked receipt reader")
	}
	os.Remove(file)
	if err := os.WriteFile(file, []byte(valid), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 && readHookFailure(file) != "" {
		t.Fatal("read unreadable receipt")
	}
	os.Remove(file)
	os.Mkdir(file, 0o700)
	if got := readHookFailure(file); got != "" {
		t.Fatal(got)
	}
	if got := readHookFailure(file + "-absent"); got != "" {
		t.Fatal(got)
	}
}

func TestHookFailureShim(t *testing.T) {
	for _, mode := range []string{"missing", "failure", "success", "which-missing", "which-found", "mcp", "other", "other-missing"} {
		t.Run(mode, func(t *testing.T) {
			f := newShimFixture(t)
			dir := filepath.Dir(f.cfg)
			file := filepath.Join(dir, hookFailureFile)
			args := []string{"hook", "private-argument"}
			if mode != "missing" && mode != "which-missing" && mode != "other-missing" {
				bin := f.fake(filepath.Join(f.home, "bin"))
				code := "7"
				if mode == "success" || mode == "which-found" {
					code = "0"
				}
				if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat\nprintf 'private-stderr\\n' >&2\nexit "+code+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				f.record(bin)
			}
			if strings.HasPrefix(mode, "which-") {
				args = []string{"--which"}
			}
			if mode == "mcp" {
				args = []string{"mcp"}
			}
			if strings.HasPrefix(mode, "other") {
				args = []string{"version"}
			}
			if mode == "missing" || mode == "failure" {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("v1 1970-01-01T00:00:00Z hook-exit 9\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, _, code := f.run("private-body\n", args...)
			wantCode := 1
			if mode == "success" || mode == "which-found" {
				wantCode = 0
			}
			if mode == "mcp" {
				wantCode = 7
			}
			if code != wantCode {
				t.Fatalf("exit %d, want %d", code, wantCode)
			}
			if mode == "failure" || mode == "success" || mode == "mcp" || mode == "other" {
				if out != "private-body\n" {
					t.Fatalf("stdin/stdout changed: %q", out)
				}
			} else if mode != "which-found" && out != "" {
				t.Fatalf("stdout %q", out)
			}
			raw, err := os.ReadFile(file)
			if mode != "missing" && mode != "failure" {
				if !os.IsNotExist(err) {
					t.Fatalf("unexpected receipt: %q %v", raw, err)
				}
				return
			}
			if err != nil || parseHookFailure(raw) == "" {
				t.Fatalf("receipt: %q %v", raw, err)
			}
			wantReason := "missing-binary 1\n"
			if mode == "failure" {
				wantReason = "hook-exit 7\n"
			}
			if !strings.HasSuffix(string(raw), wantReason) || strings.Contains(string(raw), "1970") {
				t.Fatalf("latest failure not recorded: %q", raw)
			}
			if bytes.Contains(raw, []byte("private")) {
				t.Fatalf("private content in %q", raw)
			}
			st, _ := os.Stat(file)
			if st.Mode().Perm() != 0o600 {
				t.Fatalf("mode %v", st.Mode())
			}
			before := string(raw)
			f.run("", "--which")
			raw, _ = os.ReadFile(file)
			if string(raw) != before {
				t.Fatal("--which changed history")
			}
			bin := f.fake(filepath.Join(f.home, "success"))
			f.record(bin)
			f.run("", "hook")
			raw, _ = os.ReadFile(file)
			if string(raw) != before {
				t.Fatal("success changed history")
			}
			matches, _ := filepath.Glob(filepath.Join(dir, "hook-failure.*"))
			if len(matches) != 0 {
				t.Fatalf("staging files leaked: %v", matches)
			}
		})
	}
}

func TestHookFailureMissingPreservesStdin(t *testing.T) {
	f := newShimFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `/bin/sh "$1" hook; result=$?; cat; exit "$result"`, "sh", codexPluginShim)
	cmd.Env = f.environ()
	cmd.Stdin = strings.NewReader("untouched-input\n")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err == nil || cmd.ProcessState.ExitCode() != 1 || out.String() != "untouched-input\n" {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestHookFailureWriteSafety(t *testing.T) {
	for _, mode := range []string{"directory-symlink", "receipt-symlink", "receipt-directory", "receipt-fifo", "group-writable", "world-writable", "unwritable", "directory-is-file"} {
		t.Run(mode, func(t *testing.T) {
			f := newShimFixture(t)
			dir := filepath.Dir(f.cfg)
			file := filepath.Join(dir, hookFailureFile)
			target := filepath.Join(t.TempDir(), "target")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.WriteFile(target, []byte("keep"), 0o600))
			if mode == "directory-symlink" {
				must(os.Symlink(filepath.Dir(target), dir))
			} else if mode == "directory-is-file" {
				must(os.WriteFile(dir, []byte("keep"), 0o600))
			} else {
				must(os.MkdirAll(dir, 0o700))
			}
			switch mode {
			case "receipt-symlink":
				must(os.Symlink(target, file))
			case "receipt-directory":
				must(os.Mkdir(file, 0o700))
			case "receipt-fifo":
				must(syscall.Mkfifo(file, 0o600))
			case "group-writable":
				must(os.Chmod(dir, 0o770))
			case "world-writable":
				must(os.Chmod(dir, 0o707))
			case "unwritable":
				if os.Getuid() == 0 {
					t.Skip("root can write directories without write permission")
				}
				must(os.Chmod(dir, 0o500))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", codexPluginShim, "hook")
			cmd.Env = f.environ()
			var out bytes.Buffer
			cmd.Stdout = &out
			err := cmd.Run()
			if err == nil || ctx.Err() != nil || cmd.ProcessState.ExitCode() != 1 || out.Len() != 0 {
				t.Fatalf("failure behavior: %v %q", err, out.String())
			}
			if got, _ := os.ReadFile(target); string(got) != "keep" {
				t.Fatalf("target changed: %q", got)
			}
			if mode == "group-writable" || mode == "world-writable" || mode == "directory-symlink" {
				if _, err := os.Lstat(file); !os.IsNotExist(err) {
					t.Fatalf("receipt written: %v", err)
				}
			}
		})
	}
}

func TestHookFailureConfigSelection(t *testing.T) {
	for _, mode := range []string{"override", "default", "xdg", "relative-xdg", "relative-override"} {
		t.Run(mode, func(t *testing.T) {
			f := newShimFixture(t)
			dir := filepath.Dir(f.cfg)
			switch mode {
			case "default", "xdg", "relative-xdg":
				f.cfg = ""
				dir = filepath.Join(f.home, ".config", "flopwire")
				if mode == "xdg" {
					base := filepath.Join(t.TempDir(), "xdg")
					f.env = []string{"XDG_CONFIG_HOME=" + base}
					dir = filepath.Join(base, "flopwire")
				}
				if mode == "relative-xdg" {
					f.env = []string{"XDG_CONFIG_HOME=relative"}
				}
				if runtime.GOOS == "darwin" {
					dir = filepath.Join(f.home, "Library", "Application Support", "flopwire")
				}
			case "relative-override":
				// Use a private cwd to exercise a config name without a slash.
				dir = t.TempDir()
				f.cfg = "config.json"
			}
			cmd := exec.Command("/bin/sh", codexPluginShim, "hook")
			cmd.Env = f.environ()
			if mode == "relative-override" {
				shim, _ := filepath.Abs(codexPluginShim)
				cmd = exec.Command("/bin/sh", shim, "hook")
				cmd.Env = f.environ()
				cmd.Dir = dir
			}
			_ = cmd.Run()
			got := readHookFailure(filepath.Join(dir, hookFailureFile))
			if mode == "relative-xdg" && runtime.GOOS != "darwin" {
				if got != "" {
					t.Fatal("receipt written for XDG config Go rejects")
				}
			} else if got == "" {
				t.Fatal("no receipt in resolved directory")
			}
		})
	}
}

func TestDevinCheckHistoricalHookFailure(t *testing.T) {
	d := newDevinFixture(t, false)
	if _, _, err := d.run(); err != nil {
		t.Fatal(err)
	}
	dir, err := configDir()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, hookFailureFile)
	raw := receiptForTest()
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, _, err := d.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	h := d.devin(rep)
	if !strings.Contains(rep.HookFailureWarning, "historical shared hook-shim failure") || !strings.Contains(rep.HookFailureWarning, "may come from Claude Code, Codex or Devin") {
		t.Fatalf("warning: %s", rep.HookFailureWarning)
	}
	var text bytes.Buffer
	writeSetupText(&text, rep)
	if strings.Count(text.String(), "historical shared hook-shim failure") != 1 {
		t.Fatalf("expected one shared warning: %s", text.String())
	}
	if h.Error != "" || hasString(h.Todo, "historical") {
		t.Fatalf("history became unhealthy: %+v", h)
	}
	got, _ := os.ReadFile(file)
	if string(got) != raw {
		t.Fatal("check changed receipt")
	}
	if _, _, err := d.run(); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(file)
	if string(got) != raw {
		t.Fatal("setup install changed receipt")
	}
}
