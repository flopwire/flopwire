package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"strings"
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

// Exercise JSON-RPC, the authenticated HTTP transport, real API handlers,
// PostgreSQL candidate selection, and MCP completeness reporting together.
func TestMCPScopedGrepEndToEndServer(t *testing.T) {
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	pg := store.NewPostgres(pool, nil, "")
	server := httptest.NewServer(api.New(pg, api.Config{Logger: quiet, Retrieval: &retrieval.Store{Pool: pool}}).Handler(nil))
	t.Cleanup(server.Close)
	owner, login := seedAdmin(t, pg)
	enrolled := apiCall(t, server.URL, login, "POST", "/v1/devices", map[string]string{"name": "grep-test", "platform": "darwin"})
	device := enrolled["device"].(map[string]any)["id"].(string)
	r := &retriever{backend: client.HTTP{Server: server.URL, Token: enrolled["token"].(string)}}
	now := time.Now().UTC()
	addSession := func(session string) string {
		t.Helper()
		conv := uuid.NewString()
		_, err := pool.Exec(ctx, `INSERT INTO conversations(id,agent,session_id,device_id,user_id,repo_root,last_activity_at) VALUES($1,'codex',$2,$3,$4,'/synthetic/grep',$5)`, conv, session, device, owner, now)
		if err != nil {
			t.Fatal(err)
		}
		return conv
	}
	addMessage := func(conv string, ordinal int, body string, at time.Time) {
		t.Helper()
		sum := sha256.Sum256([]byte(body))
		_, err := pool.Exec(ctx, `INSERT INTO messages(id,conversation_id,ordinal,kind,ts,text,text_len,content_sha,source_generation,parser) VALUES($1,$2,$3,'assistant',$4,$5,$6,$7,0,'test')`, uuid.NewString(), conv, ordinal, at, body, len(body), sum[:])
		if err != nil {
			t.Fatal(err)
		}
	}
	const selected = "synthetic-grep-selected"
	conv := addSession(selected)
	for i, body := range []string{"server 4040", "kickstart", "version 0.31.0", "wildcard 0x31y0"} {
		addMessage(conv, i+1, body, now.Add(-time.Minute))
	}
	addMessage(conv, 5, "old 0z31q0", now.Add(-time.Hour))
	addMessage(addSession("synthetic-grep-other"), 1, "other 0a31b0", now.Add(-time.Minute))
	addMessage(addSession("synthetic-grep-large"), 1, strings.Repeat("z", (32<<20)+1), now.Add(-time.Minute))

	type result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	call := func(args map[string]any) result {
		t.Helper()
		args["include_self"] = true
		request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "flopwire_grep", "arguments": args}}
		input, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := serveMCP(ctx, r, bytes.NewReader(append(input, '\n')), &output); err != nil {
			t.Fatal(err)
		}
		var response struct {
			Result result          `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(output.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v: %s", err, &output)
		}
		if len(response.Error) != 0 || len(response.Result.Content) != 1 {
			t.Fatalf("unexpected response: %s", &output)
		}
		return response.Result
	}
	page := func(out result) format.Page {
		t.Helper()
		var p format.Page
		if err := json.Unmarshal([]byte(out.Content[0].Text), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	for _, tc := range []struct {
		name, pattern, session string
		want                   int
		scanned                bool
	}{
		{"scoped original", `4040|kickstart|0.31.0`, selected, 4, true},
		{"unscoped escaped", `4040|kickstart|0\.31\.0`, "", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"pattern": tc.pattern, "since": "30m", "format": "json", "limit": 5}
			if tc.session != "" {
				args["session"] = tc.session
			}
			out := call(args)
			p := page(out)
			if out.IsError || !p.Exact || p.Truncated || len(p.Hits) != tc.want || (len(p.Notes) > 0) != tc.scanned {
				t.Fatalf("unexpected results: %+v %+v", out, p)
			}
			wantTexts := map[string]bool{"server 4040": true, "kickstart": true, "version 0.31.0": true}
			if tc.scanned {
				wantTexts["wildcard 0x31y0"] = true
			}
			for _, hit := range p.Hits {
				if hit.SessionID != selected {
					t.Fatalf("scope leaked: %+v", hit)
				}
				for _, line := range hit.Lines {
					if line.Match {
						if !wantTexts[line.Text] {
							t.Fatalf("unexpected matched text: %q", line.Text)
						}
						delete(wantTexts, line.Text)
					}
				}
			}
			if len(wantTexts) != 0 {
				t.Fatalf("missing matched texts: %v", wantTexts)
			}
		})
	}
	bad := call(map[string]any{"pattern": `4040|kickstart|0.31.0`, "since": "30m"})
	if !bad.IsError || !strings.Contains(bad.Content[0].Text, "specify a session") {
		t.Fatalf("unactionable broad error: %+v", bad)
	}
	for _, mode := range []string{"json", "text"} {
		t.Run(mode+" empty versus incomplete", func(t *testing.T) {
			empty := call(map[string]any{"pattern": "q", "session": selected, "since": "30m", "format": mode})
			limited := call(map[string]any{"pattern": "q", "session": "synthetic-grep-large", "since": "30m", "format": mode})
			if empty.IsError || !limited.IsError || !strings.Contains(limited.Content[0].Text, "verification limit") {
				t.Fatalf("completeness lost: empty=%+v limited=%+v", empty, limited)
			}
			if mode == "json" {
				complete, partial := page(empty), page(limited)
				if !complete.Exact || complete.Truncated || len(complete.Hits) != 0 || partial.Exact || !partial.Truncated || len(partial.Hits) != 0 {
					t.Fatalf("bad completeness: %+v %+v", complete, partial)
				}
			}
		})
	}
}
