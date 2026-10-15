package main

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// Identical local placement metadata must select identical literal session IDs
// through the real authenticated API and PostgreSQL repository predicates.
func TestRetainedRepoMetadataRealServerResultParity(t *testing.T) {
	s := newAcceptServer(t)
	srv := httptest.NewServer(api.New(store.NewPostgres(s.pool, nil, ""), api.Config{Logger: quiet, Retrieval: &retrieval.Store{Pool: s.pool}}).Handler(nil))
	defer srv.Close()
	enrolled := apiCall(t, srv.URL, s.gary, "POST", "/v1/devices", map[string]string{"name": "parity", "platform": "test"})
	device := enrolled["device"].(map[string]any)["id"].(string)
	cc := client.Config{Server: srv.URL, Token: enrolled["token"].(string), DeviceID: device}
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv(client.EnvServer, "")
	t.Setenv(client.EnvToken, "")
	if err := client.Save(cc); err != nil {
		t.Fatal(err)
	}
	establishEmptyCollectionPolicy(t, cc)
	base := t.TempDir()
	main, gone, clone := filepath.Join(base, "app"), filepath.Join(base, "gone"), filepath.Join(base, "clone")
	for _, p := range []string{main, clone} {
		if out, err := exec.Command("git", "init", "-q", p).CombinedOutput(); err != nil {
			t.Fatalf("git init: %s %v", out, err)
		}
		if out, err := exec.Command("git", "-C", p, "remote", "add", "origin", "https://github.test/team/app.git").CombinedOutput(); err != nil {
			t.Fatalf("git remote: %s %v", out, err)
		}
	}
	for _, row := range []struct {
		id, checkout, remote string
		hidden               bool
	}{
		{"parity-main", main, "github.test/team/app", false},
		{"parity-clone", clone, "github.test/team/app", false},
		{"parity-unrelated", filepath.Join(base, "other"), "github.test/team/other", false},
		{"parity-denied", main, "github.test/team/app", true},
	} {
		src, conv := uuid.NewString(), uuid.NewString()
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,checkout,remote) VALUES($1,$2,'claude',$3,'f','jsonl_append','test',now(),$4,$5)`, src, device, "/synthetic/"+row.id, row.checkout, row.remote); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd) VALUES($1,$2,'claude',$3,$4,$5,$6)`, conv, src, row.id, device, s.garyID, row.checkout); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `UPDATE conversation_activity SET last_activity_at=$2 WHERE conversation_id=$1`, conv, time.Now()); err != nil {
			t.Fatal(err)
		}
		if row.hidden {
			if _, err := s.pool.Exec(t.Context(), `UPDATE conversations SET hidden_at=now() WHERE id=$1`, conv); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, syncOnly := range []bool{false, true} {
		p := filepath.Join(t.TempDir(), "index.db")
		db, err := localindex.Open(p, localindex.Options{SyncOnly: syncOnly})
		if err != nil {
			t.Fatal(err)
		}
		for i, d := range []localindex.RepoDir{{Dir: gone, Main: main, Remote: "github.test/team/app"}, {Dir: clone, Main: clone, Remote: "github.test/team/app"}} {
			if err := db.SavePlacement(t.Context(), localindex.Placement{Agent: transcript.AgentClaude, SessionID: string(rune('a' + i)), How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: d.Dir, Worktree: d.Dir, Main: d.Main, Remote: d.Remote}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLOPWIRE_INDEX", p)
		for _, repo := range []string{main, gone, clone} {
			out := captureStdout(t, func() error {
				return run(t.Context(), []string{"sessions", "--server", "--repo", repo, "--include-self"})
			})
			var result format.Sessions
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("sessions JSON: %s %v", out, err)
			}
			var ids []string
			for _, session := range result.Sessions {
				ids = append(ids, session.SessionID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, []string{"parity-clone", "parity-main"}) {
				t.Fatalf("syncOnly=%v repo=%s result=%v", syncOnly, repo, ids)
			}
		}
	}
}
