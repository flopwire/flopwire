package perfguard

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Beginner is satisfied by *pgx.Conn, *pgxpool.Pool and *pgxpool.Conn.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// AssertIndexedPlan fails t when the plan for query, made with sequential
// scans disabled, still reads a whole table: a Seq Scan, or an Index Scan
// or Index Only Scan with no Index Cond (the planner's fallback when no
// index matches the predicate) that no Limit stops early. A Limit stops a
// scan early only through nodes that pass every row up unfiltered (not a
// Sort, a Filter, an inner join, or an init plan or subplan). Either means no
// index serves the query: a missing index, or a predicate that defeats
// one such as `id::text > $1`. The query is planned with args bound but
// not executed. The plan is printed on failure.
func AssertIndexedPlan(t testing.TB, conn Beginner, query string, args ...any) {
	t.Helper()
	AssertIndexedPlanExcept(t, conn, nil, query, args...)
}

// AssertIndexedPlanExcept is AssertIndexedPlan that tolerates full scans
// of the named tables (unqualified names, as the plan reports them), such
// as a tiny lookup table with no index.
func AssertIndexedPlanExcept(t testing.TB, conn Beginner, allow []string, query string, args ...any) {
	t.Helper()
	plan, err := Explain(conn, query, args...)
	if err != nil {
		t.Fatalf("perfguard: explain: %v\nquery: %s", err, query)
	}
	var bad []string
	for _, s := range FullScans(plan) {
		if !slices.Contains(allow, s.Relation) {
			bad = append(bad, s.String())
		}
	}
	if len(bad) > 0 {
		t.Errorf("perfguard: unbounded scan with enable_seqscan=off (no usable index): %s\nquery: %s\nplan:\n%s",
			strings.Join(bad, ", "), query, plan)
	}
}

// Explain returns the indented JSON plan of query under
// enable_seqscan = off, in a transaction that is rolled back.
func Explain(conn Beginner, query string, args ...any) (string, error) {
	ctx, cancel := context.WithTimeout(untraced(context.Background()), time.Minute)
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		return "", err
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+query, args...).Scan(&raw); err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	return string(out), err
}

// FullScan is a plan node that reads a whole table or index.
type FullScan struct {
	Node     string // "Seq Scan", "Index Scan", "Index Only Scan"
	Relation string
	Index    string // for index scans
}

func (s FullScan) String() string {
	if s.Index != "" {
		return fmt.Sprintf("%s on %s using %s without Index Cond", s.Node, s.Relation, s.Index)
	}
	return s.Node + " on " + s.Relation
}

// limitTransparent nodes pass a Limit's early stop through to their
// children: they emit rows as their child produces them.
var limitTransparent = map[string]bool{
	"Result": true, "Subquery Scan": true, "Append": true, "Merge Append": true,
	"Unique": true, "Incremental Sort": true, "Gather Merge": true,
}

// FullScans returns every unbounded scan in a JSON plan (EXPLAIN FORMAT
// JSON), including subplans and init plans, sorted. A Bitmap Index Scan
// always has an Index Cond, so bitmap scans never count.
func FullScans(plan string) []FullScan {
	var v any
	if err := json.Unmarshal([]byte(plan), &v); err != nil {
		panic(fmt.Sprintf("perfguard: bad plan json: %v", err))
	}
	var out []FullScan
	var walk func(v any, limited bool)
	walk = func(v any, limited bool) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e, limited)
			}
		case map[string]any:
			if p, ok := x["Plan"]; ok { // top level: {"Plan": {...}}
				walk(p, false)
				return
			}
			node, _ := x["Node Type"].(string)
			rel, _ := x["Relation Name"].(string)
			switch node {
			case "Seq Scan":
				out = append(out, FullScan{Node: node, Relation: rel})
			case "Index Scan", "Index Only Scan":
				// A Limit stops a scan early only when every row it
				// emits counts toward the Limit: with a Filter it reads
				// on until enough rows match, the whole index if few do.
				_, cond := x["Index Cond"]
				_, filter := x["Filter"]
				if !cond && (!limited || filter) {
					idx, _ := x["Index Name"].(string)
					out = append(out, FullScan{Node: node, Relation: rel, Index: idx})
				}
			}
			childLimited := node == "Limit" || (limited && limitTransparent[node])
			if kids, ok := x["Plans"].([]any); ok {
				for _, k := range kids {
					km, _ := k.(map[string]any)
					switch rel := km["Parent Relationship"]; {
					case rel == "InitPlan" || rel == "SubPlan":
						// Runs to completion (per outer row, for a
						// correlated subplan), whatever Limit is above.
						walk(k, false)
					case childLimited:
						walk(k, true)
					default:
						// Only the outer side of a left nested loop is
						// stopped by a Limit: an inner join can drop
						// outer rows, and the inner side is rescanned
						// per outer row.
						walk(k, limited && node == "Nested Loop" && rel == "Outer" && x["Join Type"] == "Left")
					}
				}
			}
		}
	}
	walk(v, false)
	slices.SortFunc(out, func(a, b FullScan) int { return strings.Compare(a.String(), b.String()) })
	return out
}
