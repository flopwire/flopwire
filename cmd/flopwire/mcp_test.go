package main

import (
	"context"
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
func (b *blockingBackend) Sessions(ctx context.Context, glob string, offset int, f format.Filters) (*format.Sessions, error) {
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
	if l := next(); !strings.Contains(l, `"id":3`) || !strings.Contains(l, "no sessions") {
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

// Every tool says it only reads (so a client may run it unasked and in
// parallel), and every argument is described in the schema itself: a
// client may drop the server instructions. An unknown argument's error
// lists the ones the tool takes.
func TestMCPToolSchemasSelfDescribing(t *testing.T) {
	for _, tl := range mcpTools() {
		tm := tl.(map[string]any)
		ann := tm["annotations"].(map[string]any)
		if ann["readOnlyHint"] != true || ann["openWorldHint"] != false || ann["destructiveHint"] != false || tm["title"] == "" {
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
