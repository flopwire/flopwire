package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
	"golang.org/x/sys/unix"
)

// This helper replaces the Codex executable, not Flopwire's native-wake
// function. The real agent must invoke queue with an exact UUID and notice.
func TestHermeticCodexQueueProcess(t *testing.T) {
	if os.Getenv("FLOPWIRE_HERMETIC_QUEUE") != "1" {
		t.Skip("synthetic queue subprocess")
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 6 || args[1] != "queue" || args[2] != "--thread" || args[4] != "--message" {
		fmt.Fprintln(os.Stderr, "unexpected queue arguments")
		os.Exit(2)
	}
	conn, err := net.DialTimeout("unix", filepath.Join(os.Getenv("CODEX_HOME"), "app-server-control", "app-server-control.sock"), time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(fakeNativeWake{Session: args[3], Message: map[string]string{"content": args[5]}, Transport: "codex-queue"}); err != nil {
		os.Exit(2)
	}
}

type fakeNativeWake struct {
	Session   string            `json:"session_id"`
	Message   map[string]string `json:"message"`
	Transport string            `json:"transport"`
	Error     string            `json:"error,omitempty"`
}

func fakeWakeEndpoint(t *testing.T, path string) <-chan fakeNativeWake {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan fakeNativeWake, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			var event fakeNativeWake
			if err := json.NewDecoder(conn).Decode(&event); err != nil {
				event.Error = err.Error()
			}
			conn.Close()
			select {
			case events <- event:
			default:
				return
			}
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return events
}

func appendFakeHarnessContext(t *testing.T, path, sid, harness, text string) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	record := func(v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		appendLine(t, path, string(raw))
	}
	if harness == "codex" {
		record(map[string]any{"timestamp": stamp, "type": "response_item", "payload": map[string]any{"type": "message", "id": "synthetic-hook", "role": "developer", "content": []any{map[string]string{"type": "input_text", "text": text}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "synthetic-turn", "content_item_kinds": []string{"hooks.additional_context"}}}})
		record(map[string]any{"timestamp": stamp, "type": "response_item", "payload": map[string]any{"type": "message", "id": "synthetic-answer", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "Synthetic response; no model was called."}}}})
		record(map[string]any{"timestamp": stamp, "type": "event_msg", "payload": map[string]string{"type": "task_complete", "turn_id": "synthetic-turn"}})
		return
	}
	record(map[string]any{"timestamp": stamp, "sessionId": sid, "cwd": "/tmp/e2e-api", "uuid": "synthetic-hook", "parentUuid": sid[:8] + "-a1", "type": "attachment", "attachment": map[string]any{"type": "hook_additional_context", "content": []string{text}, "hookName": "UserPromptSubmit", "hookEvent": "UserPromptSubmit"}})
	record(map[string]any{"timestamp": stamp, "sessionId": sid, "cwd": "/tmp/e2e-api", "uuid": "synthetic-answer", "parentUuid": "synthetic-hook", "type": "assistant", "message": map[string]any{"id": "synthetic-answer", "role": "assistant", "model": "synthetic-no-model", "content": []any{map[string]string{"type": "text", "text": "Synthetic response; no model was called."}}, "stop_reason": "end_turn"}})
}

// These scenarios use production discovery, native wake submission, hook
// delivery, parsing, receipts and CLI/MCP routing. Only the harness end is
// simulated; there are no model calls, real logins or installed harnesses.
func TestWakeEndToEndWithoutModels(t *testing.T) {
	for _, mode := range []string{"local", "server"} {
		t.Run(mode, func(t *testing.T) {
			for _, harness := range []string{"claude", "codex"} {
				t.Run(harness, func(t *testing.T) {
					home := shortSockDir(t)
					projects := filepath.Join(home, ".claude", "projects")
					codexHome := filepath.Join(home, ".codex")
					t.Setenv("FLOPWIRE_CONFIG", filepath.Join(home, "flopwire", "config.json"))
					t.Setenv("FLOPWIRE_INDEX", filepath.Join(home, "index.db"))
					t.Setenv(client.EnvToken, "")
					t.Setenv(client.EnvServer, "")
					t.Setenv("FLOPWIRE_CLOUD", "off")
					t.Setenv("CODEX_HOME", codexHome)
					for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
						t.Setenv(key, "")
					}
					bin := filepath.Join(home, "bin")
					if err := os.MkdirAll(bin, 0700); err != nil {
						t.Fatal(err)
					}
					// A surprise Claude invocation fails rather than reaching an installed CLI.
					if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 73\n"), 0700); err != nil {
						t.Fatal(err)
					}
					executable, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
					shim := "#!/bin/sh\nexec " + quote(executable) + " -test.run='^TestHermeticCodexQueueProcess$' -- \"$@\"\n"
					if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(shim), 0700); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
					t.Setenv("FLOPWIRE_HERMETIC_QUEUE", "1")
					if mode == "server" {
						server := newAcceptServer(t)
						enrolled := apiCall(t, server.url, server.gary, "POST", "/v1/devices", map[string]string{"name": "synthetic-native-wake", "platform": "test"})
						if err := client.Save(client.Config{Server: server.url, Token: enrolled["token"].(string), DeviceID: enrolled["device"].(map[string]any)["id"].(string)}); err != nil {
							t.Fatal(err)
						}
					}
					writeClaudeSession(t, projects, e2eA, "/tmp/e2e-api", "synthetic sender")
					pathA := filepath.Join(projects, "-tmp-e2e-api", e2eA+".jsonl")
					target := e2eB
					var pathB, nativePath string
					if harness == "claude" {
						writeClaudeSession(t, projects, target, "/tmp/e2e-api", "synthetic recipient")
						pathB = filepath.Join(projects, "-tmp-e2e-api", target+".jsonl")
						nativePath = filepath.Join(home, "claude.sock")
						start, err := exec.Command("ps", "-p", fmt.Sprint(os.Getpid()), "-o", "lstart=").Output()
						if err != nil {
							t.Fatal(err)
						}
						registry := filepath.Join(home, ".claude", "sessions")
						if err := os.MkdirAll(registry, 0700); err != nil {
							t.Fatal(err)
						}
						raw, _ := json.Marshal(map[string]any{"sessionId": target, "status": "idle", "procStart": strings.TrimSpace(string(start)), "messagingSocketPath": nativePath})
						if err := os.WriteFile(filepath.Join(registry, fmt.Sprint(os.Getpid())+".json"), raw, 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						target = "019a0000-0000-7000-8000-0000000000a2"
						nativePath = filepath.Join(codexHome, "app-server-control", "app-server-control.sock")
						pathB = filepath.Join(codexHome, "sessions", "2026", "10", "26", "rollout-2026-10-26T00-00-00-"+target+".jsonl")
						if err := os.MkdirAll(filepath.Dir(pathB), 0700); err != nil {
							t.Fatal(err)
						}
						stamp := time.Now().UTC().Format(time.RFC3339Nano)
						meta, _ := json.Marshal(map[string]any{"timestamp": stamp, "type": "session_meta", "payload": map[string]string{"id": target, "cwd": "/tmp/e2e-api", "source": "cli"}})
						if err := os.WriteFile(pathB, append(meta, '\n'), 0600); err != nil {
							t.Fatal(err)
						}
						appendLine(t, pathB, fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"task_complete","turn_id":"initial"}}`, stamp))
						locks := filepath.Join(codexHome, "thread-writer-locks")
						if err := os.MkdirAll(locks, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(locks, ".coordination.lock"), nil, 0600); err != nil {
							t.Fatal(err)
						}
						lock, err := os.OpenFile(filepath.Join(locks, target+".lock"), os.O_CREATE|os.O_RDWR, 0600)
						if err != nil {
							t.Fatal(err)
						}
						if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
							lock.Close()
							t.Fatal(err)
						}
						t.Cleanup(func() { unix.Flock(int(lock.Fd()), unix.LOCK_UN); lock.Close() })
					}
					events := fakeWakeEndpoint(t, nativePath)
					sock := filepath.Join(home, "agent.sock")
					extra := []string{}
					if mode == "local" {
						extra = append(extra, "--no-sync")
					}
					startAgent(t, home, sock, extra...)
					waitPeer(t, sock, e2eA, target)
					e2eWait(t, "native wake capability", func() bool {
						r, err := agent.Call(t.Context(), sock, agent.Request{Op: "peers", Peers: &busproto.PeersQuery{Session: e2eA}})
						if err == nil && r.Peers != nil {
							for _, p := range r.Peers.Peers {
								if p.Session == target && p.WakeSupported {
									return true
								}
							}
						}
						return false
					})
					passive, err := busCLI(t, sock, e2eA, "", "send", target, "--", "PASSIVE-SYNTHETIC")
					if err != nil {
						t.Fatal(err)
					}
					passiveID := strings.Fields(passive)[1]
					assertNoWake := func() {
						t.Helper()
						select {
						case event := <-events:
							t.Fatalf("unintended native wake: %+v", event)
						case <-time.After(2200 * time.Millisecond):
						}
					}
					assertNoWake()
					sent, err := busCLI(t, sock, e2eA, "", "send", target, "--wake", "--intent", "request", "--", "WAKE-SYNTHETIC")
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(sent, "may start a new turn and consume usage") {
						t.Fatalf("missing cost warning: %s", sent)
					}
					requestID := strings.Fields(sent)[1]
					select {
					case event := <-events:
						if event.Error != "" || event.Session != target || !strings.Contains(event.Message["content"], "Flopwire wake requested") || strings.Contains(event.Message["content"], "WAKE-SYNTHETIC") {
							t.Fatalf("wrong native wake: %+v", event)
						}
						if harness == "codex" && event.Transport != "codex-queue" {
							t.Fatal("Codex queue executable was bypassed")
						}
					case <-time.After(10 * time.Second):
						t.Fatal("explicit wake never reached fake harness")
					}
					deliver := func(sid, path, kind string) string {
						t.Helper()
						in := hookJSON(map[string]any{"session_id": sid, "hook_event_name": evUserPromptSubmit, "transcript_path": path, "cwd": "/tmp/e2e-api"})
						text := contextOf(t, runHooks(t, sock, in, 1)[0])
						if text == "" {
							t.Fatal("hook delivered no context")
						}
						appendFakeHarnessContext(t, path, sid, kind, text)
						flushNow(t, sock, path)
						return text
					}
					printed := deliver(target, pathB, harness)
					for _, id := range []string{passiveID, requestID} {
						if strings.Count(printed, `<flopwire-message id="`+id+`"`) != 1 {
							t.Fatalf("message %s not delivered exactly once: %s", id, printed)
						}
					}
					// A real MCP invocation sends the simulated model's reply, without wake.
					receiver := &retriever{caller: func(context.Context) (local.Caller, bool) {
						return local.Caller{Agent: transcript.Agent(harness), SessionID: target, Rule: "synthetic"}, true
					}, busSocket: sock}
					raw, err := mcpCall(t.Context(), receiver, "flopwire_send", map[string]any{"to": e2eA, "message": "SYNTHETIC-REPLY", "reply_to": requestID, "intent": "done"})
					var reply sendJSON
					if err != nil || json.Unmarshal([]byte(raw), &reply) != nil || reply.WakeRequested {
						t.Fatalf("MCP reply: %s %v", raw, err)
					}
					if _, err := busCLI(t, sock, e2eA, "", "send", target, "--wake", "--intent", "request", "--", "WAKE-SYNTHETIC"); err == nil || !strings.Contains(err.Error(), "duplicate") {
						t.Fatalf("duplicate wake accepted: %v", err)
					}
					assertNoWake()
					senderText := deliver(e2eA, pathA, "claude")
					if strings.Count(senderText, `<flopwire-message id="`+reply.ID+`"`) != 1 {
						t.Fatalf("reply not delivered once: %s", senderText)
					}
					for _, who := range []struct {
						id, agent string
						messages  []string
					}{{e2eA, "claude", []string{passiveID, requestID}}, {target, harness, []string{reply.ID}}} {
						e2eWait(t, "read receipts", func() bool {
							r, err := agent.Call(t.Context(), sock, agent.Request{Op: "inbox", Inbox: &busproto.InboxQuery{Session: who.id, Agent: who.agent, SentOnly: true}})
							if err != nil || r.Inbox == nil {
								return false
							}
							count := 0
							for _, id := range who.messages {
								for _, m := range r.Inbox.Messages {
									if m.ID == id && m.State == busproto.StateRead && m.ReadAt != nil {
										count++
									}
								}
							}
							return count == len(who.messages)
						})
					}
				})
			}
		})
	}
}
