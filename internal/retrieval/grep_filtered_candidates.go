package retrieval

import (
	"context"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Metadata selection is an optimization, not a candidate cap. The extra row
// proves when the entire eligible set does not fit; then the original text
// index query runs without any message-ID restriction.
const grepMetadataCap = 20000

func grepMetadataUseful(f format.Filters) bool {
	return len(f.Kinds) > 0 || len(f.ExcludeKinds) > 0 || len(f.Tools) > 0 || !f.Since.IsZero() || !f.Until.IsZero()
}

func grepMetadataQuery(q *query) string {
	return `SELECT m.id FROM ` + from + ` WHERE ` + q.sql() + ` LIMIT ` + strconv.Itoa(grepMetadataCap+1)
}

func grepMetadataCandidates(ctx context.Context, tx pgx.Tx, f format.Filters, conversations []string) ([]uuid.UUID, bool, error) {
	q := &query{}
	if err := hitFilters(q, f); err != nil {
		return nil, false, err
	}
	q.where("m.conversation_id=ANY(" + q.arg(conversations) + "::uuid[])")
	rows, err := tx.Query(ctx, grepMetadataQuery(q), q.args...)
	if err != nil {
		return nil, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, false, err
	}
	if len(ids) > grepMetadataCap {
		return nil, false, nil
	}
	return ids, true, nil
}

// The complete small metadata cohort is probed by primary key before its
// text is evaluated. OFFSET 0 keeps the correlated lookup separate: allowing
// it to flatten would permit the global trigram bitmap to recheck large,
// irrelevant tool bodies before kind/time/session filters are applied.
// Final filters, ordering and the text-qualified cap remain unchanged.
func grepFilteredCandidates(q *query, ids []uuid.UUID, oldest bool) string {
	probes := `unnest(` + q.arg(ids) + `::uuid[]) eligible(id)
 JOIN LATERAL (SELECT * FROM messages WHERE id=eligible.id OFFSET 0) m ON true`
	filteredFrom := probes + strings.TrimPrefix(from, "messages m")
	order := ` ORDER BY m.ts DESC NULLS LAST, m.id LIMIT `
	if oldest {
		order = ` ORDER BY m.ts NULLS LAST, m.id LIMIT `
	}
	return `SELECT m.id FROM ` + filteredFrom + ` WHERE ` + q.sql() + order + strconv.Itoa(maxCandidates)
}
