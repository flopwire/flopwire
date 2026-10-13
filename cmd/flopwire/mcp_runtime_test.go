package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
)

func runtimeFixture(t *testing.T, binary string) mcpRuntime {
	t.Helper()
	if err := os.WriteFile(binary, []byte("old executable"), 0700); err != nil {
		t.Fatal(err)
	}
	r := mcpRuntime{PID: os.Getpid(), StartedAt: time.Now().UTC(), Version: "old-build", Binary: binary, CredentialSource: "file"}
	if err := runtimeIdentity(binary, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMCPRuntimeProcess(t *testing.T) {
	if os.Getenv("FLOPWIRE_RUNTIME_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	r := mcpRuntime{PID: os.Getpid(), StartedAt: time.Now().UTC(), Version: "old-build", Binary: os.Getenv("FLOPWIRE_RUNTIME_TEST_BINARY"), CredentialSource: "file"}
	if err := runtimeIdentity(r.Binary, &r); err != nil {
		t.Fatal(err)
	}
	defer writeMCPRuntime(os.Getenv("FLOPWIRE_RUNTIME_TEST_DIR"), r)()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestMCPRuntimeDetectsUpgradeWithoutRestartingProcess(t *testing.T) {
	dir := t.TempDir()
	receipts := filepath.Join(dir, "runtime")
	binary := filepath.Join(dir, "flopwire")
	runtimeFixture(t, binary)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMCPRuntimeProcess$")
	cmd.Env = append(os.Environ(), "FLOPWIRE_RUNTIME_TEST_CHILD=1", "FLOPWIRE_RUNTIME_TEST_BINARY="+binary, "FLOPWIRE_RUNTIME_TEST_DIR="+receipts)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("subprocess readiness %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runtime subprocess did not start")
	}
	rows, _ := readMCPRuntimes(receipts, false)
	if len(rows) != 1 || rows[0].Stale || rows[0].PID != cmd.Process.Pid {
		t.Fatalf("initial runtime: %+v", rows)
	}
	replacement := binary + ".next"
	if err := os.WriteFile(replacement, []byte("new executable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, binary); err != nil {
		t.Fatal(err)
	}
	rows, _ = readMCPRuntimes(receipts, false)
	if len(rows) != 1 || !rows[0].Stale || !strings.Contains(rows[0].Note, "reconnect only this Flopwire MCP connection") {
		t.Fatalf("upgraded runtime: %+v", rows)
	}
	// The connection is still alive and retains its original build. The check
	// reports its own state rather than inspecting a possibly reused PID.
	if rows[0].Version != "old-build" {
		t.Fatalf("lost runtime build: %+v", rows[0])
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("runtime exit: %v %s", err, stderr.String())
	}
	rows, _ = readMCPRuntimes(receipts, false)
	if len(rows) != 0 {
		t.Fatalf("closed runtime still live: %+v", rows)
	}
}

func TestMCPRuntimeReceiptHasNoCredentials(t *testing.T) {
	for _, source := range []string{"file", "environment"} {
		t.Run(source, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "config.json")
			t.Setenv("FLOPWIRE_CONFIG", config)
			t.Setenv(client.EnvToken, "")
			t.Setenv("FLOPWIRE_RUNTIME_UNRELATED_SECRET", "unrelated-sensitive-value")
			if err := os.WriteFile(config, []byte(`{"token":"file-sensitive-value","server":"https://example.test"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if source == "environment" {
				t.Setenv(client.EnvToken, "env-sensitive-value")
			}
			cleanup := recordMCPRuntime()
			defer cleanup()
			dir, _ := mcpRuntimeDir()
			rows, _ := readMCPRuntimes(dir, false)
			if len(rows) != 1 || rows[0].CredentialSource != source {
				t.Fatalf("source: %+v", rows)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"file-sensitive-value", "env-sensitive-value", "unrelated-sensitive-value", "token", "FLOPWIRE_RUNTIME_UNRELATED_SECRET"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatal("receipt contains credential or arbitrary environment")
				}
			}
			var fields map[string]any
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if len(fields) != 9 && len(fields) != 10 {
				t.Fatalf("unexpected receipt fields: %v", fields)
			}
		})
	}
}

func TestMCPRuntimeDeadReceiptsAndPIDReuse(t *testing.T) {
	dir := t.TempDir()
	r := runtimeFixture(t, filepath.Join(t.TempDir(), "flopwire"))
	// This unlocked receipt even names the current live PID: PID reuse must not
	// make it count as a connection. setup check must leave it untouched.
	raw, _ := json.Marshal(r)
	dead := filepath.Join(dir, "dead.json")
	if err := os.WriteFile(dead, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rows, _ := readMCPRuntimes(dir, false)
	if len(rows) != 0 {
		t.Fatal("unlocked reused PID reported live")
	}
	if _, err := os.Stat(dead); err != nil {
		t.Fatal("read-only check changed receipt")
	}
	cleanup := writeMCPRuntime(dir, r)
	defer cleanup()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("startup did not prune dead receipt")
	}
	rows, _ = readMCPRuntimes(dir, true)
	if len(rows) != 1 {
		t.Fatal("startup pruned live receipt")
	}
}

func TestMCPRuntimeConcurrentRegistrationsAndBoundedScan(t *testing.T) {
	dir := t.TempDir()
	r := runtimeFixture(t, filepath.Join(t.TempDir(), "flopwire"))
	var wg sync.WaitGroup
	cleanups := make(chan func(), 16)
	for range 16 {
		wg.Go(func() { cleanups <- writeMCPRuntime(dir, r) })
	}
	wg.Wait()
	close(cleanups)
	defer func() {
		for cleanup := range cleanups {
			cleanup()
		}
	}()
	rows, _ := readMCPRuntimes(dir, false)
	if len(rows) != 16 {
		t.Fatalf("concurrent registrations lost: %d", len(rows))
	}
	for n := range mcpRuntimeScanLimit + 1 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("dead-%d.json", n)), []byte(`{}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, note := readMCPRuntimes(dir, false)
	if !strings.Contains(note, "limited to 1024") {
		t.Fatal("scan did not report incomplete coverage")
	}
}

func TestMCPRuntimeStagedReceipts(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Minute)
	dead := filepath.Join(dir, ".starting-dead")
	if err := os.WriteFile(dead, []byte("interrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(dir, ".starting-active")
	f, err := os.OpenFile(active, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, ".starting-fresh")
	if err := os.WriteFile(fresh, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Staging entries do not take the published receipt budget.
	for n := range mcpRuntimeLimit + 1 {
		path := filepath.Join(dir, fmt.Sprintf(".starting-%d", n))
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := runtimeFixture(t, filepath.Join(t.TempDir(), "flopwire"))
	cleanup := writeMCPRuntime(dir, r)
	defer cleanup()
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("old unlocked stage retained")
	}
	for _, path := range []string{active, fresh} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("live or fresh stage removed")
		}
	}
	rows, note := readMCPRuntimes(dir, false)
	if len(rows) != 1 || strings.Contains(note, "limited") {
		t.Fatalf("stages hid published receipt: %d %s", len(rows), note)
	}
}

func TestMCPRuntimeCheckIsReadOnlyAndRejectsSymlinks(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	rows, note := readMCPRuntimes(missing, false)
	if len(rows) != 0 || !strings.Contains(note, "unknown") {
		t.Fatalf("missing coverage: %v %s", rows, note)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("check created runtime directory")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("private unrelated content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}
	rows, _ = readMCPRuntimes(dir, true)
	if len(rows) != 0 {
		t.Fatal("followed symlink receipt")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("removed symlink target")
	}
	linkedDir := filepath.Join(t.TempDir(), "linked-runtime")
	if err := os.Symlink(dir, linkedDir); err != nil {
		t.Fatal(err)
	}
	_, note = readMCPRuntimes(linkedDir, true)
	if note != "MCP runtime diagnostics unavailable" {
		t.Fatal("followed symlink directory")
	}
}

func TestMCPRuntimeReportDerivesStaleState(t *testing.T) {
	dir := t.TempDir()
	r := runtimeFixture(t, filepath.Join(t.TempDir(), "flopwire"))
	r.Stale = true
	r.Note = "untrusted status"
	f, err := os.CreateTemp(dir, "manual-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(r); err != nil {
		t.Fatal(err)
	}
	rows, _ := readMCPRuntimes(dir, false)
	if len(rows) != 1 || rows[0].Stale || rows[0].Note != "" {
		t.Fatalf("accepted saved derived state: %+v", rows)
	}
	var output bytes.Buffer
	writeSetupText(&output, setupReport{MCP: rows, MCPNote: "coverage limited"})
	if !strings.Contains(output.String(), "credential source file") || !strings.Contains(output.String(), "MCP coverage: coverage limited") {
		t.Fatal("setup text lost runtime diagnostics")
	}
}
