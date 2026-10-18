package local

import (
	"crypto/sha256"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestCorpusReferencePortableWitness(t *testing.T) {
	s, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic/portable.jsonl", Parser: "test", StorageKind: transcript.StorageJSONLAppend})
	if err != nil {
		t.Fatal(err)
	}
	var messages []*transcript.Message
	for i, text := range []string{"PortableWitness ordinary prose", "internal/transcript", "internal/transcript", "internal/transcript"} {
		m := &transcript.Message{SessionID: []string{"visible", "injected", "inactive", "retired"}[i], NativeID: "one", Ordinal: 0, Kind: transcript.KindUser, Text: text, FullLen: len(text), ContentSHA: sha256.Sum256([]byte(text))}
		if i == 1 {
			m.Kind = transcript.KindInjected
		}
		if i == 2 {
			active := false
			m.OnActivePath = &active
		}
		messages = append(messages, m)
	}
	if _, err := s.ApplyBatch(ctx, localindex.Batch{SourceID: src.ID, Generation: 1, Messages: messages}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SupersedeSession(ctx, transcript.AgentClaude, "retired", 2); err != nil {
		t.Fatal(err)
	}
	queries := []corpusQuery{{"find", "internal/transcript", false}, {"regex", `func \w+\(`, true}, {"search", "sqlite fts5 trigram", false}}
	present, witness, token, err := corpusReference(s, queries)
	if err != nil {
		t.Fatal(err)
	}
	for _, yes := range present {
		if yes {
			t.Fatal("hidden or absent smoke phrase counted as visible")
		}
	}
	if witness.SessionID != "visible" || token != "PortableWitness" {
		t.Fatal("missing guaranteed visible witness")
	}
	b := &Backend{Store: s}
	for _, q := range []format.GrepQuery{{Pattern: token, Fixed: true}, {Pattern: regexp.QuoteMeta(token)}} {
		page, err := b.Grep(ctx, q, format.Filters{})
		if err != nil {
			t.Fatal(err)
		}
		if err := corpusPresence(true, page); err != nil {
			t.Fatal(err)
		}
	}
	page, err := b.Search(ctx, format.SearchQuery{Query: token}, format.Filters{})
	if err != nil {
		t.Fatal(err)
	}
	if err := corpusPresence(true, page); err != nil {
		t.Fatal(err)
	}
	// An implementation that drops a real positive must fail, even if it
	// labels the empty answer incomplete. Absence never follows truncation.
	for _, page := range []*format.Page{{}, {Truncated: true}} {
		if corpusPresence(true, page) == nil {
			t.Fatal("omitted positive accepted")
		}
	}
	if corpusPresence(false, &format.Page{Truncated: true}) == nil {
		t.Fatal("incomplete absence accepted")
	}
	if err := corpusPresence(false, &format.Page{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOPWIRE_CORPUS", "1")
	t.Setenv("FLOPWIRE_CORPUS_DB", s.Path())
	TestCorpus(t)
}
