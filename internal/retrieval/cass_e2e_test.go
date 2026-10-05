package retrieval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCASSRecoveryRetrieval(t *testing.T) {
	ctx := context.Background()
	s := newServer(t)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.Encode(cassimport.Record{Version: 1, Conversation: &transcript.Conversation{Agent: transcript.AgentClaude, SessionID: "cass-only-session", Cwd: "/work/recovery", Extra: map[string]any{"recovered_history": true, "cass_source_path": "/gone/actual-source.jsonl"}}})
	enc.Encode(cassimport.Record{Version: 1, Message: &transcript.Message{SessionID: "cass-only-session", NativeID: "cass:original-mac:1", Kind: transcript.KindUser, Role: "user", Text: "recovery unique needle"}})
	p := filepath.Join(t.TempDir(), "recovery.jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.sy.Sync(ctx, devicesync.SourceSpec{Path: p, Agent: transcript.AgentClaude, StorageKind: cassimport.StorageKind, Parser: cassimport.Name, SessionKey: "cass-only-session"}); err != nil {
		t.Fatal(err)
	}
	if err := s.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := s.client.Search(ctx, format.SearchQuery{Query: "recovery unique needle"}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 {
		t.Fatalf("search: %#v", page)
	}
	h := page.Hits[0]
	if h.Provenance.EvidenceKind != "cass_recovery" || h.Provenance.OriginalPath != "/gone/actual-source.jsonl" {
		t.Fatalf("search provenance: %#v", h.Provenance)
	}
	cx, err := s.client.Read(ctx, format.ReadQuery{Address: h.Address}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.Messages) != 1 || cx.Messages[0].Text != "recovery unique needle" || cx.Messages[0].Provenance.EvidenceKind != "cass_recovery" {
		t.Fatalf("read: %#v", cx)
	}
	raw, err := s.client.Raw(ctx, h.Provenance.SourceID, h.Provenance.Generation, *h.Provenance.ByteOffset, h.Provenance.ByteLen)
	if err != nil {
		t.Fatal(err)
	}
	var rec cassimport.Record
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Message == nil || rec.Message.Text != "recovery unique needle" {
		t.Fatal("raw failed to return normalized recovery evidence")
	}
	grep, err := s.client.Grep(ctx, format.GrepQuery{Pattern: "unique needle", Fixed: true}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if len(grep.Hits) != 1 || grep.Hits[0].Provenance.EvidenceKind != "cass_recovery" {
		t.Fatal("grep lost recovery provenance")
	}
	var rendered bytes.Buffer
	// JSON carries the same explicit marker even when an agent uses its own renderer.
	if err = json.NewEncoder(&rendered).Encode(grep); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), "cass_recovery") {
		t.Fatal("missing evidence label")
	}
}
