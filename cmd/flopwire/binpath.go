package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The recorded binary path: the plugins' hooks run flopwire through a shim
// (plugins/*/flopwire/bin/flopwire-hook) that reads the absolute path of
// the flopwire binary from <config dir>/binary-path before it falls back
// to PATH, because a harness runs hooks through a shell whose PATH is not
// the user's terminal PATH (Codex: a login shell; a harness started from
// the GUI: the GUI's PATH). flopwire setup writes the file. Another
// command a person runs writes it only when the recorded binary is gone
// (recordSelf), so an upgrade that moves the binary heals at the next
// command and a scratch build never replaces the user's install.

// binaryPathFile is the file name in the config directory.
const binaryPathFile = "binary-path"

// noRecordCommands are the commands that leave the recorded path alone:
// the ones a harness or another tool runs (hook, mcp: the harness picks
// the binary; version and help: setup runs them on other binaries;
// probe: a test run of a development build), the server's, and setup,
// which records the path itself in install mode only.
var noRecordCommands = map[string]bool{
	"hook": true, "mcp": true, "version": true, "help": true, "probe": true, "setup": true,
	"serve": true, "healthcheck": true, "fingerprint": true, "backup": true, "backup-verify": true,
	"restore": true, "bootstrap": true,
}

// recordedBinaryPath is the path of the recorded-path file.
func recordedBinaryPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, binaryPathFile), nil
}

// readRecordedBinary returns the recorded binary path, "" when none is
// recorded.
func readRecordedBinary(file string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// selfPath is this binary's absolute path with symlinks resolved.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Abs(exe)
}

// recordBinary writes exe to file unless it already holds exe; changed
// reports a write. The write is atomic: a hook reading the file at the
// same moment sees the old path or the new one.
func recordBinary(file, exe string) (changed bool, err error) {
	if strings.ContainsAny(exe, "\n\r") {
		return false, fmt.Errorf("binary path %q holds a line break", exe)
	}
	if readRecordedBinary(file) == exe {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), binaryPathFile+".*")
	if err != nil {
		return false, err
	}
	_, werr := tmp.WriteString(exe + "\n")
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp.Name(), 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), file)
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return false, werr
	}
	return true, nil
}

// recordSelf records this binary for a command a person ran, when no
// binary is recorded or the recorded one is gone (an upgrade moved it). It
// never replaces a recorded binary that exists: a scratch, CI or `go run`
// build must not take over the hooks from the user's install; setup does
// that. It stays silent: a read-only config directory must not fail the
// command.
func recordSelf(args []string) {
	if len(args) == 0 || noRecordCommands[args[0]] || !knownCommand(args[0]) {
		return
	}
	if args[0] == "agent" && len(args) > 1 && args[1] == "flush" {
		return // a hook or Codex's notify runs it
	}
	file, err := recordedBinaryPath()
	if err != nil {
		return
	}
	if r := readRecordedBinary(file); r != "" && isExecFile(r) {
		return // only setup replaces a recorded binary that still exists
	}
	exe, err := selfPath()
	if err != nil {
		return
	}
	_, _ = recordBinary(file, exe)
}

// knownCommand reports whether usageText lists cmd (find is grep's alias).
func knownCommand(cmd string) bool {
	return cmd == "find" || usageCommands(usageText)[cmd]
}
