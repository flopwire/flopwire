package localindex

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// changeIdentity uses parser identity, never chronological position alone.
func changeIdentity(r digest.EvidenceRef) string {
	if r.NativeID != "" {
		return fmt.Sprintf("n:%s:%d", r.NativeID, r.Part)
	}
	if r.Locator != "" {
		return fmt.Sprintf("l:%s:%d", r.Locator, r.Part)
	}
	return fmt.Sprintf("b:%d:%d", r.ByteOffset, r.Part)
}
func (w *writeTx) invalidateChanges() error {
	_, err := w.exec(`INSERT INTO meta(key,value) VALUES('change_revision','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1`)
	return err
}
func (w *writeTx) removeChangeEvents(conv int64) error {
	r, err := w.exec(`DELETE FROM change_events WHERE conversation_id=?`, conv)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return w.invalidateChanges()
	}
	return nil
}

// refreshChangeEvents replaces the derived projection in the same transaction
// as source rows and watermarks. Unchanged events retain their ingestion IDs.
func (w *writeTx) refreshChangeEvents(conv int64) error {
	if w.s.opts.SyncOnly {
		return nil
	}
	// An existing event must always be reconciled, including its removal. A
	// retained unknown result (from an older schema) also forces replay. Only
	// the absence of both events and possible hash-bearing live results proves
	// that replay cannot produce an observation, regardless of call lifecycle.
	var needed bool
	row, err := w.queryRow(`SELECT EXISTS(SELECT 1 FROM change_events WHERE conversation_id=?) OR EXISTS(SELECT 1 FROM messages WHERE conversation_id=? AND kind='tool_result' AND superseded=0 AND on_active_path IS NOT 0 AND change_result_eligible IS NOT 0)`, conv, conv)
	if err != nil {
		return err
	}
	if err := row.Scan(&needed); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	if c := w.changeCache[conv]; c != nil {
		if hit, err := w.extendChangeEvents(conv, c); err != nil || hit {
			return err
		}
	}
	return w.replayChangeEvents(conv)
}

// replayChangeEvents is the complete retained-row projection, also used by
// migration and differential tests independently of the eligibility shortcut.
func (w *writeTx) replayChangeEvents(conv int64) error {
	delete(w.changeCache, conv)
	if w.s.opts.SyncOnly {
		return nil
	}
	rows, err := w.tx.QueryContext(w.ctx, `SELECT m.id,ifnull(m.native_id,''),m.ordinal,m.part,m.kind,ifnull(m.tool_name,''),ifnull(m.tool_call_id,''),ifnull(m.is_error,0),m.text,ifnull(m.line_no,0),ifnull(m.byte_offset,0),ifnull(m.byte_len,0),ifnull(m.locator,''),m.source_id,m.source_generation,ifnull(m.enrichment,'{}'),m.change_result_eligible,(m.superseded_by IS NULL AND (m.native_id IS NULL OR m.native_id<>'') AND (m.locator IS NULL OR m.locator<>'') AND (m.native_id IS NOT NULL OR m.locator IS NOT NULL OR m.byte_offset IS NOT NULL)) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.conversation_id=? AND m.superseded=0 AND m.on_active_path IS NOT 0 AND c.deleted_in_generation IS NULL AND (m.kind='tool_call' OR m.kind='tool_result') ORDER BY m.source_id,m.ordinal,m.id`, conv)
	if err != nil {
		return err
	}
	type group struct {
		gen    int64
		msgs   []transcript.Message
		ids    map[string]int64
		stream changeStream
		events []digest.CommitEvidence
	}
	type sourceGeneration struct{ source, gen int64 }
	groups := map[sourceGeneration]*group{}
	type classification struct {
		id       int64
		eligible bool
	}
	var unknown []classification
	cacheCanonical := true
	var decoder replayTextDecoder
	for rows.Next() {
		var source, gen, id int64
		var isError int
		var eligible sql.NullBool
		var enrichment, kind string
		var text []byte
		var m transcript.Message
		var canonical bool
		if err := rows.Scan(&id, &m.NativeID, &m.Ordinal, &m.Part, &kind, &m.ToolName, &m.ToolCallID, &isError, &text, &m.LineNo, &m.ByteOffset, &m.ByteLen, &m.Locator, &source, &gen, &enrichment, &eligible, &canonical); err != nil {
			rows.Close()
			return err
		}
		cacheCanonical = cacheCanonical && canonical
		m.Kind, _ = transcript.ParseKind(kind)
		m.IsError = isError != 0
		if m.Text, err = decoder.decode(text); err != nil {
			rows.Close()
			return fmt.Errorf("row %d: %w", id, err)
		}
		if !eligible.Valid {
			unknown = append(unknown, classification{id, digest.MayContainCommitEvidence(m.Text)})
		}
		key := sourceGeneration{source, gen}
		g := groups[key]
		if g == nil {
			g = &group{gen: gen, ids: map[string]int64{}}
			groups[key] = g
		}
		if err := json.Unmarshal([]byte(enrichment), &m.Enrichment); err != nil {
			rows.Close()
			return err
		}
		if w.changeAppend {
			g.events = append(g.events, g.stream.state.Step(m)...)
			g.stream.ordinal, g.stream.id = m.Ordinal, id
		} else {
			g.msgs = append(g.msgs, m)
		}
		ref := digest.EvidenceRef{NativeID: m.NativeID, Part: m.Part, ByteOffset: m.ByteOffset, Locator: m.Locator}
		g.ids[changeIdentity(ref)] = id
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	// Classify old stored text during the replay that already decoded it.
	// Buffer updates until the reader closes, and keep them in this transaction.
	for _, c := range unknown {
		if _, err := w.exec(`UPDATE messages SET change_result_eligible=? WHERE id=?`, boolInt(c.eligible), c.id); err != nil {
			return err
		}
	}
	type stored struct {
		id                    int64
		evidence              string
		source, gen, cmd, res int64
		rc, rr                sql.NullInt64
	}
	old := map[string]stored{}
	rows, err = w.tx.QueryContext(w.ctx, `SELECT id,event_key,evidence,source_id,source_generation,command_id,result_id,reveal_command_id,reveal_result_id FROM change_events WHERE conversation_id=?`, conv)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var s stored
		if err = rows.Scan(&s.id, &k, &s.evidence, &s.source, &s.gen, &s.cmd, &s.res, &s.rc, &s.rr); err != nil {
			rows.Close()
			return err
		}
		old[k] = s
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	invalid := false
	// Sort source IDs so ingestion order does not depend on Go map iteration.
	sources := make([]sourceGeneration, 0, len(groups))
	for source := range groups {
		sources = append(sources, source)
	}
	slices.SortFunc(sources, func(a, b sourceGeneration) int {
		if n := cmp.Compare(a.source, b.source); n != 0 {
			return n
		}
		return cmp.Compare(a.gen, b.gen)
	})
	for _, scope := range sources {
		source := scope.source
		g := groups[scope]
		events := g.events
		if !w.changeAppend {
			events = digest.ExtractCommitEvidence(g.msgs)
		}
		for _, ev := range events {
			cmd, res := g.ids[changeIdentity(ev.Command)], g.ids[changeIdentity(ev.Result)]
			if cmd == 0 || res == 0 {
				continue
			}
			var rc, rr sql.NullInt64
			if ev.RevealCommand != nil {
				rc.Int64 = g.ids[changeIdentity(*ev.RevealCommand)]
				rc.Valid = rc.Int64 != 0
				if !rc.Valid {
					continue
				}
			}
			if ev.RevealResult != nil {
				rr.Int64 = g.ids[changeIdentity(*ev.RevealResult)]
				rr.Valid = rr.Int64 != 0
				if !rr.Valid {
					continue
				}
			}
			b, e := json.Marshal(ev)
			if e != nil {
				return e
			}
			key := fmt.Sprintf("%d/%s/%s/%s", source, changeIdentity(ev.Command), changeIdentity(ev.Result), ev.Hash)
			if ev.RevealCommand != nil || ev.RevealResult != nil {
				var command, result string
				if ev.RevealCommand != nil {
					command = changeIdentity(*ev.RevealCommand)
				}
				if ev.RevealResult != nil {
					result = changeIdentity(*ev.RevealResult)
				}
				key += fmt.Sprintf("/reveal:%q:%q", command, result)
			}
			prev, exists := old[key]
			delete(old, key)
			// Generation changes alone do not alter addressed evidence. Physical address
			// changes are represented inside ev and invalidate the old observation.
			if exists && prev.evidence == string(b) && prev.cmd == cmd && prev.res == res && prev.rc == rc && prev.rr == rr {
				if prev.gen != g.gen {
					if _, err = w.exec(`UPDATE change_events SET source_generation=? WHERE id=?`, g.gen, prev.id); err != nil {
						return err
					}
				}
				continue
			}
			if exists {
				invalid = true
				if _, err = w.exec(`DELETE FROM change_events WHERE id=?`, prev.id); err != nil {
					return err
				}
			}
			_, err = w.exec(`INSERT INTO change_events(source_id,source_generation,conversation_id,event_key,observed_hash,command_id,result_id,reveal_command_id,reveal_result_id,evidence) VALUES(?,?,?,?,?,?,?,?,?,?)`, source, g.gen, conv, key, ev.Hash, cmd, res, rc, rr, string(b))
			if err != nil {
				return err
			}
		}
	}
	for _, prev := range old {
		invalid = true
		if _, err = w.exec(`DELETE FROM change_events WHERE id=?`, prev.id); err != nil {
			return err
		}
	}
	if invalid {
		if err := w.invalidateChanges(); err != nil {
			return err
		}
	}
	if w.changeAppend && cacheCanonical {
		var visible bool
		row, err := w.queryRow(`SELECT deleted_in_generation IS NULL FROM conversations WHERE id=?`, conv)
		if err != nil {
			return err
		}
		if err := row.Scan(&visible); err != nil {
			return err
		}
		if !visible {
			delete(w.changeCache, conv)
			delete(w.changeAdded, conv)
			return nil
		}
		c := &changeCheckpoint{groups: map[changeScope]*changeStream{}}
		for scope, g := range groups {
			// Copy out of the replay group: an interior pointer would keep its
			// historical message slice and ID map reachable for the transaction.
			stream := g.stream
			c.groups[changeScope{scope.source, scope.gen}] = &stream
		}
		w.keepChangeCheckpoint(conv, c)
	}
	delete(w.changeAdded, conv)
	return nil
}

type ChangeRecord struct {
	SourcePath                      string
	ID, ConversationID              int64
	CommandID, ResultID             int64
	RevealCommandID, RevealResultID sql.NullInt64
	Evidence                        digest.CommitEvidence
}
type ChangeSnapshot struct {
	SourceTextTruncated bool
	IndexID             string
	Revision, Cutoff    int64
}

// ChangeRecords reads one bounded page and its mutation state in a snapshot.
func (s *Store) ChangeRecords(ctx context.Context, commits []string, f Filter, cutoff, after int64) (records []ChangeRecord, state ChangeSnapshot, lag bool, err error) {
	tx, e := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		err = e
		return
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT (SELECT value FROM meta WHERE key='index_id'),ifnull((SELECT CAST(value AS INTEGER) FROM meta WHERE key='change_revision'),0),ifnull((SELECT max(id) FROM change_events),0)`).Scan(&state.IndexID, &state.Revision, &state.Cutoff)
	if err != nil {
		return
	}
	if cutoff >= 0 {
		state.Cutoff = cutoff
	}
	// Expand each input into its possible recorded prefixes. Hash equality uses
	// the covering B-tree index; prefix length never establishes uniqueness.
	var hashes []string
	for _, h := range commits {
		for n := 7; n <= len(h); n++ {
			hashes = append(hashes, h[:n])
		}
	}
	slices.Sort(hashes)
	hashes = slices.Compact(hashes)
	where, args := f.where()
	args = append([]any{jsonArray(hashes), after, state.Cutoff}, args...)
	q := `SELECT e.id,e.conversation_id,e.command_id,e.result_id,e.reveal_command_id,e.reveal_result_id,e.evidence,ifnull(sc.path,'') FROM change_events e JOIN messages m ON m.id=e.command_id JOIN conversations c ON c.id=e.conversation_id LEFT JOIN sources sc ON sc.id=e.source_id JOIN messages r ON r.id=e.result_id LEFT JOIN messages rc ON rc.id=e.reveal_command_id LEFT JOIN messages rr ON rr.id=e.reveal_result_id WHERE e.observed_hash IN (SELECT value FROM json_each(?)) AND e.id>? AND e.id<=? AND c.deleted_in_generation IS NULL AND m.source_id=e.source_id AND r.source_id=e.source_id AND m.source_generation=e.source_generation AND r.source_generation=e.source_generation AND r.superseded=0 AND r.on_active_path IS NOT 0 AND (e.reveal_command_id IS NULL OR (rc.id IS NOT NULL AND rc.source_id=e.source_id AND rc.source_generation=e.source_generation AND rc.superseded=0 AND rc.on_active_path IS NOT 0)) AND (e.reveal_result_id IS NULL OR (rr.id IS NOT NULL AND rr.source_id=e.source_id AND rr.source_generation=e.source_generation AND rr.superseded=0 AND rr.on_active_path IS NOT 0)) AND ` + where + ` ORDER BY e.id LIMIT 101`
	rows, e := tx.QueryContext(ctx, q, args...)
	if e != nil {
		err = e
		return
	}
	defer rows.Close()
	records = []ChangeRecord{}
	for rows.Next() {
		var r ChangeRecord
		var b string
		if err = rows.Scan(&r.ID, &r.ConversationID, &r.CommandID, &r.ResultID, &r.RevealCommandID, &r.RevealResultID, &b, &r.SourcePath); err != nil {
			return
		}
		if err = json.Unmarshal([]byte(b), &r.Evidence); err != nil {
			return
		}
		records = append(records, r)
	}
	err = rows.Err()
	if err != nil {
		return
	}
	rows.Close()
	// Source extraction checkpoints identify retained sources awaiting their
	// first parse. This does not claim that absent device history is available.
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sources s JOIN conversations c ON c.source_id=s.id WHERE s.extraction_report IS NULL AND c.deleted_in_generation IS NULL)`).Scan(&lag)
	if err != nil {
		return
	}
	coverageWhere, coverageArgs := f.where()
	// Row lengths are metadata; this coverage query never decompresses text.
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.deleted_in_generation IS NULL AND m.full_len>m.text_len AND `+coverageWhere+`)`, coverageArgs...).Scan(&state.SourceTextTruncated)

	return
}

// ChangeState rechecks the projection after output metadata hydration.
func (s *Store) ChangeState(ctx context.Context) (state ChangeSnapshot, err error) {
	err = s.rdb.QueryRowContext(ctx, `SELECT (SELECT value FROM meta WHERE key='index_id'),ifnull((SELECT CAST(value AS INTEGER) FROM meta WHERE key='change_revision'),0),ifnull((SELECT max(id) FROM change_events),0)`).Scan(&state.IndexID, &state.Revision, &state.Cutoff)
	return
}

func (s *Store) ChangeCursorSecret(ctx context.Context) ([]byte, error) {
	var secret string
	if err := s.rdb.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='change_cursor_secret'`).Scan(&secret); err != nil {
		return nil, err
	}
	return hex.DecodeString(secret)
}

// changeRelevant follows the projection's input kinds, independently of digest
// counts. Even an unchanged tool call can move its evidence address or source
// generation, complete a previously retained result, or introduce a poll or
// HEAD-move barrier. Results can affect that lifecycle without printing a hash,
// and output decoding can reveal hashes absent from the raw text.
func changeRelevant(msgs []*transcript.Message) bool {
	for _, m := range msgs {
		if m.Kind == transcript.KindToolCall || m.Kind == transcript.KindToolResult {
			return true
		}
	}
	return false
}
