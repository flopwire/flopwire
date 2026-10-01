package ingest

import (
	"context"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
)

// The ingest lookups by source (notes/perf-guards.md, known violation #3)
// use an index: refusing a source, the parse-time check for stored
// conversations, applying rules to one source's conversations, and
// retiring a source's live rows (which shares the messages source index).
func TestPerfSourceLookupPlansUseIndexes(t *testing.T) {
	pool, _ := perfguard.NewPool(t)
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	ids := []string{id, uuid.NewString()}
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"refuse: conversations", refuseConversationsSQL, []any{ids, uuid.NewString(), "claude", []string{"s1"}}},
		{"refuse: messages", refuseMessagesSQL, []any{ids}},
		{"parse: had stored", hadStoredSQL, []any{id}},
		{"rules: one source's conversations", convRowsSQL + ` WHERE ($1='' OR c.source_id=$1::uuid) ORDER BY c.depth,c.id`, []any{id}},
		{"parse: list retire candidates", retireCandidatesSQL, []any{id, int64(2), int64(1)}},
		{"parse: retire absent rows", retireIDsSQL, []any{[]string{id}, int64(2)}},
		{"parse: retire previous source's live rows", retirePreviousSQL, []any{id, int64(2)}},
		{"parse: lock checkpoint conversations", lockCheckpointSQL, []any{ids, ids, id}},
	} {
		t.Run(c.name, func(t *testing.T) {
			perfguard.AssertIndexedPlan(t, pool, c.sql, c.args...)
		})
	}
}
