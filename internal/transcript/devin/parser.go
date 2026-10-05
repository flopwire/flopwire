package devin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Name is the parser name recorded on every row and source. Bump it
// whenever the rows a store yields change: the indexer re-parses every
// source whose recorded parser differs.
//
// devin@2: part fixed per native id (0) and slot-based ordinals; rows only
// an off-chain copy of a message has are emitted off the active path.
// devin@3: rows unchanged; bumped so every store is re-parsed and its
// digests re-folded with the commit evidence of issue #80.
const Name = "devin@3.0"

// Parser reads a Devin sessions.db. It implements transcript.Parser with
// these differences from the JSONL parsers, because the source is a
// database and not an appended byte stream:
//
//   - Parse opens in.Source.Path itself (read-only, see openReadOnly) and
//     ignores in.R and in.Size. The caller's Decide/Watermark gate does not
//     apply; the indexer polls (a stat of sessions.db and sessions.db-wal is
//     a cheap pre-check).
//   - The returned Cursor's Offset is the highest message_nodes.row_id seen
//     and State holds per-session watermarks (see state).
//   - The sink must implement transcript.SessionSuperseder: a session that
//     vanished, or lost rows, is superseded (and, if it still exists,
//     re-emitted whole).
//
// A Parser may be kept across parses of one store (the agent keeps one per
// store): it then keeps the graphs of sessions that keep changing, so a
// live session's next parse reads only its new rows (sessionGraph). Parse
// calls on one Parser are serialized.
//
// Rows are upserted by native id: a message_id seen again (a compaction
// copy, an on-path change, a tool_call_state update) is emitted again with
// the same NativeID and the store replaces or versions the row.
type Parser struct {
	// Caps bounds stored text per kind; nil means transcript.DefaultCaps.
	Caps map[transcript.Kind]transcript.CapConfig

	mu        sync.Mutex            // serializes Parse: graphs
	graphs    map[string]*keptGraph // session graphs kept across parses (sessionGraph)
	graphRows atomic.Int64          // message_nodes rows read into session graphs
	bodyRows  atomic.Int64          // message_nodes rows read with their content
}

// RowsRead is how many message_nodes rows this parser has read: into
// session graphs (row ids, parents, message ids) and with their content.
// Both should grow with what changed, not with session size.
func (p *Parser) RowsRead() (graph, content int64) { return p.graphRows.Load(), p.bodyRows.Load() }

func (p *Parser) Name() string                        { return Name }
func (p *Parser) Agent() transcript.Agent             { return transcript.AgentDevin }
func (p *Parser) StorageKind() transcript.StorageKind { return transcript.StorageSQLite }

var _ transcript.Parser = (*Parser)(nil)

// state is Cursor.State: the per-session watermarks of the last parse.
type state struct {
	Version  int                     `json:"v"`
	Sessions map[string]sessionState `json:"sessions"`
}

type sessionState struct {
	Count     int64     `json:"n"`              // message_nodes rows
	MaxRow    int64     `json:"max"`            // highest row_id
	MainChain *int64    `json:"chain"`          // sessions.main_chain_id
	Meta      uint64    `json:"meta"`           // hash of the sessions row
	Tools     toolTuple `json:"tools"`          // tool_call_state tuple
	Pending   []string  `json:"pend,omitempty"` // tool calls awaiting completion
	PendingOv bool      `json:"pend_ov,omitempty"`
	Hot       bool      `json:"hot,omitempty"` // nodes or sessions row changed in this parse
}

const stateVersion = 1

func decodeState(cur transcript.Cursor) (state, error) {
	st := state{Version: stateVersion, Sessions: map[string]sessionState{}}
	if len(cur.State) == 0 {
		return st, nil
	}
	var got state
	if err := json.Unmarshal(cur.State, &got); err != nil || got.Version != stateVersion {
		// Unreadable or older state: start over. Re-emitting is safe; the
		// store upserts by native id.
		return st, nil
	}
	if got.Sessions == nil {
		got.Sessions = map[string]sessionState{}
	}
	return got, nil
}

// sessionRow is one sessions row.
type sessionRow struct {
	id, cwd, backend, model, agentMode, title string
	createdAt, lastActivity                   int64
	mainChain                                 *int64
	hidden                                    bool
}

func (s *sessionRow) hash() uint64 {
	h := sha256.New()
	chain := "null"
	if s.mainChain != nil {
		chain = fmt.Sprint(*s.mainChain)
	}
	fmt.Fprintf(h, "%q|%q|%q|%q|%q|%d|%d|%s|%t", s.cwd, s.backend, s.model, s.agentMode, s.title, s.createdAt, s.lastActivity, chain, s.hidden)
	return binary.BigEndian.Uint64(h.Sum(nil))
}

func loadSessions(ctx context.Context, tx *sql.Tx) (map[string]*sessionRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, working_directory, backend_type, model, agent_mode, created_at,
		       last_activity_at, title, main_chain_id, COALESCE(hidden, 0)
		  FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("devin: load sessions: %w", err)
	}
	defer rows.Close()
	out := map[string]*sessionRow{}
	for rows.Next() {
		var s sessionRow
		var cwd, backend, model, mode, title sql.NullString
		var created, last, chain sql.NullInt64
		var hidden int64
		if err := rows.Scan(&s.id, &cwd, &backend, &model, &mode, &created, &last, &title, &chain, &hidden); err != nil {
			return nil, fmt.Errorf("devin: scan sessions: %w", err)
		}
		s.cwd, s.backend, s.model, s.agentMode, s.title = cwd.String, backend.String, model.String, mode.String, title.String
		s.createdAt, s.lastActivity, s.hidden = created.Int64, last.Int64, hidden != 0
		if chain.Valid {
			v := chain.Int64
			s.mainChain = &v
		}
		out[s.id] = &s
	}
	return out, rows.Err()
}

type nodeCount struct{ count, maxRow int64 }

func loadNodeCounts(ctx context.Context, tx *sql.Tx) (map[string]nodeCount, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id, count(*), max(row_id) FROM message_nodes GROUP BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("devin: count nodes: %w", err)
	}
	defer rows.Close()
	out := map[string]nodeCount{}
	for rows.Next() {
		var id string
		var c nodeCount
		if err := rows.Scan(&id, &c.count, &c.maxRow); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Parse brings the sink up to date with the store, starting from cur.
//
// Per session it compares the store with the saved watermark (row count,
// max row_id, main_chain_id, sessions row hash, tool_call_state tuple):
//
//   - vanished (no sessions row and no nodes): SupersedeSession;
//   - fewer rows at or below the saved max row_id (rows deleted):
//     SupersedeSession, then re-emit the whole session;
//   - otherwise it resolves every message_id against the old and the new
//     main chain (graph.view) and emits the keys whose row changed: new
//     keys, a new canonical copy, a flipped on_active_path, a new parent;
//     plus the rows referencing tool calls whose tool_call_state changed.
//
// All reads run in one read transaction, so they see one WAL snapshot.
func (p *Parser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	if in.Source == nil || in.Source.Path == "" {
		return cur, errors.New("devin: Input.Source.Path is required")
	}
	sup, ok := sink.(transcript.SessionSuperseder)
	if !ok {
		return cur, errors.New("devin: sink must implement transcript.SessionSuperseder")
	}
	st, err := decodeState(cur)
	if err != nil {
		return cur, err
	}
	db, err := openReadOnly(in.Source.Path)
	if err != nil {
		return cur, err
	}
	defer db.Close()
	tx, err := snapshot(ctx, db)
	if err != nil {
		return cur, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	sessions, err := loadSessions(ctx, tx)
	if err != nil {
		return cur, err
	}
	counts, err := loadNodeCounts(ctx, tx)
	if err != nil {
		return cur, err
	}
	tools, err := newToolLookup(ctx, tx)
	if err != nil {
		return cur, err
	}
	defer tools.stmt.Close()

	ids := make([]string, 0, len(sessions)+len(counts))
	for id := range sessions {
		ids = append(ids, id)
	}
	for id := range counts {
		if sessions[id] == nil {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	next := state{Version: stateVersion, Sessions: make(map[string]sessionState, len(ids))}
	var vanished []string
	for id := range st.Sessions {
		if sessions[id] == nil && counts[id].count == 0 {
			vanished = append(vanished, id)
		}
	}
	slices.Sort(vanished)
	for _, id := range vanished {
		if err := sup.SupersedeSession(transcript.AgentDevin, id); err != nil {
			return cur, err
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	s := &syncer{p: p, ctx: ctx, tx: tx, sink: sink, sup: sup, tools: tools, now: time.Now()}
	var maxRow int64 = cur.Offset
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		prev, had := st.Sessions[id]
		ns, err := s.session(id, sessions[id], counts[id], prev, had)
		if err != nil {
			return cur, err
		}
		if tools.err != nil {
			return cur, tools.err
		}
		next.Sessions[id] = ns
		maxRow = max(maxRow, ns.MaxRow)
	}
	p.trimGraphs(next.Sessions, s.now)
	b, err := json.Marshal(next)
	if err != nil {
		return cur, err
	}
	return transcript.Cursor{Offset: maxRow, State: b}, nil
}

type syncer struct {
	p     *Parser
	ctx   context.Context
	tx    *sql.Tx
	sink  transcript.Sink
	sup   transcript.SessionSuperseder
	tools *toolLookup
	now   time.Time
}

// session syncs one session. Its tool_call_state tuple is read only when
// the session is active: its nodes or sessions row changed in this parse or
// the previous one, or it has tool calls awaiting completion. The previous
// parse counts because the CLI may write tool_call_state just after the
// node that carries the call. A tool_call_state change in a session idle
// for two parses with nothing pending is picked up at its next node change.
func (s *syncer) session(id string, row *sessionRow, nc nodeCount, prev sessionState, had bool) (sessionState, error) {
	if row == nil {
		row = &sessionRow{id: id} // orphan nodes: no sessions row, no pointer
	}
	ns := sessionState{Count: nc.count, MaxRow: nc.maxRow, MainChain: row.mainChain, Meta: row.hash(), Tools: prev.Tools,
		Pending: prev.Pending, PendingOv: prev.PendingOv}
	chainSame := (prev.MainChain == nil) == (row.mainChain == nil) && (prev.MainChain == nil || *prev.MainChain == *row.mainChain)
	nodesChanged := !had || nc.count != prev.Count || nc.maxRow != prev.MaxRow || !chainSame || ns.Meta != prev.Meta
	ns.Hot = nodesChanged
	tt := prev.Tools
	if nodesChanged || prev.Hot || prev.Tools.Pending > 0 {
		var err error
		if tt, err = loadToolTuple(s.ctx, s.tx, id); err != nil {
			return ns, err
		}
		ns.Tools = tt
	}
	if !nodesChanged && tt == prev.Tools {
		return ns, nil
	}
	if !had || ns.Meta != prev.Meta {
		if err := s.sink.Conversation(conversation(row)); err != nil {
			return ns, err
		}
	}

	g, err := s.p.sessionGraph(s.ctx, s.tx, id, nc, had, s.now)
	if err != nil {
		return ns, err
	}
	reset := had && g.countUpTo(prev.MaxRow) < prev.Count
	if reset {
		if err := s.sup.SupersedeSession(transcript.AgentDevin, id); err != nil {
			return ns, err
		}
	}
	cur := g.view(row.mainChain, nc.maxRow)
	var old map[string]pick
	if had && !reset {
		old = g.view(prev.MainChain, prev.MaxRow)
	}

	emit := map[string]bool{} // keys whose rows to emit
	for k, p := range cur {
		if o, ok := old[k]; !ok || !g.same(o, p) {
			emit[k] = true
		}
	}
	if had && !reset && tt != prev.Tools {
		changed, err := changedToolCalls(s.ctx, s.tx, id, prev.Tools.MaxRow, prev.Pending, prev.PendingOv)
		if err != nil {
			return ns, err
		}
		if len(changed) > 0 {
			rowsWith := g.rowsCalling(changed) // had: g was loaded with its calls
			for i := range g.nodes {
				if _, ok := cur[g.nodes[i].key]; ok && rowsWith[g.nodes[i].rowID] {
					emit[g.nodes[i].key] = true
				}
			}
		}
	}
	if tt != prev.Tools || !had {
		ns.Pending, ns.PendingOv = nil, false
		if tt.Pending > 0 {
			if ns.Pending, ns.PendingOv, err = pendingToolCalls(s.ctx, s.tx, id); err != nil {
				return ns, err
			}
		}
	}
	if len(emit) == 0 {
		return ns, nil
	}
	return ns, s.emit(id, g, cur, emit, nc.maxRow)
}

// emit reads every copy of the chosen keys and sends their messages: the
// canonical copy's rows, then the rows only other copies have (newest copy
// first), off the active path. A key's rows go out once its last copy has
// been read. A selection of most of the session streams the whole session
// instead of batching row ids; a main chain switch in a long session (a few
// hundred keys flip on or off the path) is read by row id.
func (s *syncer) emit(sessionID string, g *graph, cur map[string]pick, keys map[string]bool, maxRow int64) error {
	members := g.members(keys, maxRow)
	byRow := map[int64]int{}
	var rowIDs []int64
	for _, idx := range members {
		for _, i := range idx {
			byRow[g.nodes[i].rowID] = i
			rowIDs = append(rowIDs, g.nodes[i].rowID)
		}
	}
	slices.Sort(rowIDs)
	s.tools.sessionID = sessionID
	caps := s.p.Caps
	if caps == nil {
		caps = transcript.DefaultCaps
	}
	read := map[string]map[int]nodeRow{} // key -> graph index -> row, until complete
	handle := func(n nodeRow) error {
		i, ok := byRow[n.rowID]
		if !ok {
			return nil
		}
		key := g.nodes[i].key
		got := read[key]
		if got == nil {
			got = map[int]nodeRow{}
			read[key] = got
		}
		got[i] = n
		if len(got) < len(members[key]) {
			return nil
		}
		delete(read, key)
		msgs, err := s.keyMessages(sessionID, g, cur[key], members[key], got, caps)
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if err := s.sink.Message(m); err != nil {
				return err
			}
		}
		return nil
	}
	if len(rowIDs) > 256 && len(rowIDs) > len(g.nodes)/4 {
		return s.scan(`SELECT row_id, node_id, chat_message, created_at FROM message_nodes WHERE session_id = ? ORDER BY row_id`, []any{sessionID}, handle)
	}
	for start := 0; start < len(rowIDs); start += 128 {
		chunk := rowIDs[start:min(start+128, len(rowIDs))]
		args := make([]any, len(chunk))
		for i, r := range chunk {
			args[i] = r
		}
		q := `SELECT row_id, node_id, chat_message, created_at FROM message_nodes WHERE row_id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `) ORDER BY row_id`
		if err := s.scan(q, args, handle); err != nil {
			return err
		}
	}
	return nil
}

// offCopySlot is added to the slot of a row that only a non-canonical copy
// of a message has, so its ordinal never collides with the canonical rows.
const offCopySlot = 2048

// keyMessages renders one key: the canonical copy's rows, then each row
// (by native id) that only another copy has, newest copy first, marked off
// the active path when the store has a chain pointer.
func (s *syncer) keyMessages(sessionID string, g *graph, pk pick, members []int, rows map[int]nodeRow, caps map[transcript.Kind]transcript.CapConfig) ([]*transcript.Message, error) {
	first := g.nodes[pk.first]
	ctx := func(i int) rowContext {
		rc := rowContext{
			sessionID:  sessionID,
			parser:     Name,
			caps:       caps,
			pick:       pk,
			firstRowID: first.rowID,
			firstTS:    first.createdAt,
			tools:      s.tools.get,
		}
		if k := g.parentKey(i); k != "" && !strings.HasPrefix(k, "\x00") {
			rc.parentMID = k
		}
		return rc
	}
	out, err := nodeMessages(rows[pk.canonical], ctx(pk.canonical))
	if err != nil || len(members) == 1 {
		return out, err
	}
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.NativeID] = true
	}
	extra := 0
	for j := len(members) - 1; j >= 0; j-- {
		i := members[j]
		if i == pk.canonical {
			continue
		}
		msgs, err := nodeMessages(rows[i], ctx(i))
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if m.NativeID == "" || seen[m.NativeID] {
				continue
			}
			seen[m.NativeID] = true
			m.Ordinal = transcript.OrdinalAt(first.rowID, offCopySlot+extra)
			extra++
			if pk.onPath >= 0 {
				m.OnActivePath = transcript.BoolPtr(false)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *syncer) scan(q string, args []any, fn func(nodeRow) error) error {
	rows, err := s.tx.QueryContext(s.ctx, q, args...)
	if err != nil {
		return fmt.Errorf("devin: read nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.rowID, &n.nodeID, &n.chat, &n.createdAt); err != nil {
			return fmt.Errorf("devin: scan node: %w", err)
		}
		s.p.bodyRows.Add(1)
		if err := fn(n); err != nil {
			return err
		}
	}
	return rows.Err()
}

func conversation(s *sessionRow) *transcript.Conversation {
	c := &transcript.Conversation{
		Agent:     transcript.AgentDevin,
		SessionID: s.id,
		Cwd:       s.cwd,
		Title:     strings.TrimSpace(s.title),
		Extra:     map[string]any{},
	}
	if s.createdAt > 0 {
		c.StartedAt = time.Unix(s.createdAt, 0).UTC()
	}
	if s.lastActivity > 0 {
		c.LastActivityAt = time.Unix(s.lastActivity, 0).UTC()
	}
	for k, v := range map[string]string{"model": s.model, "agent_mode": s.agentMode, "backend_type": s.backend} {
		if v != "" {
			c.Extra[k] = v
		}
	}
	if s.mainChain != nil {
		c.Extra["main_chain_id"] = *s.mainChain
	}
	if s.hidden {
		// Retired sessions `devin list` omits. Indexed like any other; FAD
		// and agentsview skip them.
		c.Extra["hidden"] = true
	}
	return c
}
