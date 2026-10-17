package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// Join device-side expansion, CLI/MCP entrypoints, authenticated HTTP, and the
// real PostgreSQL evidence index. Unit tests at each seam missed the original
// rejection of a repository whose name had expanded into checkout/remote filters.
func TestChangeExpandedRepoEndToEndServer(t *testing.T) {
	s := newAcceptServer(t)
	server := httptest.NewServer(api.New(store.NewPostgres(s.pool, nil, ""), api.Config{Logger: quiet, Retrieval: &retrieval.Store{Pool: s.pool}}).Handler(nil))
	t.Cleanup(server.Close)
	enrolled := apiCall(t, server.URL, s.gary, "POST", "/v1/devices", map[string]string{"name": "change-test", "platform": "test"})
	device := enrolled["device"].(map[string]any)["id"].(string)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv(client.EnvToken, "")
	t.Setenv(client.EnvServer, "")
	cfg := client.Config{Server: server.URL, Token: enrolled["token"].(string), DeviceID: device}
	if err := client.Save(cfg); err != nil {
		t.Fatal(err)
	}
	establishEmptyCollectionPolicy(t, cfg)
	index := filepath.Join(t.TempDir(), "index.db")
	t.Setenv("FLOPWIRE_INDEX", index)
	db, err := localindex.Open(index, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	const checkout = "/synthetic/change-app"
	if err := db.SavePlacement(t.Context(), localindex.Placement{Agent: transcript.AgentCodex, SessionID: "change-selected", How: localindex.PlacedByWorktree, Placement: pathpolicy.Placement{Cwd: checkout, Worktree: checkout, Main: checkout, Remote: "github.com/example/app"}}); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 40)
	var command, result string
	var conversations []string
	for _, row := range []struct{ session, checkout, remote string }{
		{"change-selected", checkout, "github.com/example/app"},
		{"change-unrelated", "/synthetic/other", "github.com/example/other"},
	} {
		source, conv := uuid.NewString(), uuid.NewString()
		conversations = append(conversations, conv)
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,checkout,remote) VALUES($1,$2,'codex',$3,'synthetic','jsonl_append','test',now(),$4,$5)`, source, device, "/synthetic/"+row.session, row.checkout, row.remote); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd) VALUES($1,$2,'codex',$3,$4,$5,$6)`, conv, source, row.session, device, s.garyID, row.checkout); err != nil {
			t.Fatal(err)
		}
		call := uuid.NewString()
		body, _ := json.Marshal(map[string]string{"cmd": "git commit -m synthetic", "workdir": row.checkout})
		for ordinal, text := range []string{string(body), "[topic " + hash + "] synthetic"} {
			id, kind := uuid.NewString(), "tool_call"
			if ordinal == 1 {
				kind = "tool_result"
			}
			sum := sha256.Sum256([]byte(text))
			if _, err := s.pool.Exec(t.Context(), `INSERT INTO messages(id,conversation_id,source_id,native_id,ordinal,kind,tool_name,tool_call_id,text,text_len,content_sha,source_generation,parser) VALUES($1::uuid,$2,$3,$1::uuid::text,$4,$5,'exec_command',$6,$7,$8,$9,0,'test')`, id, conv, source, ordinal+1, kind, call, text, len(text), sum[:]); err != nil {
				t.Fatal(err)
			}
			if row.session == "change-selected" {
				if ordinal == 0 {
					command = id
				} else {
					result = id
				}
			}
		}
	}
	// Complete fixture indexing before testing lookup. Production's lazy historical
	// backfill has a 500ms budget and is intentionally allowed to report indexing lag.
	if err := ingest.BackfillCommitEvidenceFor(t.Context(), s.pool, conversations, len(conversations)); err != nil {
		t.Fatal(err)
	}
	var dirty int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM commit_evidence_dirty WHERE conversation_id=ANY($1::uuid[])`, conversations).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("fixture index not ready: dirty=%d error=%v", dirty, err)
	}
	check := func(t *testing.T, output string) {
		t.Helper()
		var page format.Changes
		if err := json.Unmarshal([]byte(output), &page); err != nil {
			t.Fatalf("decode: %v: %s", err, output)
		}
		if len(page.Matches) != 1 || page.HasMore || page.RestartRequired {
			t.Fatalf("page: %+v", page)
		}
		match := page.Matches[0]
		if match.Session.SessionID != "change-selected" || match.ObservedHash != hash || match.CommandAddress != command || match.ResultAddress != result {
			t.Fatalf("evidence/scope: %+v", match)
		}
	}
	// No --server override: an enrolled device must select the shared backend.
	for _, verb := range []string{"trace", "change"} {
		t.Run("CLI/"+verb, func(t *testing.T) {
			check(t, captureStdout(t, func() error {
				return run(t.Context(), []string{verb, "--repo", "example/app", "--commits", hash, "--limit", "1"})
			}))
		})
	}
	r, err := openRetriever(true, index)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	scope, err := r.teamRepo(t.Context(), "example/app")
	if err != nil || scope.Repo != "" || len(scope.Remotes) == 0 {
		t.Fatalf("fixture did not expand repository: %+v %v", scope, err)
	}
	t.Run("MCP", func(t *testing.T) {
		input, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "flopwire_change", "arguments": map[string]any{"repo": "example/app", "commits": []string{hash}, "limit": 1}}})
		var output bytes.Buffer
		if err := serveMCP(t.Context(), r, bytes.NewReader(append(input, '\n')), &output); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(output.Bytes(), &response); err != nil || len(response.Error) != 0 || response.Result.IsError || len(response.Result.Content) != 1 {
			t.Fatalf("MCP response: %s: %v", &output, err)
		}
		check(t, response.Result.Content[0].Text)
	})
}
