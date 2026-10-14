package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The worktree is absent. Retained placement must connect it to the live
// main checkout and a clone of the same remote in every retention mode.
func TestRetainedRepoMetadataSharedScope(t *testing.T) {
	base := t.TempDir()
	main, gone, clone := filepath.Join(base, "app"), filepath.Join(base, "app-old"), filepath.Join(base, "clone")
	for _, dir := range []string{main, clone} {
		if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
			t.Fatalf("git init: %s %v", out, err)
		}
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://github.test/team/app.git").CombinedOutput(); err != nil {
			t.Fatalf("git remote: %s %v", out, err)
		}
	}
	var queries []url.Values
	var policyFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/policy" {
			policyFetches.Add(1)
			if r.Header.Get("Authorization") != "Bearer device" {
				t.Error("policy fetch lost enrolled credential")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"path_rules": []string{}, "unplaceable": "local"})
			return
		}
		if r.URL.Path != "/v1/sessions" && r.URL.Path != busproto.PathMessages {
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.Query())
		if r.URL.Path == busproto.PathMessages {
			_ = json.NewEncoder(w).Encode(busproto.MessagesResponse{Kind: "messages", Threads: []busproto.MessagesItem{{InboxItem: busproto.InboxItem{Envelope: busproto.Envelope{ID: "authorized-message", Body: "retained"}}}}})
			return
		}
		_ = json.NewEncoder(w).Encode(format.Sessions{Sessions: []format.ConversationInfo{{SessionID: "authorized-session", Address: "authorized-address"}}})
	}))
	defer srv.Close()
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := client.Save(client.Config{Server: srv.URL, Token: "device", DeviceID: "enrolled-device"}); err != nil {
		t.Fatal(err)
	}
	// Establish the connected device's actual policy cache through the public
	// agent path. Empty native roots keep this fixture independent of host data.
	empty := t.TempDir()
	t.Setenv("FLOPWIRE_INDEX", filepath.Join(t.TempDir(), "bootstrap.db"))
	bootstrapCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if lock, err := runAgent(bootstrapCtx, []string{"--once", "--socket", filepath.Join(shortSockDir(t), "bootstrap.sock"), "--desktop-code-root", "-", "--cowork-root", "-", "--claude-projects", empty, "--codex-home", empty, "--devin-db", "-", "--opencode-db", "-", "--mem-limit", "0", "--sync-timeout", "1s"}); err != nil {
		t.Fatal(err)
	} else if lock != nil {
		lock.Close()
		t.Fatal("bootstrap unexpectedly requested re-exec")
	}
	cancel()
	if policyFetches.Load() == 0 {
		t.Fatal("bootstrap did not fetch connected policy")
	}
	want := map[string]string{}
	for _, mode := range []string{"full", "sync-only", "missing-fts"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.db")
			s, err := localindex.Open(path, localindex.Options{SyncOnly: mode == "sync-only"})
			if err != nil {
				t.Fatal(err)
			}
			for i, d := range []localindex.RepoDir{{Dir: gone, Main: main, Remote: "github.test/team/app"}, {Dir: clone, Main: clone, Remote: "github.test/team/app"}} {
				err = s.SavePlacement(t.Context(), localindex.Placement{Agent: transcript.AgentClaude, SessionID: string(rune('a' + i)), How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: d.Dir, Worktree: d.Dir, Main: d.Main, Remote: d.Remote}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "missing-fts" {
				files, err := filepath.Glob(path + "*")
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range files {
					if strings.HasPrefix(p, path+"-tok") || strings.HasPrefix(p, path+"-tri") {
						if err = os.Remove(p); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			asCaller(t, claudeSelf)
			fa := startFakeAgent(t, func(r agent.Request) agent.Response {
				return agent.Response{OK: true, Peers: &busproto.PeersResponse{Peers: []busproto.Peer{{Session: peerID, Agent: "codex", User: "g@example.test", Repo: clone, Main: clone, Remote: "github.test/team/app"}}}}
			})
			t.Setenv("FLOPWIRE_INDEX", path)
			dirs := localRepoDirs(t.Context(), path)
			if len(dirs) != 2 {
				t.Fatalf("retained dirs = %+v", dirs)
			}
			for _, repo := range []string{main, gone, clone} {
				out := captureStdout(t, func() error { return run(t.Context(), []string{"sessions", "--server", "--repo", repo}) })
				if !strings.Contains(out, "authorized") {
					t.Fatalf("missing authorized result: %s", out)
				}
				q := queries[len(queries)-1]
				if !slices.Contains(q["repo_checkout"], main) || !slices.Contains(q["repo_checkout"], clone) || q.Get("repo_remote") != "github.test/team/app" {
					t.Fatalf("lost recorded identity: %v", q)
				}
				if want[repo] == "" {
					want[repo] = q.Encode()
				} else if q.Encode() != want[repo] {
					t.Fatalf("scope changed: got %s want %s", q.Encode(), want[repo])
				}
			}

			peers, _, err := cliJSON(t, fa, "", "peers", "--repo", gone)
			if err != nil || !strings.Contains(peers, peerID) {
				t.Fatalf("peers: %s %v", peers, err)
			}
			pq := fa.requests()[0].Peers
			if !slices.Contains(pq.Mains, main) || !slices.Contains(pq.Mains, clone) || !slices.Contains(pq.Remotes, "github.test/team/app") {
				t.Fatalf("peers scope: %+v", pq)
			}
			var messages bytes.Buffer
			cfg := client.Config{Server: srv.URL, Token: "device", DeviceID: "enrolled-device"}
			if err := runMessages(t.Context(), busproto.MessagesQuery{Repo: gone}, &messages, busStyle{JSON: true}, func() (client.Config, error) { return cfg, nil }); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(messages.String(), "authorized-message") {
				t.Fatalf("messages: %s", messages.String())
			}
			mq := queries[len(queries)-1]
			if !slices.Contains(mq["repo_checkout"], main) || !slices.Contains(mq["repo_checkout"], clone) || mq.Get("repo_remote") != "github.test/team/app" {
				t.Fatalf("messages scope: %v", mq)
			}
			if mode == "sync-only" {
				if err := run(t.Context(), []string{"sessions", "--local"}); err == nil {
					t.Fatal("sync-only accepted local retrieval")
				}
			}
		})
	}
}
