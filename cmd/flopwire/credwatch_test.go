package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// fakeBusServer answers bus polls (counting them) and refuses the rest.
func fakeBusServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != busproto.PathPoll {
			http.NotFound(w, r)
			return
		}
		polls.Add(1)
		select { // a short hold, as a server with nothing to deliver
		case <-r.Context().Done():
		case <-time.After(50 * time.Millisecond):
		}
		_ = json.NewEncoder(w).Encode(busproto.PollResponse{Now: time.Now()})
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

// runAgentAsync runs `flopwire agent run` in the background until it
// returns, and waits until it answers on sock.
func runAgentAsync(t *testing.T, ctx context.Context, home, sock string) <-chan *os.File {
	t.Helper()
	done := make(chan *os.File, 1)
	go func() {
		lock, err := runAgent(ctx, []string{"--socket", sock, "--desktop-code-root", "-", "--cowork-root", "-", "--claude-projects", filepath.Join(home, ".claude", "projects"),
			"--codex-home", filepath.Join(home, ".codex"), "--devin-db", "-", "--opencode-db", "-", "--mem-limit", "0", "--gc-percent", "100"})
		if err != nil {
			t.Errorf("agent: %v", err)
		}
		done <- lock
	}()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := agent.Call(ctx, sock, agent.Request{Op: "ping"}); err == nil {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatal("agent never answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitRestart waits for the agent to ask for a re-exec (it returns the
// index lock), as agentRun then does, and releases the lock for the next
// run.
func waitRestart(t *testing.T, done <-chan *os.File, within time.Duration) {
	t.Helper()
	select {
	case lock := <-done:
		if lock == nil {
			t.Fatal("the agent stopped instead of restarting")
		}
		lock.Close()
	case <-time.After(within):
		t.Fatalf("the agent did not restart within %s", within)
	}
}

// A device that started the agent before any login gets the server bus
// once a device login is saved, without a manual restart: the agent
// restarts itself within one watch tick and polls the server. Removing
// the credential restarts it local (issue #71).
func TestAgentConnectsAfterFirstLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	defer func(d time.Duration) { credentialWatchEvery = d }(credentialWatchEvery)
	credentialWatchEvery = 100 * time.Millisecond
	srv, polls := fakeBusServer(t)
	sock := filepath.Join(shortSockDir(t), "a.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	status := func() agent.Response {
		t.Helper()
		resp, err := agent.Call(ctx, sock, agent.Request{Op: "status"})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// No config: the bus routes on the device.
	done := runAgentAsync(t, ctx, home, sock)
	if r := status(); r.Bus == nil || r.Bus.State != devicebus.StateLocal || r.Credential == nil || r.Credential.Source != credNone {
		t.Fatalf("before login: bus %+v credential %+v", r.Bus, r.Credential)
	}

	// A device login is saved (login, then enroll).
	if err := client.Save(client.Config{Server: srv.URL, Token: "dev", DeviceID: "d1", RotatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	waitRestart(t, done, 2*time.Second)
	done = runAgentAsync(t, ctx, home, sock)
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no poll reached the server; bus %+v", status().Bus)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := status(); r.Bus == nil || r.Bus.State == devicebus.StateLocal || r.Credential == nil || r.Credential.Source != credDevice {
		t.Fatalf("after login: bus %+v credential %+v", r.Bus, r.Credential)
	}

	// The credential is removed: the agent restarts without a server and
	// stops polling.
	p, _ := client.Path()
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	waitRestart(t, done, 2*time.Second)
	done = runAgentAsync(t, ctx, home, sock)
	if r := status(); r.Bus == nil || r.Bus.State != devicebus.StateLocal || r.Credential == nil || r.Credential.Source != credNone {
		t.Fatalf("after removal: bus %+v credential %+v", r.Bus, r.Credential)
	}
	n := polls.Load()
	time.Sleep(300 * time.Millisecond)
	if polls.Load() != n {
		t.Fatal("the bus still polls after the credential was removed")
	}
	cancel()
	if lock := <-done; lock != nil {
		t.Fatal("a shutdown asked for a restart")
	}
}

// The watch ignores a config it cannot read and an unchanged one.
func TestWatchCredential(t *testing.T) {
	for _, c := range []struct {
		name  string
		wired bool
		load  func() (client.Config, error)
		want  string
	}{
		{"saved", false, func() (client.Config, error) { return client.Config{Server: "https://x.test", Token: "t"}, nil }, "a server credential was saved"},
		{"removed", true, func() (client.Config, error) { return client.Config{}, os.ErrNotExist }, "the server credential was removed"},
		{"emptied", true, func() (client.Config, error) { return client.Config{Server: "https://x.test"}, client.ErrNoCredential }, "the server credential was removed"},
		{"damaged", true, func() (client.Config, error) { return client.Config{}, &json.SyntaxError{} }, ""},
		{"unchanged", true, func() (client.Config, error) { return client.Config{Server: "https://x.test", Token: "t"}, nil }, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			var got string
			watchCredential(ctx, 5*time.Millisecond, c.wired, c.load, func(why string) { got = why })
			if c.want == "" && got != "" || !strings.Contains(got, c.want) {
				t.Fatalf("restart %q, want %q", got, c.want)
			}
		})
	}
}

// A config that flaps (saved, removed, saved...) restarts the agent at
// most once per change it sees, and once the config settles the agent
// stops restarting: no restart loop.
func TestAgentCredentialFlapSettles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "flopwire", "config.json"))
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "index.db"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	defer func(d time.Duration) { credentialWatchEvery = d }(credentialWatchEvery)
	credentialWatchEvery = 50 * time.Millisecond
	srv, _ := fakeBusServer(t)
	sock := filepath.Join(shortSockDir(t), "a.sock")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, _ := client.Path()

	flapping := make(chan struct{})
	go func() {
		defer close(flapping)
		for i := range 20 {
			if i%2 == 0 {
				_ = client.Save(client.Config{Server: srv.URL, Token: "dev", DeviceID: "d1"})
			} else {
				_ = os.Remove(p)
			}
			time.Sleep(30 * time.Millisecond)
		}
		// Settled: a device login is saved.
		_ = client.Save(client.Config{Server: srv.URL, Token: "dev", DeviceID: "d1"})
	}()
	restarts := 0
	done := runAgentAsync(t, ctx, home, sock)
	settled := false
	for {
		select {
		case <-flapping:
			flapping, settled = nil, true
		case lock := <-done:
			if lock == nil {
				t.Fatal("the agent stopped instead of restarting")
			}
			lock.Close()
			restarts++
			done = runAgentAsync(t, ctx, home, sock)
			continue
		case <-time.After(500 * time.Millisecond):
			if !settled {
				continue
			}
			// Ten ticks with no restart after the config settled.
			resp, err := agent.Call(ctx, sock, agent.Request{Op: "status"})
			if err != nil || resp.Credential == nil || resp.Credential.Source != credDevice {
				t.Fatalf("after the flap settled: %+v %v", resp.Credential, err)
			}
			if restarts > 20 {
				t.Fatalf("%d restarts for 20 changes", restarts)
			}
			cancel()
			if lock := <-done; lock != nil {
				t.Fatal("a shutdown asked for a restart")
			}
			return
		}
	}
}
