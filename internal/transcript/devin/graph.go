package devin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// maxChainDepth bounds one parent walk. The schema does not forbid a cycle,
// and a real conversation is far shorter. Same bound as FAD.
const maxChainDepth = 50_000

// graphNode is the light projection of one message_nodes row: no content.
type graphNode struct {
	rowID     int64
	nodeID    int64
	parent    int64 // valid when hasParent
	hasParent bool
	createdAt int64    // epoch seconds
	key       string   // message_id, or "\x00node:<node_id>" when it has none
	calls     []string // tool call ids the row carries (tool_calls[].id, tool_call_id); loaded withCalls
}

// graph is one session's forest, in row_id order.
type graph struct {
	nodes  []graphNode
	byNode map[int64]int // node_id -> index in nodes
}

func (g *graph) maxRow() int64 {
	if len(g.nodes) == 0 {
		return 0
	}
	return g.nodes[len(g.nodes)-1].rowID
}

// loadGraph reads a session's rows with row_id > after into g (a new graph
// when nil), in row_id order. withCalls also reads the tool call ids each
// row carries, which costs a second pass over its JSON.
func loadGraph(ctx context.Context, tx *sql.Tx, sessionID string, after int64, g *graph, withCalls bool) (*graph, error) {
	calls := "NULL, NULL"
	if withCalls {
		calls = `json_extract(chat_message, '$.tool_call_id'),
		       (SELECT json_group_array(json_extract(j.value, '$.id')) FROM json_each(m.chat_message, '$.tool_calls') j)`
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT row_id, node_id, parent_node_id, json_extract(chat_message, '$.message_id'), created_at, `+calls+`
		  FROM message_nodes m
		 WHERE session_id = ? AND row_id > ?
		 ORDER BY row_id`, sessionID, after)
	if err != nil {
		return nil, fmt.Errorf("devin: load graph %s: %w", sessionID, err)
	}
	defer rows.Close()
	if g == nil {
		g = &graph{byNode: map[int64]int{}}
	}
	for rows.Next() {
		var n graphNode
		var parent sql.NullInt64
		var mid, callID, callIDs sql.NullString
		if err := rows.Scan(&n.rowID, &n.nodeID, &parent, &mid, &n.createdAt, &callID, &callIDs); err != nil {
			return nil, fmt.Errorf("devin: scan graph %s: %w", sessionID, err)
		}
		n.parent, n.hasParent = parent.Int64, parent.Valid
		n.key = mid.String
		if n.key == "" {
			n.key = "\x00node:" + strconv.FormatInt(n.nodeID, 10)
		}
		if callID.String != "" {
			n.calls = append(n.calls, callID.String)
		}
		if callIDs.Valid && callIDs.String != "[]" {
			var ids []*string
			_ = json.Unmarshal([]byte(callIDs.String), &ids) // malformed: no references
			for _, id := range ids {
				if id != nil && *id != "" {
					n.calls = append(n.calls, *id)
				}
			}
		}
		g.byNode[n.nodeID] = len(g.nodes)
		g.nodes = append(g.nodes, n)
	}
	return g, rows.Err()
}

// rowsCalling returns the row_ids of nodes carrying one of ids. g must have
// been loaded withCalls.
func (g *graph) rowsCalling(ids []string) map[int64]bool {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[int64]bool{}
	for i := range g.nodes {
		for _, c := range g.nodes[i].calls {
			if want[c] {
				out[g.nodes[i].rowID] = true
				break
			}
		}
	}
	return out
}

// Session graphs kept across parses. message_nodes is insert-only per row:
// a row is added with a higher row_id (AUTOINCREMENT) or deleted, never
// rewritten (notes/local-search/README.md). So a kept graph is brought up
// to date by reading only the rows after its last row_id, and a deletion
// shows as a row count that no longer matches.
const (
	// graphIdle drops a kept graph whose session has not changed for this
	// long.
	graphIdle = 10 * time.Minute
	// maxKeptNodes bounds the nodes of all kept graphs (about 100 bytes
	// each); least recently used graphs go first.
	maxKeptNodes = 250_000
)

type keptGraph struct {
	g    *graph
	used time.Time
}

// sessionGraph returns the session's graph (with tool call ids) as of the
// parse's snapshot, whose message_nodes count and max row_id are nc. A
// session that changes again after its first parse (keep) has its graph
// kept, so later changes read only new rows; a session parsed for the
// first time is read whole and not kept, so the initial parse of a large
// store keeps nothing.
func (p *Parser) sessionGraph(ctx context.Context, tx *sql.Tx, id string, nc nodeCount, keep bool, now time.Time) (*graph, error) {
	if !keep {
		delete(p.graphs, id)
		g, err := loadGraph(ctx, tx, id, 0, nil, false)
		if g != nil {
			p.graphRows.Add(int64(len(g.nodes)))
		}
		return g, err
	}
	if k := p.graphs[id]; k != nil {
		n := len(k.g.nodes)
		g, err := loadGraph(ctx, tx, id, k.g.maxRow(), k.g, true)
		if err != nil {
			delete(p.graphs, id)
			return nil, err
		}
		p.graphRows.Add(int64(len(g.nodes) - n))
		if int64(len(g.nodes)) == nc.count && g.maxRow() == nc.maxRow {
			k.used = now
			return g, nil
		}
		delete(p.graphs, id) // rows were deleted: read the session again
	}
	g, err := loadGraph(ctx, tx, id, 0, nil, true)
	if err != nil {
		return nil, err
	}
	p.graphRows.Add(int64(len(g.nodes)))
	if p.graphs == nil {
		p.graphs = map[string]*keptGraph{}
	}
	p.graphs[id] = &keptGraph{g: g, used: now}
	return g, nil
}

// trimGraphs drops kept graphs of sessions that are gone or idle, then the
// least recently used ones beyond maxKeptNodes.
func (p *Parser) trimGraphs(live map[string]sessionState, now time.Time) {
	total := 0
	for id, k := range p.graphs {
		if _, ok := live[id]; !ok || now.Sub(k.used) > graphIdle {
			delete(p.graphs, id)
			continue
		}
		total += len(k.g.nodes)
	}
	for total > maxKeptNodes {
		var oldest string
		for id, k := range p.graphs {
			if oldest == "" || k.used.Before(p.graphs[oldest].used) {
				oldest = id
			}
		}
		total -= len(p.graphs[oldest].g.nodes)
		delete(p.graphs, oldest)
	}
}

// countUpTo returns how many nodes have row_id <= maxRow.
func (g *graph) countUpTo(maxRow int64) int64 {
	var n int64
	for i := range g.nodes {
		if g.nodes[i].rowID <= maxRow {
			n++
		}
	}
	return n
}

// chain returns the node ids on the ancestry of tip among nodes with
// row_id <= maxRow, or nil when there is no pointer or the tip is not
// among them. The walk stops at a root, a missing parent, a repeated node
// (cycle) or maxChainDepth; it never spins. Adapted from FAD 0.3.1
// devin.rs walk_main_chain.
func (g *graph) chain(tip *int64, maxRow int64) map[int64]bool {
	if tip == nil {
		return nil
	}
	in := func(node int64) (graphNode, bool) {
		i, ok := g.byNode[node]
		if !ok || g.nodes[i].rowID > maxRow {
			return graphNode{}, false
		}
		return g.nodes[i], true
	}
	if _, ok := in(*tip); !ok {
		return nil
	}
	on := map[int64]bool{}
	cur, ok := *tip, true
	for ok && len(on) < maxChainDepth && !on[cur] {
		n, found := in(cur)
		if !found {
			break
		}
		on[cur] = true
		cur, ok = n.parent, n.hasParent
	}
	return on
}

// pick is the emission decision for one message_id key.
type pick struct {
	canonical int    // index of the node whose content becomes the row
	first     int    // index of the earliest node: ordinal and timestamp
	onPath    int8   // -1 unknown (no pointer), 0 false, 1 true
	parentKey string // key of the canonical node's parent node, "" for a root
	copies    int    // nodes sharing the key; a new copy may add rows
}

// view resolves every key among nodes with row_id <= maxRow against the
// chain ending at tip. Upsert by message_id (spec §5.4): all nodes sharing
// a message_id (compaction copies) collapse into one set of rows. The rows
// are on the active path if any copy is on the chain; their content comes
// from the newest on-chain copy, else the newest copy (the canonical copy);
// their ordinal and timestamp from the first copy, so the collapsed rows
// keep their original position. Rows only other copies have (a tool call
// the canonical copy lacks) are emitted too, off the active path (see
// syncer.emit).
func (g *graph) view(tip *int64, maxRow int64) map[string]pick {
	on := g.chain(tip, maxRow)
	out := map[string]pick{}
	for i := range g.nodes {
		n := &g.nodes[i]
		if n.rowID > maxRow {
			break // row_id order
		}
		p, seen := out[n.key]
		onChain := on[n.nodeID]
		switch {
		case !seen:
			p = pick{canonical: i, first: i, onPath: -1}
			if on != nil {
				p.onPath = 0
			}
		case onChain || p.onPath != 1:
			p.canonical = i // newer copy; on-chain copies win
		}
		p.copies++
		if onChain {
			p.onPath = 1
		}
		out[n.key] = p
	}
	for k, p := range out {
		p.parentKey = g.parentKey(p.canonical)
		out[k] = p
	}
	return out
}

func (g *graph) parentKey(i int) string {
	n := g.nodes[i]
	if !n.hasParent {
		return ""
	}
	j, ok := g.byNode[n.parent]
	if !ok {
		return ""
	}
	return g.nodes[j].key
}

// same reports whether two picks of the same graph produce identical rows.
func (g *graph) same(a, b pick) bool {
	return a.canonical == b.canonical && a.first == b.first && a.onPath == b.onPath && a.parentKey == b.parentKey && a.copies == b.copies
}

// members returns, per key in keys, the indexes of its nodes with
// row_id <= maxRow, in row_id order.
func (g *graph) members(keys map[string]bool, maxRow int64) map[string][]int {
	out := make(map[string][]int, len(keys))
	for i := range g.nodes {
		n := &g.nodes[i]
		if n.rowID > maxRow {
			break
		}
		if keys[n.key] {
			out[n.key] = append(out[n.key], i)
		}
	}
	return out
}
