package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicebus"
)

// credentialFixture is one credential source: what the config file and
// the environment hold, and what setup and status must say about it.
type credentialFixture struct {
	name    string
	file    *client.Config // nil: no config file
	env     bool           // FLOPWIRE_TOKEN and FLOPWIRE_SERVER set
	source  string
	off     string // in the messaging: off line; "" for none
	warning string // "" for none
}

var credentialFixtures = []credentialFixture{
	{name: "none", source: credNone},
	{name: "device", file: &client.Config{Server: "https://team.test", Token: "dev", DeviceID: "d1"}, source: credDevice},
	{name: "legacy", file: &client.Config{Server: "https://team.test", Token: "sess"}, source: credLegacy,
		off: "this login has no device credential, and messaging needs one: run flopwire login, then flopwire enroll"},
	{name: "env", env: true, source: credEnv,
		off: "FLOPWIRE_TOKEN is a minted token, and messaging needs an enrolled device credential: unset FLOPWIRE_TOKEN, then run flopwire login and flopwire enroll"},
	{name: "env-hides-device", env: true, file: &client.Config{Server: "https://team.test", Token: "dev", DeviceID: "d1"}, source: credEnv,
		off:     "unset FLOPWIRE_TOKEN to use the saved device login",
		warning: "FLOPWIRE_TOKEN in the environment hides the device login saved for https://team.test; unset FLOPWIRE_TOKEN to use it"},
	// A legacy login under FLOPWIRE_TOKEN: nothing that messaging could
	// use is hidden, so no warning.
	{name: "env-over-legacy", env: true, file: &client.Config{Server: "https://team.test", Token: "sess"}, source: credEnv,
		off: "unset FLOPWIRE_TOKEN, then run flopwire login and flopwire enroll"},
}

// apply puts the fixture's credential in place. FLOPWIRE_CONFIG must
// point at a fresh file.
func (f credentialFixture) apply(t *testing.T) {
	t.Helper()
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	t.Setenv(client.EnvFingerprint, "")
	if f.env {
		t.Setenv(client.EnvToken, "minted")
		t.Setenv(client.EnvServer, "https://ci.test")
	}
	if f.file != nil {
		if err := client.Save(*f.file); err != nil {
			t.Fatal(err)
		}
	}
}

// Each credential source is named, with messaging: off and the fix when
// the source cannot use the bus, and a warning when FLOPWIRE_TOKEN hides a
// saved device login (issue #71). FLOPWIRE_TOKEN still wins.
func TestCredentialSourceFixtures(t *testing.T) {
	for _, f := range credentialFixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			f.apply(t)
			got := credentialSource(client.Load, client.LoadFile)
			if got.Source != f.source || !strings.Contains(got.MessagingOff, f.off) || f.off == "" && got.MessagingOff != "" || got.Warning != f.warning {
				t.Fatalf("credentialSource = %+v, want source %q, off %q, warning %q", got, f.source, f.off, f.warning)
			}
			if cc, err := client.Load(); err == nil && f.env && cc.Token != "minted" {
				t.Fatalf("FLOPWIRE_TOKEN no longer wins: %+v", cc)
			}
			var b strings.Builder
			cfg, _ := client.LoadFile()
			credentialStatus(&b, got, cfg, time.Now())
			out := b.String()
			if !strings.HasPrefix(out, "credential: "+f.source) {
				t.Fatalf("status lacks the source:\n%s", out)
			}
			if f.off != "" && !strings.Contains(out, "messaging: off: ") || !strings.Contains(out, f.off) || f.off == "" && strings.Contains(out, "messaging: off") {
				t.Fatalf("status messaging line, want %q:\n%s", f.off, out)
			}
			if f.warning != "" && !strings.Contains(out, "warning: "+f.warning+"\n") || f.warning == "" && strings.Contains(out, "warning: FLOPWIRE_TOKEN") {
				t.Fatalf("status warning, want %q:\n%s", f.warning, out)
			}
		})
	}
}

// setup --check names the credential source in its server section, with
// the messaging and warning lines (issue #71).
func TestSetupCheckNamesTheCredential(t *testing.T) {
	for _, f := range credentialFixtures {
		t.Run(f.name, func(t *testing.T) {
			fx := newSetupFixture(t, false)
			f.apply(t)
			rep, _, err := fx.run("--check")
			if err != nil {
				t.Fatal(err)
			}
			if rep.Server.Credential != f.source || !strings.Contains(rep.Server.Messaging, f.off) || f.off == "" && rep.Server.Messaging != "" || rep.Server.Warning != f.warning {
				t.Fatalf("server section %+v, want source %q, off %q, warning %q", rep.Server, f.source, f.off, f.warning)
			}
			_, out, err := fx.run("--check", "--text")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "\ncredential: "+f.source+"\n") {
				t.Fatalf("--text lacks the credential line:\n%s", out)
			}
			if f.off != "" && !strings.Contains(out, "\nmessaging: off: ") || !strings.Contains(out, f.off) || f.off == "" && strings.Contains(out, "messaging: off") {
				t.Fatalf("--text messaging line, want %q:\n%s", f.off, out)
			}
			if f.warning != "" && !strings.Contains(out, "\nwarning: "+f.warning+"\n") || f.warning == "" && strings.Contains(out, "\nwarning: ") {
				t.Fatalf("--text warning, want %q:\n%s", f.warning, out)
			}
		})
	}
}

// agent status prints the agent's credential, and a bus stopped for the
// same reason does not say it a second time.
func TestAgentStatusMessagingOffOnce(t *testing.T) {
	off := "this login has no device credential, and messaging needs one: run flopwire login, then flopwire enroll"
	resp := agent.Response{Bus: &devicebus.Status{State: devicebus.StateStopped, LastError: off}, Credential: &agent.Credential{Source: credLegacy, MessagingOff: off}}
	var b strings.Builder
	printAgentStatus(&b, resp)
	credentialStatus(&b, *resp.Credential, client.Config{}, time.Now())
	out := b.String()
	if strings.Count(out, off) != 1 || !strings.Contains(out, "messaging: off: "+off) || strings.Contains(out, "messaging: stopped") {
		t.Fatalf("status:\n%s", out)
	}
	// A bus stopped for another reason (a refused credential) still says so.
	resp.Bus.LastError = "the server refused this device's credential: run flopwire login"
	b.Reset()
	printAgentStatus(&b, resp)
	if !strings.Contains(b.String(), "messaging: stopped: the server refused") {
		t.Fatalf("status:\n%s", b.String())
	}
}

// A shell with FLOPWIRE_TOKEN set while the agent runs without it: setup
// --check and agent status report the agent's credential, and say that
// this shell's differs and why.
func TestCredentialShellDiffersFromAgent(t *testing.T) {
	fx := newSetupFixture(t, false)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(shortSockDir(t), "config.json"))
	credentialFixtures[4].apply(t) // env-hides-device: FLOPWIRE_TOKEN here, a device login saved
	fa := startFakeAgent(t, func(agent.Request) agent.Response {
		return agent.Response{OK: true, Credential: &agent.Credential{Source: credDevice}}
	})
	t.Setenv("FLOPWIRE_SOCKET", fa.sock)
	rep, _, err := fx.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	want := "this shell's credential is FLOPWIRE_TOKEN, the running agent's is device login: FLOPWIRE_TOKEN is set in this shell but not in the agent's environment"
	if rep.Server.Credential != credDevice || !strings.Contains(rep.Server.Differs, want) {
		t.Fatalf("server section %+v, want the agent's source and %q", rep.Server, want)
	}
	_, out, err := fx.run("--check", "--text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\nnote: "+want) {
		t.Fatalf("--text lacks the note:\n%s", out)
	}
	// agent status asks the socket beside the config.
	dir, _ := configDir()
	if err := os.Symlink(fa.sock, filepath.Join(dir, "agent.sock")); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	_ = agentStatusOutput(t.Context(), &b, false)
	if !strings.Contains(b.String(), "credential: device login") || !strings.Contains(b.String(), "note: "+want) {
		t.Fatalf("agent status:\n%s", b.String())
	}
}
