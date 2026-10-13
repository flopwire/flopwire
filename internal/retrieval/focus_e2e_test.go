package retrieval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/retrieval/local/localtest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/google/uuid"
)

// The same ingested transcript is read through the local index and HTTP server.
// A huge preceding line must not consume an implicit citation's text budget.
func TestFocusedReadLocalServerParity(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	session := uuid.NewString()
	body := strings.Repeat("x", 2000) + "\nnearby\nrationale: jitter prevents synchronized retries é\nlater evidence"
	record, err := json.Marshal(map[string]any{
		"type": "assistant", "sessionId": session, "uuid": uuid.NewString(),
		"cwd": "/tmp/focused-read", "gitBranch": "main", "timestamp": "2026-10-13T12:00:00Z",
		"message": map[string]any{"role": "assistant", "content": body},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.home, ".claude", "projects", "-tmp-focused-read", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(record, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	index, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { index.Close() })
	if err := localtest.IndexHome(ctx, index, s.home); err != nil {
		t.Fatal(err)
	}
	lb := &local.Backend{Store: index}
	for _, tc := range []struct {
		name                           string
		line, offset, budget, from, to int
		want                           string
	}{
		{"implicit citation", 3, 0, 500, 2, 4, "nearby\nrationale: jitter prevents synchronized retries é\nlater evidence"},
		{"target alone", 3, 0, 50, 3, 3, "rationale: jitter prevents synchronized retries é"},
		{"oversized target", 3, 0, 10, 3, 3, "rationale:"},
		{"explicit earlier offset", 3, 1, 500, 1, 1, strings.Repeat("x", 500)},
		{"explicit later offset", 3, 4, 500, 4, 4, "later evidence"},
		{"uncited", 0, 0, 500, 1, 1, strings.Repeat("x", 500)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The parser's first message ordinal is not assumed: obtain it from
			// the citation-bearing search result for each backend below.
			reads := []struct {
				name string
				grep func(context.Context, format.GrepQuery, format.Filters) (*format.Page, error)
				read func(context.Context, format.ReadQuery, format.Filters) (*format.Context, error)
			}{{"local", lb.Grep, lb.Read}, {"server", s.client.Grep, s.client.Read}}
			var first format.Excerpt
			for i, backend := range reads {
				p, err := backend.grep(ctx, format.GrepQuery{Pattern: "rationale", Limit: 1}, format.Filters{})
				if err != nil || len(p.Hits) != 1 {
					t.Fatalf("%s search: %+v %v", backend.name, p, err)
				}
				address := p.Hits[0].Address
				if tc.line > 0 {
					address += fmt.Sprintf(":%d", tc.line)
				}
				cx, err := backend.read(ctx, format.ReadQuery{Address: address, LineOffset: tc.offset, MaxChars: tc.budget}, format.Filters{})
				if err != nil || len(cx.Messages) != 1 {
					t.Fatalf("%s read: %+v %v", backend.name, cx, err)
				}
				m := cx.Messages[0]
				got := format.Excerpt{Text: m.Text, From: m.LineFrom, To: m.LineTo, Lines: m.Lines, Clipped: m.Clipped}
				if got.Text != tc.want || got.From != tc.from || got.To != tc.to || got.Lines != 4 || !got.Clipped {
					t.Fatalf("%s excerpt: %+v", backend.name, got)
				}
				if i == 0 {
					first = got
				} else if !reflect.DeepEqual(first, got) {
					t.Fatalf("local/server differ: %+v / %+v", first, got)
				}
				if tc.name == "target alone" {
					var rendered strings.Builder
					if err := format.WriteRead(&rendered, cx, format.Style{}); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(rendered.String(), ">    3  rationale:") || !strings.Contains(rendered.String(), "--line-offset 4") {
						t.Fatalf("%s focus/continuation: %s", backend.name, rendered.String())
					}
					continued, err := backend.read(ctx, format.ReadQuery{Address: m.Address, LineOffset: 4, MaxChars: tc.budget}, format.Filters{})
					if err != nil || continued.Messages[0].Text != "later evidence" {
						t.Fatalf("%s continuation: %+v %v", backend.name, continued, err)
					}
				}
			}
		})
	}
}
