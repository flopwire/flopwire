package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"golang.org/x/sys/unix"
)

const mcpRuntimeLimit = 128
const mcpRuntimeScanLimit = 1024

// A locked receipt belongs to this process, even if its PID is later reused.
// No tokens, tool arguments, transcript content or arbitrary environment are saved.
type mcpRuntime struct {
	PID              int       `json:"pid"`
	StartedAt        time.Time `json:"started_at"`
	Version          string    `json:"version"`
	Revision         string    `json:"revision,omitempty"`
	Binary           string    `json:"binary"`
	CredentialSource string    `json:"credential_source"`
	Device           uint64    `json:"device"`
	Inode            uint64    `json:"inode"`
	Size             int64     `json:"size"`
	ModifiedAt       time.Time `json:"modified_at"`
	Stale            bool      `json:"stale,omitempty"`
	Note             string    `json:"note,omitempty"`
}

func mcpBuildRevision() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return ""
}

func runtimeIdentity(path string, r *mcpRuntime) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("binary identity unavailable")
	}
	r.Device, r.Inode = uint64(stat.Dev), uint64(stat.Ino)
	r.Size, r.ModifiedAt = info.Size(), info.ModTime()
	return nil
}

func mcpRuntimeDir() (string, error) {
	dir, err := configDir()
	return filepath.Join(dir, "mcp-runtime"), err
}

// Best effort: diagnostic storage must never prevent tools from starting.
func recordMCPRuntime() func() {
	dir, err := mcpRuntimeDir()
	if err != nil {
		return func() {}
	}
	binary, err := selfPath()
	if err != nil {
		return func() {}
	}
	source := "file"
	if strings.TrimSpace(os.Getenv(client.EnvToken)) != "" {
		source = "environment"
	}
	r := mcpRuntime{PID: os.Getpid(), StartedAt: time.Now().UTC(), Version: version, Revision: mcpBuildRevision(), Binary: binary, CredentialSource: source}
	if runtimeIdentity(binary, &r) != nil {
		return func() {}
	}
	return writeMCPRuntime(dir, r)
}

func writeMCPRuntime(dir string, r mcpRuntime) func() {
	r.Stale, r.Note = false, ""
	noop := func() {}
	if os.MkdirAll(dir, 0700) != nil {
		return noop
	}
	// Clean only unlocked old receipts. A bounded scan preserves live instances.
	_, _ = readMCPRuntimes(dir, true)
	file, err := os.CreateTemp(dir, ".starting-*")
	if err != nil {
		return noop
	}
	path := file.Name()
	fail := func() func() { file.Close(); os.Remove(path); return noop }
	if unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return fail()
	}
	if json.NewEncoder(file).Encode(r) != nil {
		return fail()
	}
	published := path + ".json"
	if os.Rename(path, published) != nil {
		return fail()
	}
	return func() { os.Remove(published); file.Close() }
}

// setup --check reads receipts without changing them. Startup may prune dead
// receipts. flock, rather than PID liveness, also handles PID reuse and crashes.
func readMCPRuntimes(dir string, prune bool) ([]mcpRuntime, string) {
	directoryFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil, "live MCP builds are unknown until each Flopwire connection is reconnected with runtime diagnostics"
	}
	if err != nil {
		return nil, "MCP runtime diagnostics unavailable"
	}
	directory := os.NewFile(uintptr(directoryFD), dir)
	defer directory.Close()
	entries, err := directory.ReadDir(mcpRuntimeScanLimit + 1)
	if err != nil && err != io.EOF {
		return nil, "MCP runtime diagnostics unavailable"
	}
	note := "pre-diagnostic MCP processes are not tracked; an empty list does not prove that none are running"
	if len(entries) > mcpRuntimeScanLimit {
		entries = entries[:mcpRuntimeScanLimit]
		note += "; directory scan limited to 1024 entries"
	}
	var rows []mcpRuntime
	for _, entry := range entries {
		stage := strings.HasPrefix(entry.Name(), ".starting-") && !strings.HasSuffix(entry.Name(), ".json")
		if entry.IsDir() || (!stage && !strings.HasSuffix(entry.Name(), ".json")) || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		// Never follow a receipt symlink, including one swapped after ReadDir.
		fd, err := unix.Openat(directoryFD, entry.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			continue
		}
		f := os.NewFile(uintptr(fd), path)
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			continue
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if prune && (!stage || time.Since(info.ModTime()) > time.Minute) {
				unix.Unlinkat(directoryFD, entry.Name(), 0)
			}
			f.Close()
			continue
		}
		if stage {
			f.Close()
			continue
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			f.Close()
			continue
		}
		var r mcpRuntime
		err = json.NewDecoder(io.LimitReader(f, 8192)).Decode(&r)
		f.Close()
		if err != nil || r.PID <= 0 || r.StartedAt.IsZero() || !filepath.IsAbs(r.Binary) {
			continue
		}
		if r.CredentialSource != "file" && r.CredentialSource != "environment" {
			continue
		}
		r.Stale, r.Note = false, ""
		current := mcpRuntime{}
		if runtimeIdentity(r.Binary, &current) != nil {
			r.Note = "installed executable unavailable; reconnect only this Flopwire MCP connection after installing the binary"
		} else if current.Device != r.Device || current.Inode != r.Inode || current.Size != r.Size || !current.ModifiedAt.Equal(r.ModifiedAt) {
			r.Stale = true
			r.Note = "installed executable changed; reconnect only this Flopwire MCP connection in its harness to load the update; preserve the agent session"
		}
		if len(rows) == mcpRuntimeLimit {
			note += "; runtime scan limited to 128 live receipts"
			break
		}
		rows = append(rows, r)
	}
	return rows, note
}
