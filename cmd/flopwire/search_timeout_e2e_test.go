package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/retrieval"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Force real PostgreSQL timeouts without relying on corpus size or CPU speed.
// Exercise the authenticated HTTP response and the MCP wire outcome, then retry
// the same query after releasing the lock to prove that recovery is caller-owned.
func TestMCPSearchTimeoutEndToEndServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool, nil, "")
	handler := api.New(pg, api.Config{Logger: quiet, Retrieval: &retrieval.Store{Pool: pool}}).Handler(nil)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/search" {
			requests.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	owner, login := seedAdmin(t, pg)
	enrolled := apiCall(t, server.URL, login, "POST", "/v1/devices", map[string]string{"name": "search-timeout-test", "platform": "darwin"})
	device := enrolled["device"].(map[string]any)["id"].(string)
	conv := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root) VALUES($1,'codex','synthetic-search-timeout',$2,$3,'/synthetic/search')`, conv, device, owner); err != nil {
		t.Fatal(err)
	}
	body := "cobalt recovery evidence"
	sum := sha256.Sum256([]byte(body))
	if _, err := pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser) VALUES($1,$2,1,'assistant',now(),$3,$4,$5,0,'test')`, uuid.NewString(), conv, body, len(body), sum[:]); err != nil {
		t.Fatal(err)
	}
	r := &retriever{backend: client.HTTP{Server: server.URL, Token: enrolled["token"].(string)}}
	call := func(t *testing.T, query, mode, matching string, wantError bool) string {
		t.Helper()
		before := requests.Load()
		request := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_search","arguments":{"query":%q,"matching":%q,"format":%q,"timeout":1,"include_self":true}}}`, query, matching, mode)
		answer, isError, _ := mcpContent(t, mcpRoundTrip(t, r, request))
		if isError != wantError {
			t.Fatalf("isError=%v want %v: %s", isError, wantError, answer)
		}
		if got := requests.Load() - before; got != 1 {
			t.Fatalf("one caller request made %d HTTP searches; retry must remain caller-controlled", got)
		}
		return answer
	}
	for _, mode := range []string{"json", "text"} {
		for _, matching := range []string{"all", "auto"} {
			t.Run(mode+"/"+matching, func(t *testing.T) {
				empty := call(t, "absentneedle", mode, matching, false)
				if !strings.Contains(empty, "No matches in this query scope") {
					t.Fatalf("complete empty result lost: %s", empty)
				}
				lock, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Rollback(context.Background())
				if _, err := lock.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
				answer := call(t, "cobalt", mode, matching, true)
				if !strings.Contains(answer, "timed out after 1s before lexical matching probes or ranking finished") || !strings.Contains(answer, "cannot establish absence") {
					t.Fatalf("timeout or recovery diagnostic lost: %s", answer)
				}
				if mode == "json" {
					var page format.Page
					if err := json.Unmarshal([]byte(answer), &page); err != nil || !page.Truncated || len(page.Hits) != 0 {
						t.Fatalf("partial page lost: %s (%v)", answer, err)
					}
				}
				if err := lock.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				if recovered := call(t, "cobalt", mode, matching, false); !strings.Contains(recovered, body) {
					t.Fatalf("same-query retry did not recover: %s", recovered)
				}
			})
		}
	}
}
