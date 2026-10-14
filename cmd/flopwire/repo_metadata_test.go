package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sync/coverage" {
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
