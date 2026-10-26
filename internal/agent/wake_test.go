package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicebus"
)

func TestClaudeWakeUsesVerifiedRegistryAndNativeInput(t *testing.T) {
	f := newFixture(t, "-")
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	f.a.pidAlive = func(pid int) bool { return pid == 4242 }
	f.a.procStart = func(pid int) (time.Time, bool) { return started, pid == 4242 }
	socketDir, err := os.MkdirTemp("/tmp", "fw-wake-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "wake.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	file := filepath.Join(filepath.Dir(f.a.cfg.ClaudeProjects), "sessions", "4242.json")
	writeFile(t, file, fmt.Sprintf(`{"sessionId":"synthetic-wake","procStart":%q,"messagingSocketPath":%q}`, started.Format(time.ANSIC), socket))
	s := devicebus.Session{}
	s.SessionID = "synthetic-wake"
	s.Agent = "claude"
	if !f.a.wakeSupported(s) {
		t.Fatal("verified inbox not supported")
	}
	received := make(chan map[string]any, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		var raw map[string]any
		json.NewDecoder(bufio.NewReader(conn)).Decode(&raw)
		received <- raw
	}()
	if err := f.a.wakeSession(t.Context(), s, "synthetic wake notice"); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-received:
		if raw["session_id"] != s.SessionID || raw["from"] != "flopwire" || raw["type"] != "user" {
			t.Fatalf("native payload: %+v", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("native input not received")
	}
	f.a.pidAlive = func(int) bool { return false }
	if f.a.wakeSupported(s) {
		t.Fatal("stale process advertised wake")
	}
	if err := f.a.wakeSession(t.Context(), s, "must not send"); err == nil {
		t.Fatal("stale process received native input")
	}
}
