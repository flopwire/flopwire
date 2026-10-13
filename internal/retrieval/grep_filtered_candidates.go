package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Metadata selection is an optimization, not a candidate cap. The extra row
// proves when the entire eligible set does not fit; then the original text
// index query runs without any message-ID restriction.
const grepMetadataCap = 20000

// The measured slow finite-OR query estimated 704 global text candidates,
// then rechecked large tool bodies before its metadata filter. Rare text
// predicates and plans already starting from metadata retain their access path.
// The gate changes only performance; estimates never restrict matching rows.
const grepTextCandidateFloor = 64

type grepCandidatePlan struct {
	Node     string              `json:"Node Type"`
	Relation string              `json:"Relation Name"`
	Index    string              `json:"Index Name"`
	Filter   string              `json:"Filter"`
	Rows     float64             `json:"Plan Rows"`
	Plans    []grepCandidatePlan `json:"Plans"`
}

func grepGlobalTextRows(p grepCandidatePlan) (float64, bool) {
	switch p.Node {
	case "Bitmap Index Scan":
		return p.Rows, p.Index == "messages_text_trgm_idx"
	case "BitmapOr":
		if len(p.Plans) == 0 {
			return 0, false
		}
		for _, child := range p.Plans {
			if _, ok := grepGlobalTextRows(child); !ok {
				return 0, false
			}
		}
		return p.Rows, true
	default:
		// In particular a BitmapAnd with conversation/metadata indexes already
		// narrows the heap recheck. Unknown shapes retain the original query.
		return 0, false
	}
}

func grepNeedsMetadataProbes(p grepCandidatePlan) bool {
	if p.Node == "Bitmap Heap Scan" && p.Relation == "messages" && p.Filter != "" && len(p.Plans) == 1 {
		if rows, ok := grepGlobalTextRows(p.Plans[0]); ok && rows > grepTextCandidateFloor {
			return true
		}
	}
	for _, child := range p.Plans {
		if grepNeedsMetadataProbes(child) {
			return true
		}
	}
	return false
}

// Planning itself can be expensive on a large trigram index. A savepoint
// lets only our short statement timeout fall back without aborting the read
// snapshot. Other errors and caller cancellation remain errors.
const grepPlanningBudget = 50 * time.Millisecond

func grepPlanNeedsMetadata(ctx context.Context, tx pgx.Tx, q *query, oldest bool, deadline time.Time) (bool, error) {
	if _, err := tx.Exec(ctx, `SAVEPOINT grep_planning`); err != nil {
		return false, err
	}
	left := time.Until(deadline)
	if left <= 0 {
		return false, context.DeadlineExceeded
	}
	if err := setTimeout(ctx, tx, min(left, grepPlanningBudget)); err != nil {
		return false, err
	}
	var raw []byte
	err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+grepCandidates(q, oldest), q.args...).Scan(&raw)
	if err != nil {
		var pgErr *pgconn.PgError
		if ctx.Err() != nil || time.Until(deadline) <= 0 || !errors.As(err, &pgErr) || pgErr.Code != "57014" || !strings.Contains(pgErr.Message, "statement timeout") {
			return false, err
		}
		if _, rollbackErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT grep_planning`); rollbackErr != nil {
			return false, rollbackErr
		}
	}
	if _, releaseErr := tx.Exec(ctx, `RELEASE SAVEPOINT grep_planning`); releaseErr != nil {
		return false, releaseErr
	}
	left = time.Until(deadline)
	if left <= 0 {
		return false, context.DeadlineExceeded
	}
	if timeoutErr := setTimeout(ctx, tx, left); timeoutErr != nil {
		return false, timeoutErr
	}
	if err != nil {
		return false, nil
	}
	var plans []struct{ Plan grepCandidatePlan }
	if json.Unmarshal(raw, &plans) != nil || len(plans) != 1 {
		return false, nil
	}
	return grepNeedsMetadataProbes(plans[0].Plan), nil
}

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
