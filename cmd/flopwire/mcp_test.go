package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// blockingBackend's Grep waits until its context ends and reports that.
type blockingBackend struct {
	started   chan struct{}
	cancelled chan struct{}
}

func (b *blockingBackend) Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error) {
	b.started <- struct{}{}
	<-ctx.Done()
	close(b.cancelled)
	return nil, ctx.Err()
}
func (b *blockingBackend) Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error) {
	return &format.Page{}, nil
}
func (b *blockingBackend) Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error) {
	return &format.Sessions{}, nil
}
func (b *blockingBackend) Read(ctx context.Context, q format.ReadQuery, f format.Filters) (*format.Context, error) {
	return &format.Context{}, nil
}
func (b *blockingBackend) RawAt(ctx context.Context, address string) ([]byte, error) { return nil, nil }
func (b *blockingBackend) Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, error) {
	return nil, nil
}

// C1, D8: a slow call does not block the server: ping and other calls
// answer meanwhile, and notifications/cancelled stops it without a
// response. C3: a request line over 4MB gets a JSON-RPC error and the
// server reads on.
func TestMCPConcurrencyCancellationAndLongLines(t *testing.T) {
	b := &blockingBackend{started: make(chan struct{}, 1), cancelled: make(chan struct{})}
	r := &retriever{backend: b}
	in, feed := io.Pipe()
	outR, out := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- serveMCP(t.Context(), r, in, out); out.Close() }()
	lines := make(chan string, 16)
	go func() {
		buf := make([]byte, 0, 1<<16)
		chunk := make([]byte, 1<<16)
		for {
			n, err := outR.Read(chunk)
			buf = append(buf, chunk[:n]...)
			for {
				i := strings.IndexByte(string(buf), '\n')
				if i < 0 {
					break
				}
				lines <- string(buf[:i])
				buf = buf[i+1:]
			}
			if err != nil {
				close(lines)
				return
			}
		}
	}()
	send := func(s string) {
		if _, err := io.WriteString(feed, s+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(5 * time.Second):
			t.Fatal("no response")
		}
		return ""
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"slow"}}}`)
	<-b.started
	send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if l := next(); !strings.Contains(l, `"id":2`) {
		t.Fatalf("ping while a call runs: %s", l)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"flopwire_sessions","arguments":{}}}`)
	if l := next(); !strings.Contains(l, `"id":3`) || !strings.Contains(l, `"text":"{\"kind\":\"sessions\",\"sessions\":[],\"has_more\":false,\"hint\":\"Adjust the filters if more evidence is needed.\"}"`) || strings.Contains(l, "structuredContent") {
		t.Fatalf("second call while the first runs: %s", l)
	}
	send(strings.Repeat("x", 5<<20))
	if l := next(); !strings.Contains(l, `"code":-32600`) || !strings.Contains(l, `"id":null`) {
		t.Fatalf("over-long line: %s", l)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user"}}`)
	select {
	case <-b.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not reach the call")
	}
	send(`{"jsonrpc":"2.0","id":4,"method":"ping"}`)
	if l := next(); !strings.Contains(l, `"id":4`) {
		t.Fatalf("a cancelled call answered, or ping did not: %s", l)
	}
	feed.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for l := range lines {
		t.Fatalf("unexpected response after cancel: %s", l)
	}
}

// Every tool but flopwire_send says it only reads (so a client may run it
// unasked and in parallel); send says it writes to other people's
// sessions and is not idempotent. Every argument is described in the
// schema itself: a client may drop the server instructions. An unknown
// argument's error lists the ones the tool takes.
func TestMCPToolSchemasSelfDescribing(t *testing.T) {
	for _, tl := range mcpTools() {
		tm := tl.(map[string]any)
		ann := tm["annotations"].(map[string]any)
		if tm["name"] == "flopwire_send" {
			if ann["readOnlyHint"] != false || ann["openWorldHint"] != true || ann["destructiveHint"] != false || ann["idempotentHint"] != false || tm["title"] == "" {
				t.Errorf("%s annotations: %v", tm["name"], ann)
			}
		} else if ann["readOnlyHint"] != true || ann["openWorldHint"] != false || ann["destructiveHint"] != false || tm["title"] == "" {
			t.Errorf("%s annotations: %v", tm["name"], ann)
		}
		for k, v := range tm["inputSchema"].(map[string]any)["properties"].(map[string]any) {
			if d, _ := v.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s: argument %s has no description", tm["name"], k)
			}
		}
	}
	_, _, err := mcpOpts("flopwire_grep", map[string]any{"pattern": "x", "glob": "01a0"})
	if err == nil || !strings.Contains(err.Error(), `unknown argument "glob"; it takes pattern, `) || !strings.Contains(err.Error(), "session") {
		t.Fatalf("unknown argument: %v", err)
	}
}

// A timeout with no verified matches is an unsuccessful MCP grep call.
// Useful partial hits and complete empty answers keep the one-answer contract.
type outcomeBackend struct{ blockingBackend }

func (*outcomeBackend) Grep(_ context.Context, q format.GrepQuery, _ format.Filters) (*format.Page, error) {
	switch q.Pattern {
	case "empty":
		return &format.Page{Exact: true, Hits: []format.Hit{}}, nil
	case "timeout":
		return &format.Page{Truncated: true, Reason: "timed out while fetching the first candidate page (checked 0 candidates); narrow with repo or since", Hits: []format.Hit{}}, nil
	case "partial":
		return &format.Page{Truncated: true, Total: 3, Reason: "timed out after checking candidates", Hits: []format.Hit{{SessionID: "synthetic-session", Ordinal: 1, Kind: "assistant", Snippet: "synthetic partial match", Lines: []format.Line{{N: 1, Text: "synthetic partial match", Match: true}}}}}, nil
	default:
		return &format.Page{Total: 200, Hits: []format.Hit{}}, nil // count cap, not a timeout
	}
}
func (b *outcomeBackend) Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error) {
	return &format.Page{Hits: []format.Hit{}}, nil
}

func TestMCPUnproductiveGrepTimeoutOnWire(t *testing.T) {
	cases := []struct {
		pattern    string
		isError    bool
		partialHit bool
	}{
		{"empty", false, false},
		{"timeout", true, false},
		{"partial", false, true},
		{"capped", false, false},
	}
	var input strings.Builder
	type expectedOutcome struct{ isError, partialHit bool }
	expected := map[int]expectedOutcome{}
	id := 0
	for _, mode := range []string{"text", "json"} {
		for _, tc := range cases {
			id++
			fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"flopwire_grep","arguments":{"pattern":"%s","format":"%s"}}}`+"\n", id, tc.pattern, mode)
			expected[id] = expectedOutcome{tc.isError, tc.partialHit}
		}
	}
	input.WriteString(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"flopwire_search","arguments":{"query":"synthetic"}}}` + "\n")
	var output bytes.Buffer
	if err := serveMCP(t.Context(), &retriever{backend: &outcomeBackend{}}, strings.NewReader(input.String()), &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	seen := map[int]bool{}
	searchResponses := 0
	for decoder.More() {
		var response struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				Structured any  `json:"structuredContent"`
				IsError    bool `json:"isError"`
			} `json:"result"`
		}
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Result.Structured != nil || len(response.Result.Content) != 1 {
			t.Fatalf("answer contract changed: %+v", response)
		}
		if response.ID == 99 {
			searchResponses++
			if response.Result.IsError {
				t.Fatalf("search inherits grep timeout outcome: %+v", response)
			}
			continue
		}
		want, ok := expected[response.ID]
		if !ok || seen[response.ID] {
			t.Fatalf("unexpected response: %+v", response)
		}
		seen[response.ID] = true
		text := response.Result.Content[0].Text
		if response.Result.IsError != want.isError {
			t.Fatalf("id %d isError=%v want %v", response.ID, response.Result.IsError, want.isError)
		}
		if want.isError && (!strings.Contains(text, "checked 0 candidates") || !strings.Contains(text, "narrow with repo or since")) {
			t.Fatalf("timeout diagnostic lost: %q", text)
		}
		if want.partialHit && !strings.Contains(text, "synthetic partial match") {
			t.Fatalf("verified partial hit lost: %q", text)
		}
	}
	if searchResponses != 1 || len(seen) != len(expected) {
		t.Fatalf("got %d grep and %d search responses", len(seen), searchResponses)
	}
}
