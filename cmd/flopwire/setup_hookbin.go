package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/flopwire/flopwire/plugins"
)

// Which flopwire binary a harness's hooks run. Every harness runs a hook
// command through a shell, so `flopwire` resolves through that shell's
// PATH, which is not the terminal's: Claude Code runs /bin/sh -c with the
// PATH it started with (a GUI-started Claude Code has the GUI's), Codex
// runs your login shell (zsh -lc), Devin runs bash -c with the PATH it took
// from your login shell. So the plugins' hooks run the shim
// bin/flopwire-hook, which tries the path flopwire setup recorded, then
// PATH, then the usual install directories (binpath.go). setup --check
// runs the installed plugin's shim the way the harness runs a hook and
// reports what it found.

// hookBinaryReport is the binary a harness's hooks run.
type hookBinaryReport struct {
	// Shim is the installed plugin's shim; empty for opencode, whose
	// plugin searches by itself.
	Shim string `json:"shim,omitempty"`
	// Shell is how the harness runs the hook command.
	Shell string `json:"shell"`
	Path  string `json:"path,omitempty"`
	// Via is how the binary was found: recorded path, PATH or known dir.
	Via   string `json:"via,omitempty"`
	Error string `json:"error,omitempty"`

	noShim bool // the installed plugin predates the shim
}

// Ways the shim finds the binary (FLOPWIRE_HOOK_VIA), and their names in
// the report.
var hookVia = map[string]string{"recorded": "recorded path", "path": "PATH", "known": "known dir"}

// hookShell is the shell a harness runs hook commands in: argv before the
// script.
func hookShell(harness string) []string {
	switch harness {
	case "codex", "devin":
		sh := os.Getenv("SHELL")
		if sh == "" || !filepath.IsAbs(sh) {
			sh = "/bin/sh"
		}
		return []string{sh, "-lc"}
	}
	return []string{"/bin/sh", "-c"}
}

// hookShellNote describes hookShell for the report.
func hookShellNote(harness string) string {
	argv := strings.Join(hookShell(harness), " ")
	switch harness {
	case "claude":
		return argv + " (Claude Code's PATH: this shell's when started here)"
	case "codex":
		return argv + " (your login shell, as Codex runs hooks)"
	case "devin":
		return argv + " (Devin takes PATH from your login shell)"
	}
	return argv
}

// resolveHookBinary runs the installed plugin's shim (shim) the way the
// harness runs a hook command and reports the binary it finds.
func resolveHookBinary(ctx context.Context, env *setupEnv, harness, shim string) *hookBinaryReport {
	r := &hookBinaryReport{Shim: shim, Shell: hookShellNote(harness)}
	if _, err := os.Stat(shim); err != nil {
		r.Error = "the installed plugin has no " + plugins.ShimPath + ": it predates the hook shim and runs flopwire from the hook shell's PATH. Fix: flopwire setup"
		r.noShim = true
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	argv := append(hookShell(harness), `/bin/sh "$0" --which`, shim)
	out, errb, err := env.run(ctx, argv[0], argv[1:]...)
	via, path, ok := parseShimWhich(out)
	if err != nil || !ok {
		msg := strings.TrimSpace(string(errb))
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:] // the shim's line comes last, after any shell noise
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		r.Error = msg
		if !strings.Contains(msg, "flopwire setup") {
			r.Error += "; fix: flopwire setup"
		}
		return r
	}
	r.Path, r.Via = path, hookVia[via]
	return r
}

// parseShimWhich reads `flopwire-hook --which`: "<via> <path>" on the last
// line (a login shell's startup files may print before it).
func parseShimWhich(out []byte) (via, path string, ok bool) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	via, path, ok = strings.Cut(lines[len(lines)-1], " ")
	if !ok || hookVia[via] == "" || !filepath.IsAbs(path) {
		return "", "", false
	}
	return via, path, true
}

// resolveLikeShim finds the binary the way the shim and the opencode
// plugin do, with getenv as the environment: for opencode, whose plugin
// runs no shell.
func resolveLikeShim(getenv func(string) string, goos string) *hookBinaryReport {
	r := &hookBinaryReport{Shell: "none: the opencode plugin starts flopwire itself (opencode's PATH: this shell's when started here)"}
	home := getenv("HOME")
	var dir string
	switch x := getenv("XDG_CONFIG_HOME"); {
	case getenv("FLOPWIRE_CONFIG") != "":
		dir = filepath.Dir(getenv("FLOPWIRE_CONFIG"))
	case goos == "darwin":
		dir = filepath.Join(home, "Library", "Application Support", "flopwire")
	case x != "" && filepath.IsAbs(x):
		dir = filepath.Join(x, "flopwire")
	default:
		dir = filepath.Join(home, ".config", "flopwire")
	}
	file := filepath.Join(dir, binaryPathFile)
	recorded := readRecordedBinary(file)
	if recorded != "" && isExecFile(recorded) {
		r.Path, r.Via = recorded, hookVia["recorded"]
		return r
	}
	for _, d := range filepath.SplitList(getenv("PATH")) {
		if p := filepath.Join(d, "flopwire"); filepath.IsAbs(d) && isExecFile(p) {
			r.Path, r.Via = p, hookVia["path"]
			return r
		}
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, "go", "bin"), filepath.Join(home, ".local", "bin")} {
		if p := filepath.Join(d, "flopwire"); isExecFile(p) {
			r.Path, r.Via = p, hookVia["known"]
			return r
		}
	}
	if recorded == "" {
		recorded = "none"
	}
	r.Error = fmt.Sprintf("no flopwire binary: the recorded path (%s, from %s) is not executable, flopwire is not on PATH (%s), and not in /opt/homebrew/bin, /usr/local/bin, ~/go/bin or ~/.local/bin; fix: run flopwire setup", recorded, file, getenv("PATH"))
	return r
}

func isExecFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
}

// applyHookBinary records hb in r: a failure becomes a todo, and a found
// binary replaces the one on PATH for comparing plugin and binary.
func applyHookBinary(ctx context.Context, env *setupEnv, r *harnessReport, hb *hookBinaryReport) *setupBinary {
	r.HookBinary = hb
	if hb == nil {
		return env.binary
	}
	switch {
	case hb.noShim:
		// Its hooks run flopwire by name: compare with the one on PATH.
		r.Todo = append(r.Todo, "update the plugin: its hooks run flopwire from the hook shell's PATH, which can differ from your terminal's, and the new plugin's shim finds the binary setup records: flopwire setup")
		return env.binary
	case hb.Error != "":
		r.Todo = append(r.Todo, "the hooks find no flopwire binary, so no message arrives: "+hb.Error)
		return nil
	}
	if env.binary != nil && env.binary.Path != "" && samePath(env.binary.Path, hb.Path) {
		return env.binary
	}
	b := pathBinary(ctx, env, hb.Path)
	return &b
}

// codexPluginRoot is Codex's installed copy of the plugin, version
// (as codex plugin list names it) first, else the newest; "" when none.
func codexPluginRoot(env *setupEnv, version string) string {
	home := codexHome(env)
	if home == "" {
		return ""
	}
	base := filepath.Join(home, "plugins", "cache", codexMarketplace, "flopwire")
	if version != "" {
		if d := filepath.Join(base, version); isDir(d) {
			return d
		}
	}
	return newestDir(base, "*", "")
}

// devinPluginRoot is Devin's installed copy of the plugin: the cache
// directory whose .claude-plugin/plugin.json names flopwire, newest
// first; "" when none.
func devinPluginRoot(env *setupEnv) string {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" || !filepath.IsAbs(data) {
		data = filepath.Join(env.home, ".local", "share")
	}
	return newestDir(filepath.Join(data, "devin", "cli", "plugins", "cache"), filepath.Join("*", "*"), devinPlugin)
}

// newestDir is the most recently modified directory matching pattern
// under base; with name set, only one whose .claude-plugin/plugin.json
// names it.
func newestDir(base, pattern, name string) string {
	matches, _ := filepath.Glob(filepath.Join(base, pattern))
	var best string
	var bestT time.Time
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || !st.IsDir() {
			continue
		}
		if name != "" {
			var man struct {
				Name string `json:"name"`
			}
			if b, err := os.ReadFile(filepath.Join(m, ".claude-plugin", "plugin.json")); err != nil || json.Unmarshal(b, &man) != nil || man.Name != name {
				continue
			}
		}
		if best == "" || st.ModTime().After(bestT) {
			best, bestT = m, st.ModTime()
		}
	}
	return best
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// shimIn is the shim inside an installed plugin directory, "" for none.
func shimIn(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(plugins.ShimPath))
}

// opencodeHookBinary is what the opencode plugin would run, from this
// process's environment.
func opencodeHookBinary() *hookBinaryReport {
	return resolveLikeShim(os.Getenv, runtime.GOOS)
}
