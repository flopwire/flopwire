package localindex

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Batch is one unit of indexer output, applied in a single transaction.
type Batch struct {
	SourceID   int64
	Generation int64 // stamped on every row written; the source's current generation
	// NewGeneration, when set, is recorded (and made current) first.
	NewGeneration       *transcript.Generation
	NewGenerationReason string

	Conversations []*transcript.Conversation
	Messages      []*transcript.Message

	// Watermark, when set, is saved in the same transaction, so the resume
	// offset never runs ahead of the rows it covers.
	Watermark     *transcript.Watermark
	CursorState   []byte
	Extraction    *transcript.ExtractionCheckpoint
	AppliedParser string // nonempty on a completed parse; committed with its checkpoint

	// The end of a re-parse, applied after the rows and before the
	// watermark in the same transaction, so a crash cannot leave the new
	// watermark saved while rows the file no longer has stay live.
	// SupersedeAbsent retires the source's live rows that this generation
	// did not touch (Store.SupersedeAbsent with Generation); RetireSources
	// retires every live row of those sources (older file identities at
	// the same path).
	SupersedeAbsent bool
	RetireSources   []int64

	// prep holds each message's compressed and FTS text, computed by the
	// Sink on the parse worker; ApplyBatch computes what is missing.
	prep []prepared
}

// BatchResult counts what ApplyBatch did to message rows.
type BatchResult struct {
	Inserted      int // new keys
	Grown         int // prefix growth, replaced in place
	Touched       int // same text; metadata, generation, or supersede state refreshed
	Versioned     int // text changed: old version superseded, new version inserted
	Conversations int
}

// ApplyBatch upserts conversations and messages (spec §4.2):
//
//   - identity is (conversation, native_id, part), else (source, locator or
//     byte offset, part); the positional ordinal is never a key;
//   - same uncapped content: refresh metadata and generation, clear
//     superseded (a row that re-appears in a later generation is live again);
//   - new capped text has the stored text as a prefix: replace in place and
//     re-index (streaming growth, results filled in later);
//   - otherwise: mark the stored version superseded and insert version+1.
//
// Both FTS tables are updated on every path that changes stored text.
// Messages name their conversation by SessionID; a conversation missing
// from both the batch and the index gets a stub row under the source's agent.
func (s *Store) ApplyBatch(ctx context.Context, b Batch) (BatchResult, error) {
	var res BatchResult
	err := s.write(ctx, func(w *writeTx) error {
		res = BatchResult{}
		return w.applyBatch(&b, &res)
	})
	return res, err
}

func (w *writeTx) applyBatch(b *Batch, res *BatchResult) error {
	if b.NewGeneration != nil {
		if err := w.startGeneration(b.SourceID, b.NewGeneration, b.NewGenerationReason); err != nil {
			return err
		}
		if b.Generation == 0 {
			b.Generation = b.NewGeneration.Generation
		}
	}
	var agent string
	row, err := w.queryRow(`SELECT agent FROM sources WHERE id = ?`, b.SourceID)
	if err != nil {
		return err
	}
	if err := row.Scan(&agent); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &NotFoundError{What: "source", ID: b.SourceID}
		}
		return err
	}
	// The owner's redactions, read on the writer: a redaction records its
	// tombstones in its own write request, so every batch the writer runs
	// after it is masked, and every batch before it is masked by it.
	w.s.tombs.maskTitles(b.Conversations)
	if len(b.prep) != len(b.Messages) {
		b.prep = prepareAll(b.Messages)
	}
	w.s.tombs.mask(b.Messages, b.prep)
	convs := map[string]int64{}
	for _, c := range b.Conversations {
		if c.Agent == "" {
			c.Agent = transcript.Agent(agent)
		}
		id, err := w.upsertConversation(b.SourceID, c)
		if err != nil {
			return err
		}
		convs[c.SessionID] = id
		res.Conversations++
	}
	if w.s.opts.SyncOnly {
		b.Messages = nil // a sync-only index keeps no message rows
	}
	type span struct{ min, max int64 }
	spans := map[int64]*span{}
	byConv := map[int64][]*transcript.Message{}  // every row written: folded into the digest
	counted := map[int64][]*transcript.Message{} // new rows: added to the digest's counts
	replaced := map[int64]bool{}                 // a counted attribute of an existing row changed
	for i, m := range b.Messages {
		convID, ok := convs[m.SessionID]
		if !ok {
			if convID, err = w.conversationID(agent, m.SessionID, b.SourceID); err != nil {
				return err
			}
			convs[m.SessionID] = convID
		}
		change, err := w.upsertMessage(b, convID, m, &b.prep[i], res)
		if err != nil {
			return fmt.Errorf("message %s/%s part %d: %w", m.SessionID, m.NativeID, m.Part, err)
		}
		switch change {
		case rowAdded:
			counted[convID] = append(counted[convID], m)
		case rowChanged:
			replaced[convID] = true
		}
		byConv[convID] = append(byConv[convID], m)
		if !m.TS.IsZero() {
			ts := m.TS.UnixMilli()
			sp := spans[convID]
			if sp == nil {
				spans[convID] = &span{ts, ts}
			} else {
				sp.min, sp.max = min(sp.min, ts), max(sp.max, ts)
			}
		}
	}
	for convID, sp := range spans {
		if _, err := w.exec(`UPDATE conversations SET
			started_at = CASE WHEN started_at IS NULL OR started_at > ? THEN ? ELSE started_at END,
			last_activity_at = CASE WHEN last_activity_at IS NULL OR last_activity_at < ? THEN ? ELSE last_activity_at END
			WHERE id = ?`, sp.min, sp.min, sp.max, sp.max, convID); err != nil {
			return err
		}
	}
	for _, id := range convs {
		if err := w.resolveLinks(id); err != nil {
			return err
		}
	}
	if b.SupersedeAbsent {
		if _, err := w.supersedeAbsent(b.SourceID, b.Generation); err != nil {
			return err
		}
	}
	for _, id := range b.RetireSources {
		if id == b.SourceID {
			continue
		}
		if _, err := w.supersedeAbsent(id, math.MaxInt64); err != nil {
			return err
		}
	}
	// Every append refreshes the digests of the conversations it touched:
	// adding new rows to the counts, or recounting when a counted
	// attribute of an existing row changed or rows were superseded. A
	// re-parse that touches rows without changing what the digest counts
	// (a new generation, a moved line) adds nothing, so it does not
	// recount the whole conversation per batch.
	retired := b.SupersedeAbsent || len(b.RetireSources) > 0
	ids := make([]int64, 0, len(convs))
	for _, id := range convs {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := w.refreshDigest(id, byConv[id], counted[id], retired || replaced[id]); err != nil {
			return fmt.Errorf("digest of conversation %d: %w", id, err)
		}
	}
	if b.Watermark != nil {
		if b.AppliedParser != "" {
			var report any
			if b.Extraction != nil {
				if b.Extraction.Generation != b.Generation || b.Extraction.Offset != b.Watermark.Offset || b.Extraction.LineNo != b.Watermark.LineNo {
					return errors.New("extraction report does not match watermark")
				}
				if err := b.Extraction.Report.Validate(); err != nil {
					return err
				}
				data, err := json.Marshal(b.Extraction)
				if err != nil {
					return err
				}
				report = string(data)
			}
			if _, err := w.exec("UPDATE sources SET parser=?, extraction_report=? WHERE id=?", b.AppliedParser, report, b.SourceID); err != nil {
				return err
			}
		}
		return w.saveWatermark(b.SourceID, b.Watermark, b.CursorState)
	}
	return nil
}

func (w *writeTx) upsertConversation(sourceID int64, c *transcript.Conversation) (int64, error) {
	if c.SessionID == "" {
		return 0, errors.New("conversation without session id")
	}
	var extra any
	if len(c.Extra) > 0 {
		j, err := json.Marshal(c.Extra)
		if err != nil {
			return 0, err
		}
		extra = string(j)
	}
	var repo any
	if c.Cwd != "" {
		if r := w.s.opts.RepoRoot(c.Cwd); r != "" {
			repo = r
		}
	}
	var branches any
	if len(c.Branches) > 0 {
		j, err := json.Marshal(c.Branches)
		if err != nil {
			return 0, err
		}
		branches = string(j)
	}
	row, err := w.queryRow(`INSERT INTO conversations (source_id, agent, session_id, device_id, cwd, repo_root, title,
		  started_at, last_activity_at, parent_session_id, spawned_by_tool_call_id, depth, extra, branches)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id, agent, session_id) DO UPDATE SET
		  source_id = excluded.source_id,
		  cwd = coalesce(excluded.cwd, cwd),
		  repo_root = coalesce(repo_root, excluded.repo_root),
		  title = coalesce(excluded.title, title),
		  started_at = coalesce(min(excluded.started_at, started_at), started_at, excluded.started_at),
		  last_activity_at = coalesce(max(excluded.last_activity_at, last_activity_at), last_activity_at, excluded.last_activity_at),
		  parent_session_id = coalesce(excluded.parent_session_id, parent_session_id),
		  spawned_by_tool_call_id = coalesce(excluded.spawned_by_tool_call_id, spawned_by_tool_call_id),
		  depth = max(excluded.depth, depth),
		  extra = coalesce(excluded.extra, extra),
		  branches = coalesce(excluded.branches, branches),
		  deleted_in_generation = NULL
		RETURNING id`,
		sourceID, string(c.Agent), c.SessionID, w.s.opts.DeviceID, nullStr(c.Cwd), repo, nullStr(c.Title),
		nullTime(c.StartedAt), nullTime(c.LastActivityAt), nullStr(c.ParentSessionID), nullStr(c.SpawnedByToolCallID), c.Depth, extra, branches)
	if err != nil {
		return 0, err
	}
	var id int64
	return id, row.Scan(&id)
}

// conversationID finds a conversation by native session id, inserting a
// stub when a message arrives before its conversation record.
func (w *writeTx) conversationID(agent, sessionID string, sourceID int64) (int64, error) {
	row, err := w.queryRow(`SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?`, w.s.opts.DeviceID, agent, sessionID)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := row.Scan(&id); err == nil || !errors.Is(err, sql.ErrNoRows) {
		return id, err
	}
	return w.upsertConversation(sourceID, &transcript.Conversation{Agent: transcript.Agent(agent), SessionID: sessionID})
}

// resolveLinks fills parent_conversation_id and spawned_by_message_id for
// the conversation and for children that arrived before it (spec §4.3).
func (w *writeTx) resolveLinks(convID int64) error {
	if _, err := w.exec(resolveParentSQL, convID); err != nil {
		return err
	}
	if _, err := w.exec(resolveChildrenSQL, convID, convID); err != nil {
		return err
	}
	_, err := w.exec(resolveSpawnSQL, convID, convID)
	return err
}

const (
	resolveParentSQL = `UPDATE conversations SET parent_conversation_id = (
		  SELECT p.id FROM conversations p WHERE p.device_id = conversations.device_id AND p.agent = conversations.agent
		    AND p.session_id = conversations.parent_session_id)
		WHERE id = ? AND parent_session_id IS NOT NULL AND parent_conversation_id IS NULL`
	resolveChildrenSQL = `UPDATE conversations SET parent_conversation_id = ?
		WHERE parent_conversation_id IS NULL AND (device_id, agent, parent_session_id) =
		  (SELECT device_id, agent, session_id FROM conversations WHERE id = ?)`
	// The spawning tool call: in the parent, the tool_call row whose call
	// id (or native id) is the child's spawned_by_tool_call_id.
	// The two lookups are separate so each uses its index
	// (messages_tool_call, messages_native_head).
	resolveSpawnSQL = `UPDATE conversations SET spawned_by_message_id = (SELECT min(id) FROM (
		  SELECT m.id FROM messages m WHERE m.conversation_id = conversations.parent_conversation_id
		    AND m.kind = 'tool_call' AND m.superseded_by IS NULL AND m.tool_call_id = conversations.spawned_by_tool_call_id
		  UNION ALL
		  SELECT m.id FROM messages m WHERE m.conversation_id = conversations.parent_conversation_id
		    AND m.kind = 'tool_call' AND m.superseded_by IS NULL AND m.native_id = conversations.spawned_by_tool_call_id))
		WHERE spawned_by_message_id IS NULL AND spawned_by_tool_call_id IS NOT NULL AND parent_conversation_id IS NOT NULL
		  AND (id = ? OR parent_conversation_id = ?)`
)

// head is the current version of a message key, with the attributes the
// digest counts.
type head struct {
	id, version int64
	text        []byte
	sha         []byte

	kind                 string
	tool, callID, enrich sql.NullString
	isError, superseded  bool
	onPath               sql.NullBool
}

// sameCounts reports whether refreshing the head with m (refresh's
// coalescing of on_active_path and enrichment included) leaves every
// attribute the digest counts as it was.
func (h *head) sameCounts(m *transcript.Message, enr any) bool {
	if h.kind != m.Kind.String() || h.tool.String != m.ToolName || h.callID.String != m.ToolCallID ||
		h.isError != m.IsError || h.superseded != m.Superseded {
		return false
	}
	if m.OnActivePath != nil && (!h.onPath.Valid || h.onPath.Bool != *m.OnActivePath) {
		return false
	}
	if e, ok := enr.(string); ok && (!h.enrich.Valid || h.enrich.String != e) {
		return false
	}
	return true
}

// rowChange is what an upsert did to the digest's counts.
type rowChange int

const (
	rowAdded   rowChange = iota // a new key: its counts add
	rowSame                     // nothing counted changed
	rowChanged                  // a counted attribute changed, or a new version replaced the row: recount
)

func (w *writeTx) findHead(b *Batch, convID int64, m *transcript.Message) (*head, error) {
	var row *sql.Row
	var err error
	if m.NativeID != "" {
		row, err = w.queryRow(`SELECT `+headCols+` FROM messages
			WHERE conversation_id = ? AND native_id = ? AND part = ? AND superseded_by IS NULL`, convID, m.NativeID, m.Part)
	} else {
		row, err = w.queryRow(`SELECT `+headCols+` FROM messages
			WHERE source_id = ? AND ifnull(locator, byte_offset) = ? AND part = ? AND native_id IS NULL AND superseded_by IS NULL`,
			b.SourceID, locatorKey(m), m.Part)
	}
	if err != nil {
		return nil, err
	}
	h := &head{}
	if err := row.Scan(&h.id, &h.version, &h.text, &h.sha, &h.kind, &h.tool, &h.callID, &h.enrich, &h.isError, &h.superseded, &h.onPath); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return h, nil
}

const headCols = `id, version, text, content_sha, kind, tool_name, tool_call_id, enrichment, is_error, superseded, on_active_path`

// locatorKey matches the messages_locator_head index expression.
func locatorKey(m *transcript.Message) any {
	if m.Locator != "" {
		return m.Locator
	}
	return m.ByteOffset
}

func (w *writeTx) upsertMessage(b *Batch, convID int64, m *transcript.Message, p *prepared, res *BatchResult) (rowChange, error) {
	h, err := w.findHead(b, convID, m)
	if err != nil {
		return 0, err
	}
	if h == nil {
		_, err := w.insertMessage(b, convID, m, p, 1)
		res.Inserted++
		return rowAdded, err
	}
	enr, err := enrichmentJSON(m)
	if err != nil {
		return 0, err
	}
	change := rowChanged
	if h.sameCounts(m, enr) {
		change = rowSame
	}
	if bytes.Equal(h.sha, m.ContentSHA[:]) {
		res.Touched++
		return change, w.refresh(b, h.id, m, enr, nil)
	}
	old, err := decompress(h.text)
	if err != nil {
		return 0, fmt.Errorf("row %d: %w", h.id, err)
	}
	if strings.HasPrefix(m.Text, old) {
		if m.Text == old {
			res.Touched++
			return change, w.refresh(b, h.id, m, enr, nil)
		}
		res.Grown++
		if err := w.ftsDelete(h.id); err != nil {
			return 0, err
		}
		if err := w.refresh(b, h.id, m, enr, p); err != nil {
			return 0, err
		}
		w.ftsInsert(h.id, p)
		return change, nil
	}
	res.Versioned++
	// Clear the head slot before inserting the new version.
	if _, err := w.exec(`UPDATE messages SET superseded = 1, superseded_by = -1,
		superseded_in_generation = coalesce(superseded_in_generation, ?) WHERE id = ?`, b.Generation, h.id); err != nil {
		return 0, err
	}
	newID, err := w.insertMessage(b, convID, m, p, h.version+1)
	if err != nil {
		return 0, err
	}
	_, err = w.exec(`UPDATE messages SET superseded_by = ? WHERE id = ?`, newID, h.id)
	return rowChanged, err
}

const msgCols = `conversation_id, source_id, native_id, part, parent_native_id, ordinal, kind, role, tool_name, tool_call_id,
	is_error, ts, text, text_len, full_len, content_sha, version, superseded, superseded_in_generation, on_active_path,
	enrichment, source_generation, line_no, byte_offset, byte_len, locator, parser`

func (w *writeTx) insertMessage(b *Batch, convID int64, m *transcript.Message, p *prepared, version int64) (int64, error) {
	enr, err := enrichmentJSON(m)
	if err != nil {
		return 0, err
	}
	var supGen any
	if m.Superseded {
		supGen = b.Generation
	}
	row, err := w.queryRow(`INSERT INTO messages (`+msgCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		convID, b.SourceID, nullStr(m.NativeID), m.Part, nullStr(m.ParentNativeID), m.Ordinal, m.Kind.String(),
		nullStr(m.Role), nullStr(m.ToolName), nullStr(m.ToolCallID), boolInt(m.IsError), nullTime(m.TS),
		p.z, len(m.Text), m.FullLen, m.ContentSHA[:], version, boolInt(m.Superseded), supGen,
		nullBool(m.OnActivePath), enr, b.Generation, nullInt(m.LineNo), jsonlInt(m, m.ByteOffset), jsonlInt(m, m.ByteLen),
		nullStr(m.Locator), m.Parser)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, err
	}
	w.ftsInsertNew(id, p)
	return id, nil
}

// refresh rewrites a head row's mutable fields: metadata, locator, the
// generation stamp and the supersede state, and the text when newText is
// set (p non-nil). The ordinal from first sight is kept, and on_active_path and
// enrichment only change when the message carries a value.
func (w *writeTx) refresh(b *Batch, id int64, m *transcript.Message, enr any, newText *prepared) error {
	var supGen any
	if m.Superseded {
		supGen = b.Generation
	}
	var text []byte
	var textLen any
	if newText != nil {
		text, textLen = newText.z, len(m.Text)
	}
	_, err := w.exec(`UPDATE messages SET source_id = ?, parent_native_id = ?, kind = ?, role = ?, tool_name = ?, tool_call_id = ?,
		  is_error = ?, ts = coalesce(?, ts), full_len = ?, content_sha = ?,
		  superseded = ?, superseded_in_generation = CASE WHEN ? THEN coalesce(superseded_in_generation, ?) ELSE NULL END,
		  on_active_path = coalesce(?, on_active_path), enrichment = coalesce(?, enrichment), source_generation = ?,
		  line_no = ?, byte_offset = ?, byte_len = ?, locator = ?, parser = ?,
		  text = coalesce(?, text), text_len = coalesce(?, text_len)
		WHERE id = ?`,
		b.SourceID, nullStr(m.ParentNativeID), m.Kind.String(), nullStr(m.Role), nullStr(m.ToolName), nullStr(m.ToolCallID),
		boolInt(m.IsError), nullTime(m.TS), m.FullLen, m.ContentSHA[:],
		boolInt(m.Superseded), boolInt(m.Superseded), supGen,
		nullBool(m.OnActivePath), enr, b.Generation,
		nullInt(m.LineNo), jsonlInt(m, m.ByteOffset), jsonlInt(m, m.ByteLen), nullStr(m.Locator), m.Parser,
		text, textLen, id)
	return err
}

// FTS writes are buffered per transaction and applied at its end. FTS5
// flushes its in-memory segment whenever a statement opens a savepoint,
// which every INSERT into messages does inside a transaction (and the
// writer's per-request savepoints do); interleaving FTS writes with them
// would create one tiny segment per row. Applied last, row by row with one
// prepared statement, they build one segment per table per transaction
// (more when the pending text passes the table's hashsize).
type ftsPending struct {
	del   []int64             // rows indexed before this transaction whose text changed
	ins   map[int64]*prepared // row id -> text to index
	news  map[int64]bool      // rows first inserted in this transaction
	bytes int                 // text in ins
	log   []ftsUndo           // for rolling back one request
}

type ftsUndo struct {
	id       int64
	prev     *prepared
	hadPrev  bool
	newsSet  bool
	delAdded bool
}

func (f *ftsPending) mark() int { return len(f.log) }

// undo reverts the buffer to mark, for a request rolled back to its
// savepoint.
func (f *ftsPending) undo(mark int) {
	for i := len(f.log) - 1; i >= mark; i-- {
		u := f.log[i]
		if u.delAdded {
			f.del = f.del[:len(f.del)-1]
		}
		if u.newsSet {
			delete(f.news, u.id)
		}
		f.set(u.id, u.prev, u.hadPrev)
	}
	f.log = f.log[:mark]
}

// set replaces the pending text of id (removing it when !ok), keeping bytes.
func (f *ftsPending) set(id int64, p *prepared, ok bool) {
	if cur, had := f.ins[id]; had {
		f.bytes -= len(cur.text)
	}
	if !ok {
		delete(f.ins, id)
		return
	}
	if f.ins == nil {
		f.ins = map[int64]*prepared{}
	}
	f.ins[id] = p
	f.bytes += len(p.text)
}

func (w *writeTx) ftsInsert(id int64, p *prepared) {
	f := &w.fts
	prev, had := f.ins[id]
	f.log = append(f.log, ftsUndo{id: id, prev: prev, hadPrev: had})
	f.set(id, p, p.text != "")
}

func (w *writeTx) ftsInsertNew(id int64, p *prepared) {
	w.ftsInsert(id, p)
	if w.fts.news == nil {
		w.fts.news = map[int64]bool{}
	}
	if !w.fts.news[id] {
		w.fts.news[id] = true
		w.fts.log[len(w.fts.log)-1].newsSet = true
	}
}

// ftsDelete drops a row's indexed text: any text queued in this
// transaction, and the committed entry unless the row is new.
func (w *writeTx) ftsDelete(id int64) error {
	f := &w.fts
	prev, had := f.ins[id]
	u := ftsUndo{id: id, prev: prev, hadPrev: had}
	f.set(id, nil, false)
	if !f.news[id] {
		f.del = append(f.del, id)
		u.delAdded = true
	}
	f.log = append(f.log, u)
	return nil
}

// queueFTS records the transaction's FTS changes in fts_queue and returns
// them as one work item per shard, to hand over after the commit. It also
// prunes queue entries every shard has applied.
func (w *writeTx) queueFTS() ([]*ftsWork, error) {
	defer func() { w.fts = ftsPending{} }()
	applied := int64(-1)
	for _, sh := range w.s.shards {
		if a := sh.appliedSeq(); applied < 0 || a < applied {
			applied = a
		}
	}
	if applied > 0 {
		if _, err := w.exec(`DELETE FROM fts_queue WHERE seq <= ?`, applied); err != nil {
			return nil, err
		}
	}
	f := &w.fts
	if len(f.del) == 0 && len(f.ins) == 0 {
		return nil, nil
	}
	q, err := w.stmt(`INSERT INTO fts_queue (msg_id, op) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	var seq int64
	enqueue := func(id int64, op int) error {
		res, err := q.ExecContext(w.ctx, id, op)
		if err != nil {
			return err
		}
		seq, err = res.LastInsertId()
		return err
	}
	works := make([]*ftsWork, len(w.s.shards))
	for i := range works {
		works[i] = &ftsWork{}
	}
	for _, id := range f.del {
		if err := enqueue(id, opDel); err != nil {
			return nil, err
		}
		for i, sh := range w.s.shards {
			if sh.owns(id) {
				works[i].add(ftsOp{seq: seq, id: id, del: true})
			}
		}
	}
	ids := make([]int64, 0, len(f.ins))
	for id := range f.ins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := enqueue(id, opIns); err != nil {
			return nil, err
		}
		text := f.ins[id].text
		for i, sh := range w.s.shards {
			if sh.owns(id) {
				works[i].add(ftsOp{seq: seq, id: id, text: text})
			}
		}
	}
	for _, wk := range works {
		wk.seq, wk.scrub = seq, w.scrub
	}
	if w.scrub {
		// Durable, so Open compacts a shard that stopped before it did.
		if _, err := w.exec(`INSERT INTO meta (key, value) VALUES ('fts_scrub_seq', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, strconv.FormatInt(seq, 10)); err != nil {
			return nil, err
		}
	}
	return works, nil
}

// SupersedeAbsent marks superseded every live row of the source that
// generation gen did not touch (its source_generation is older), recording
// gen as superseded_in_generation. Rows are kept and stay searchable with
// include_superseded. With sessionIDs, only those conversations are
// considered (a SQLite source re-read one session at a time). Pass a
// generation above any real one to retire a source that vanished.
func (s *Store) SupersedeAbsent(ctx context.Context, sourceID, gen int64, sessionIDs ...string) (int64, error) {
	var n int64
	err := s.write(ctx, func(w *writeTx) error {
		var err error
		n, err = w.supersedeAbsent(sourceID, gen, sessionIDs...)
		return err
	})
	return n, err
}

func (w *writeTx) supersedeAbsent(sourceID, gen int64, sessionIDs ...string) (int64, error) {
	q := `UPDATE messages SET superseded = 1, superseded_in_generation = ?
		WHERE source_id = ? AND source_generation < ? AND superseded = 0`
	args := []any{gen, sourceID, gen}
	if len(sessionIDs) > 0 {
		q += ` AND conversation_id IN (SELECT c.id FROM conversations c, json_each(?) j
			WHERE c.device_id = ? AND c.session_id = j.value AND c.agent = (SELECT agent FROM sources WHERE id = ?))`
		args = append(args, jsonArray(sessionIDs), w.s.opts.DeviceID, sourceID)
	}
	rows, err := w.tx.QueryContext(w.ctx, q+` RETURNING conversation_id`, args...)
	if err != nil {
		return 0, err
	}
	var n int64
	var convs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		n++
		if !slices.Contains(convs, id) {
			convs = append(convs, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, w.recountDigests(convs)
}

// recountDigests recounts the digests of conversations whose rows were
// superseded outside an append.
func (w *writeTx) recountDigests(convs []int64) error {
	slices.Sort(convs)
	for _, id := range convs {
		if err := w.refreshDigest(id, nil, nil, true); err != nil {
			return fmt.Errorf("digest of conversation %d: %w", id, err)
		}
	}
	return nil
}

// SetActivePath sets on_active_path for every row of a conversation that
// has a native id: true for ids in active, false for the rest (Devin
// main_chain_id). Rows without a native id are left untouched.
func (s *Store) SetActivePath(ctx context.Context, agent transcript.Agent, sessionID string, active []string) (int64, error) {
	var n int64
	err := s.write(ctx, func(w *writeTx) error {
		r, err := w.exec(`UPDATE messages SET on_active_path = (native_id IN (SELECT value FROM json_each(?)))
			WHERE native_id IS NOT NULL AND conversation_id =
			  (SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?)
			  AND on_active_path IS NOT (native_id IN (SELECT value FROM json_each(?)))`,
			jsonArray(active), w.s.opts.DeviceID, string(agent), sessionID, jsonArray(active))
		if err != nil {
			return err
		}
		if n, err = r.RowsAffected(); err != nil || n == 0 {
			return err
		}
		return w.recountSession(agent, sessionID)
	})
	return n, err
}

// SupersedeSession marks every live row of a session superseded in gen
// (transcript.SessionSuperseder semantics: a multi-session source lost the
// session or rows of it). Rows the parser emits again later are revived by
// the upsert. It returns the number of rows marked.
func (s *Store) SupersedeSession(ctx context.Context, agent transcript.Agent, sessionID string, gen int64) (int64, error) {
	var n int64
	err := s.write(ctx, func(w *writeTx) error {
		r, err := w.exec(`UPDATE messages SET superseded = 1, superseded_in_generation = coalesce(superseded_in_generation, ?)
			WHERE superseded = 0 AND conversation_id =
			  (SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?)`,
			gen, w.s.opts.DeviceID, string(agent), sessionID)
		if err != nil {
			return err
		}
		if n, err = r.RowsAffected(); err != nil || n == 0 {
			return err
		}
		return w.recountSession(agent, sessionID)
	})
	return n, err
}

// recountSession recounts the digest of this device's conversation of a
// session.
func (w *writeTx) recountSession(agent transcript.Agent, sessionID string) error {
	row, err := w.queryRow(`SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?`, w.s.opts.DeviceID, string(agent), sessionID)
	if err != nil {
		return err
	}
	var conv int64
	if err := row.Scan(&conv); err != nil {
		return err
	}
	return w.recountDigests([]int64{conv})
}

// TombstoneConversation records that a session vanished from its source
// (a deleted Devin session): every live row becomes superseded in gen and
// the conversation is flagged. Nothing is deleted.
func (s *Store) TombstoneConversation(ctx context.Context, agent transcript.Agent, sessionID string, gen int64) error {
	return s.write(ctx, func(w *writeTx) error {
		row, err := w.queryRow(`UPDATE conversations SET deleted_in_generation = ?
			WHERE device_id = ? AND agent = ? AND session_id = ? RETURNING id`, gen, w.s.opts.DeviceID, string(agent), sessionID)
		if err != nil {
			return err
		}
		var id int64
		if err := row.Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err = w.exec(`UPDATE messages SET superseded = 1, superseded_in_generation = ? WHERE conversation_id = ? AND superseded = 0`, gen, id); err != nil {
			return err
		}
		return w.recountDigests([]int64{id})
	})
}

// PurgeConversation hard-deletes a conversation, its rows, their FTS
// entries and its companions. Indexing never purges; this is for an owner's
// explicit delete.
func (s *Store) PurgeConversation(ctx context.Context, agent transcript.Agent, sessionID string) error {
	return s.write(ctx, func(w *writeTx) error {
		row, err := w.queryRow(`SELECT id FROM conversations WHERE device_id = ? AND agent = ? AND session_id = ?`, w.s.opts.DeviceID, string(agent), sessionID)
		if err != nil {
			return err
		}
		var convID int64
		if err := row.Scan(&convID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		ids, err := w.tx.QueryContext(w.ctx, `SELECT id FROM messages WHERE conversation_id = ?`, convID)
		if err != nil {
			return err
		}
		for ids.Next() {
			var id int64
			if err := ids.Scan(&id); err != nil {
				ids.Close()
				return err
			}
			w.ftsDelete(id)
		}
		ids.Close()
		if err := ids.Err(); err != nil {
			return err
		}
		for _, stmt := range []string{
			`DELETE FROM messages WHERE conversation_id = ?`,
			`DELETE FROM companions WHERE conversation_id = ?`,
			`UPDATE conversations SET parent_conversation_id = NULL, spawned_by_message_id = NULL WHERE parent_conversation_id = ?`,
			`DELETE FROM conversations WHERE id = ?`,
		} {
			if _, err := w.exec(stmt, convID); err != nil {
				return err
			}
		}
		return nil
	})
}

func enrichmentJSON(m *transcript.Message) (any, error) {
	if len(m.Enrichment) == 0 {
		return nil, nil
	}
	j, err := json.Marshal(m.Enrichment)
	if err != nil {
		return nil, err
	}
	return string(j), nil
}

// jsonlInt returns v for rows located by byte offset (JSONL) and NULL for
// rows located by Locator.
func jsonlInt(m *transcript.Message, v int64) any {
	if m.Locator != "" && m.LineNo == 0 && v == 0 {
		return nil
	}
	return v
}

func jsonArray(ss []string) string {
	if ss == nil {
		ss = []string{}
	}
	j, _ := json.Marshal(ss)
	return string(j)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return boolInt(*b)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
