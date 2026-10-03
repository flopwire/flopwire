package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Name is the parser name recorded on every row and source. Bump it
// whenever the rows a store yields change: the indexer re-parses every
// source whose recorded parser differs.
const Name = "opencode@1.0"

// lagMS is how far behind a session's last seen time_updated the next
// parse looks again for changed rows. opencode stamps time_updated just
// before its write commits, so a row committed after the snapshot that saw
// a later stamp is still picked up.
const lagMS = 2000

// Parser reads an opencode.db. Like the Devin parser, and unlike the JSONL
// parsers, it reads a database, not an appended byte stream:
//
//   - Parse opens in.Source.Path itself (read-only, see openReadOnly) and
//     ignores in.R and in.Size. The indexer polls (a stat of opencode.db
//     and opencode.db-wal is a cheap pre-check).
//   - The returned Cursor's Offset is 0; State holds per-session
//     watermarks (see state).
//   - The sink must implement transcript.SessionSuperseder: a session that
//     vanished, or lost rows (a reverted turn), is superseded and, if it
//     still exists, re-emitted whole.
//
// Rows are upserted by native id (the part id, Part 1 for a tool result):
// a part updated in place is emitted again with the same identity.
type Parser struct {
	// Caps bounds stored text per kind; nil means transcript.DefaultCaps.
	Caps map[transcript.Kind]transcript.CapConfig
}

func (p *Parser) Name() string                        { return Name }
func (p *Parser) Agent() transcript.Agent             { return transcript.AgentOpencode }
func (p *Parser) StorageKind() transcript.StorageKind { return transcript.StorageSQLite }

var _ transcript.Parser = (*Parser)(nil)

// state is Cursor.State: the per-session watermarks of the last parse.
type state struct {
	Version  int                     `json:"v"`
	Sessions map[string]sessionState `json:"sessions"`
}

type sessionState struct {
	Meta    uint64 `json:"meta"`              // hash of the session row
	Spawned string `json:"spawned,omitempty"` // the parent's spawning task part, once found
	Parts   agg    `json:"parts"`
	Msgs    agg    `json:"msgs"`
}

// agg summarizes one session's rows of a table. Any insert, update or
// delete moves N, Max or Sum.
type agg struct {
	N   int64  `json:"n"`   // rows
	Max string `json:"max"` // highest id
	Sum int64  `json:"sum"` // sum of time_updated
	WM  int64  `json:"wm"`  // highest time_updated
}

const stateVersion = 1

func decodeState(cur transcript.Cursor) state {
	st := state{Version: stateVersion, Sessions: map[string]sessionState{}}
	if len(cur.State) == 0 {
		return st
	}
	var got state
	if err := json.Unmarshal(cur.State, &got); err != nil || got.Version != stateVersion {
		return st // start over; the store upserts by native id
	}
	if got.Sessions == nil {
		got.Sessions = map[string]sessionState{}
	}
	return got
}

// sessionRow is one session row.
type sessionRow struct {
	id, parent, cwd, title, version, agent, model, slug string
	created, updated, archived                          int64
	spawnedBy                                           string
	hasSpawnedBy                                        bool // the store records it (an export store)
}

func (s *sessionRow) hash() uint64 {
	h := sha256.New()
	fmt.Fprintf(h, "%q|%q|%q|%q|%q|%q|%q|%d|%d|%d", s.parent, s.cwd, s.title, s.version, s.agent, s.model, s.slug,
		s.created, s.updated, s.archived)
	if s.hasSpawnedBy {
		fmt.Fprintf(h, "|%q", s.spawnedBy) // an export store records it on the row
	}
	return binary.BigEndian.Uint64(h.Sum(nil))
}

// columns returns the column names of a table, so the parser reads stores
// from before a column was added.
func columns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("opencode: columns of %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

func loadSessions(ctx context.Context, tx *sql.Tx) (map[string]*sessionRow, error) {
	cols, err := columns(ctx, tx, "session")
	if err != nil {
		return nil, err
	}
	if !cols["id"] {
		return nil, errors.New("opencode: no session table")
	}
	col := func(name string) string {
		if cols[name] {
			return "`" + name + "`"
		}
		return "NULL"
	}
	q := `SELECT id, ` + strings.Join([]string{col("parent_id"), col("directory"), col("title"), col("version"), col("agent"),
		col("model"), col("slug"), col("time_created"), col("time_updated"), col("time_archived"), col("spawned_by")}, ", ") + ` FROM session`
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("opencode: load sessions: %w", err)
	}
	defer rows.Close()
	out := map[string]*sessionRow{}
	for rows.Next() {
		var s sessionRow
		var parent, cwd, title, version, agent, model, slug, spawned sql.NullString
		var created, updated, archived sql.NullInt64
		if err := rows.Scan(&s.id, &parent, &cwd, &title, &version, &agent, &model, &slug, &created, &updated, &archived, &spawned); err != nil {
			return nil, fmt.Errorf("opencode: scan session: %w", err)
		}
		s.parent, s.cwd, s.title, s.version, s.agent, s.model, s.slug = parent.String, cwd.String, title.String, version.String, agent.String, model.String, slug.String
		s.created, s.updated, s.archived = created.Int64, updated.Int64, archived.Int64
		s.spawnedBy, s.hasSpawnedBy = spawned.String, cols["spawned_by"]
		out[s.id] = &s
	}
	return out, rows.Err()
}

func loadAggs(ctx context.Context, tx *sql.Tx, table string) (map[string]agg, error) {
	rows, err := tx.QueryContext(ctx, `SELECT session_id, count(*), max(id), total(time_updated), max(time_updated) FROM `+table+` GROUP BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("opencode: summarize %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]agg{}
	for rows.Next() {
		var id string
		var a agg
		var sum float64
		if err := rows.Scan(&id, &a.N, &a.Max, &sum, &a.WM); err != nil {
			return nil, err
		}
		a.Sum = int64(sum)
		out[id] = a
	}
	return out, rows.Err()
}

// spawningCall returns the id of the parent's task tool part that started
// the subagent session child, or "".
func spawningCall(ctx context.Context, tx *sql.Tx, parent, child string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM part WHERE session_id = ?
		AND json_extract(data, '$.type') = 'tool' AND json_extract(data, '$.state.metadata.sessionId') = ?
		ORDER BY id LIMIT 1`, parent, child).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("opencode: spawning call of %s: %w", child, err)
	}
	return id, nil
}

// Parse brings the sink up to date with the store, starting from cur.
//
// Per session it compares the store with the saved watermark (row count,
// highest id and the sum of time_updated, for parts and for messages, and
// a hash of the session row):
//
//   - vanished (no session row, no messages, no parts): SupersedeSession;
//   - fewer rows at or below the saved highest id (a reverted turn):
//     SupersedeSession, then re-emit the whole session;
//   - otherwise it emits the parts written since the watermark (less
//     lagMS), and every part of a message new since then.
//
// All reads run in one read transaction, so they see one WAL snapshot.
func (p *Parser) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	if in.Source == nil || in.Source.Path == "" {
		return cur, errors.New("opencode: Input.Source.Path is required")
	}
	sup, ok := sink.(transcript.SessionSuperseder)
	if !ok {
		return cur, errors.New("opencode: sink must implement transcript.SessionSuperseder")
	}
	st := decodeState(cur)
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
	parts, err := loadAggs(ctx, tx, "part")
	if err != nil {
		return cur, err
	}
	msgs, err := loadAggs(ctx, tx, "message")
	if err != nil {
		return cur, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range []map[string]bool{keys(sessions), keys(parts), keys(msgs)} {
		for id := range m {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	var vanished []string
	for id := range st.Sessions {
		if !seen[id] {
			vanished = append(vanished, id)
		}
	}
	slices.Sort(vanished)
	for _, id := range vanished {
		if err := sup.SupersedeSession(transcript.AgentOpencode, id); err != nil {
			return cur, err
		}
	}
	caps := p.Caps
	if caps == nil {
		caps = transcript.DefaultCaps
	}
	s := &syncer{ctx: ctx, tx: tx, sink: sink, sup: sup, caps: caps, sessions: sessions}
	next := state{Version: stateVersion, Sessions: make(map[string]sessionState, len(ids))}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return cur, err
		}
		prev, had := st.Sessions[id]
		ns, err := s.session(id, parts[id], msgs[id], prev, had)
		if err != nil {
			return cur, err
		}
		next.Sessions[id] = ns
	}
	b, err := json.Marshal(next)
	if err != nil {
		return cur, err
	}
	return transcript.Cursor{State: b}, nil
}

func keys[V any](m map[string]V) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

type syncer struct {
	ctx      context.Context
	tx       *sql.Tx
	sink     transcript.Sink
	sup      transcript.SessionSuperseder
	caps     map[transcript.Kind]transcript.CapConfig
	sessions map[string]*sessionRow
}

// session syncs one session.
func (s *syncer) session(id string, pa, ma agg, prev sessionState, had bool) (sessionState, error) {
	row := s.sessions[id]
	if row == nil {
		row = &sessionRow{id: id} // orphan rows: no session row
	}
	ns := sessionState{Meta: row.hash(), Parts: pa, Msgs: ma}
	rowsChanged := !had || pa != prev.Parts || ma != prev.Msgs
	if had && !rowsChanged && ns.Meta == prev.Meta {
		return prev, nil
	}
	found := false
	if row.parent != "" && !row.hasSpawnedBy {
		// The parent's task call names the child once it starts; look again
		// whenever the child changes, until it is found.
		row.spawnedBy = prev.Spawned
		if row.spawnedBy == "" {
			var err error
			if row.spawnedBy, err = spawningCall(s.ctx, s.tx, row.parent, id); err != nil {
				return ns, err
			}
			found = row.spawnedBy != ""
		}
		ns.Spawned = row.spawnedBy
	}
	if s.sessions[id] != nil && (!had || ns.Meta != prev.Meta || found) {
		if err := s.sink.Conversation(s.conversation(row)); err != nil {
			return ns, err
		}
	}
	if had && !rowsChanged {
		return ns, nil
	}
	full := !had
	if had {
		lostParts, err := s.lost("part", id, prev.Parts)
		if err != nil {
			return ns, err
		}
		lostMsgs, err := s.lost("message", id, prev.Msgs)
		if err != nil {
			return ns, err
		}
		if lostParts || lostMsgs {
			if err := s.sup.SupersedeSession(transcript.AgentOpencode, id); err != nil {
				return ns, err
			}
			full = true
		}
	}
	// Thresholds: everything for a full emit, else what moved since the
	// watermark.
	partWM, partMax, msgWM, msgMax := int64(math.MinInt64), "", int64(math.MinInt64), ""
	if !full {
		partWM, partMax, msgWM, msgMax = prev.Parts.WM-lagMS, prev.Parts.Max, prev.Msgs.WM-lagMS, prev.Msgs.Max
	}
	return ns, s.emit(id, partWM, partMax, msgWM, msgMax)
}

// lost reports whether rows at or below the saved highest id are gone.
func (s *syncer) lost(table, session string, prev agg) (bool, error) {
	if prev.N == 0 {
		return false, nil
	}
	var n int64
	if err := s.tx.QueryRowContext(s.ctx, `SELECT count(*) FROM `+table+` WHERE session_id = ? AND id <= ?`, session, prev.Max).Scan(&n); err != nil {
		return false, fmt.Errorf("opencode: count %s: %w", table, err)
	}
	return n < prev.N, nil
}

// emit sends the rows of the parts updated at or after partWM or newer
// than partMax, every part of a message newer than msgMax, and the error
// rows of messages updated at or after msgWM or newer than msgMax.
func (s *syncer) emit(session string, partWM int64, partMax string, msgWM int64, msgMax string) error {
	rc := rowContext{sessionID: session, caps: s.caps}
	rows, err := s.tx.QueryContext(s.ctx, `SELECT p.id, p.message_id, p.time_created, p.time_updated, p.data, m.data
		  FROM part p JOIN message m ON m.id = p.message_id
		 WHERE p.session_id = ? AND (p.time_updated >= ? OR p.id > ? OR m.id > ?)
		 ORDER BY p.id`, session, partWM, partMax, msgMax)
	if err != nil {
		return fmt.Errorf("opencode: read parts: %w", err)
	}
	for rows.Next() {
		var r partRow
		if err := rows.Scan(&r.id, &r.messageID, &r.created, &r.updated, &r.data, &r.message); err != nil {
			rows.Close()
			return fmt.Errorf("opencode: scan part: %w", err)
		}
		for _, m := range partMessages(r, rc) {
			if err := s.sink.Message(m); err != nil {
				rows.Close()
				return err
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	mrows, err := s.tx.QueryContext(s.ctx, `SELECT id, time_updated, data FROM message
		 WHERE session_id = ? AND (time_updated >= ? OR id > ?) AND json_extract(data, '$.error') IS NOT NULL
		 ORDER BY id`, session, msgWM, msgMax)
	if err != nil {
		return fmt.Errorf("opencode: read messages: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var id, data string
		var updated int64
		if err := mrows.Scan(&id, &updated, &data); err != nil {
			return fmt.Errorf("opencode: scan message: %w", err)
		}
		if m := messageError(id, updated, data, rc); m != nil {
			if err := s.sink.Message(m); err != nil {
				return err
			}
		}
	}
	return mrows.Err()
}

func (s *syncer) conversation(r *sessionRow) *transcript.Conversation {
	c := &transcript.Conversation{
		Agent:           transcript.AgentOpencode,
		SessionID:       r.id,
		Cwd:             r.cwd,
		Title:           strings.TrimSpace(r.title),
		ParentSessionID: r.parent,
		Extra:           map[string]any{},
	}
	if r.parent != "" {
		c.SpawnedByToolCallID = r.spawnedBy
		c.Depth = s.depth(r)
	}
	if r.created > 0 {
		c.StartedAt = time.UnixMilli(r.created).UTC()
	}
	if r.updated > 0 {
		c.LastActivityAt = time.UnixMilli(r.updated).UTC()
	}
	for k, v := range map[string]string{"version": r.version, "agent": r.agent, "model": modelName(r.model), "slug": r.slug} {
		if v != "" {
			c.Extra[k] = v
		}
	}
	if r.archived > 0 {
		c.Extra["archived"] = true
	}
	return c
}

// depth counts a session's ancestors, through the session rows the store
// still has.
func (s *syncer) depth(r *sessionRow) int {
	d := 0
	for seen := map[string]bool{r.id: true}; r != nil && r.parent != "" && !seen[r.parent] && d < 64; {
		d++
		seen[r.parent] = true
		r = s.sessions[r.parent]
	}
	return d
}

// modelName renders session.model, JSON {providerID, id}, as
// "provider/id"; any other value is kept as it is.
func modelName(raw string) string {
	var m struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	}
	if json.Unmarshal([]byte(raw), &m) != nil || m.ID == "" {
		return raw
	}
	if m.ProviderID == "" {
		return m.ID
	}
	return m.ProviderID + "/" + m.ID
}
