package localindex

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// A checkpoint lives only in this writer transaction. Only ApplyBatch and
// audited source reads preserve it. Every other request and every failed
// request discards all checkpoints.
const changeCacheBudget int64 = 8 << 20
const changeCacheGroups = 256

type changeScope struct{ source, gen int64 }
type changeStream struct {
	state       digest.CommitEvidenceState
	ordinal, id int64
}
type changeCheckpoint struct{ groups map[changeScope]*changeStream }

// Compare effective stored metadata, including coalesced paths and references.
// Text-writing upsert branches invalidate before mutation; refresh(nil) keeps
// the exact stored text. Tool-call enrichment is erased before extractor use.
func (w *writeTx) changeInput(id int64) (string, error) {
	row, err := w.queryRow(`SELECT json_array(source_id,source_generation,ifnull(native_id,''),ordinal,part,kind,ifnull(tool_name,''),ifnull(tool_call_id,''),ifnull(is_error,0),ifnull(line_no,0),ifnull(byte_offset,0),ifnull(byte_len,0),ifnull(locator,''),superseded,on_active_path,CASE WHEN kind='tool_call' THEN '' ELSE ifnull(enrichment,'{}') END) FROM messages WHERE id=?`, id)
	if err != nil {
		return "", err
	}
	var input string
	err = row.Scan(&input)
	return input, err
}

func (w *writeTx) keepChangeCheckpoint(conv int64, c *changeCheckpoint) {
	if !w.changeAppend {
		return
	}
	if w.changeCache == nil {
		w.changeCache = map[int64]*changeCheckpoint{}
		w.changeMapPeak = 0
	}
	w.changeCache[conv] = c
	w.changeMapPeak = max(w.changeMapPeak, len(w.changeCache))
	if !w.changeCacheWithinBudget() {
		w.changeCache = nil
	}
}
func (w *writeTx) changeCacheWithinBudget() bool {
	size := int64(w.changeMapPeak) * 128
	groups := 0
	for _, c := range w.changeCache {
		size += 256
		for _, g := range c.groups {
			groups++
			size += 256 + g.state.RetainedSize()
		}
	}
	return groups <= changeCacheGroups && size <= changeCacheBudget
}

// The identity predicates deliberately follow changeIdentity's precedence and
// replay's NULL normalization. ORDER BY reproduces the replay map's last write.
func changeRefPredicate(r digest.EvidenceRef) (string, []any) {
	// Checkpoint seeding excludes noncanonical legacy rows and non-head live
	// versions. These predicates use the existing unique head indexes: each
	// physical identity lookup examines at most one candidate, not its history.
	if r.NativeID != "" {
		return `native_id=? AND part=? AND superseded_by IS NULL`, []any{r.NativeID, r.Part}
	}
	if r.Locator != "" {
		return `native_id IS NULL AND superseded_by IS NULL AND ifnull(locator,byte_offset)=? AND part=?`, []any{r.Locator, r.Part}
	}
	return `native_id IS NULL AND superseded_by IS NULL AND ifnull(locator,byte_offset)=? AND part=?`, []any{r.ByteOffset, r.Part}
}
func changeRefIndex(r digest.EvidenceRef) string {
	if r.NativeID != "" {
		return "messages_native_head"
	}
	return "messages_locator_head"
}
func (w *writeTx) changeRefID(conv int64, scope changeScope, r digest.EvidenceRef) (int64, error) {
	pred, args := changeRefPredicate(r)
	args = append([]any{conv, scope.source, scope.gen}, args...)
	row, err := w.queryRow(`SELECT id FROM messages INDEXED BY `+changeRefIndex(r)+` WHERE conversation_id=? AND source_id=? AND source_generation=? AND superseded=0 AND on_active_path IS NOT 0 AND kind IN ('tool_call','tool_result') AND `+pred+` ORDER BY ordinal DESC,id DESC LIMIT 1`, args...)
	if err != nil {
		return 0, err
	}
	var id int64
	err = row.Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func changeEventKey(source int64, ev digest.CommitEvidence) string {
	key := fmt.Sprintf("%d/%s/%s/%s", source, changeIdentity(ev.Command), changeIdentity(ev.Result), ev.Hash)
	if ev.RevealCommand != nil || ev.RevealResult != nil {
		var c, r string
		if ev.RevealCommand != nil {
			c = changeIdentity(*ev.RevealCommand)
		}
		if ev.RevealResult != nil {
			r = changeIdentity(*ev.RevealResult)
		}
		key += fmt.Sprintf("/reveal:%q:%q", c, r)
	}
	return key
}

// insertChangeDelta stores an observation after proving its key is new.
// An unchanged prefix cannot remove or revise earlier observations.
func (w *writeTx) insertChangeDelta(conv int64, scope changeScope, ev digest.CommitEvidence) error {
	cmd, err := w.changeRefID(conv, scope, ev.Command)
	if err != nil {
		return err
	}
	res, err := w.changeRefID(conv, scope, ev.Result)
	if err != nil {
		return err
	}
	if cmd == 0 || res == 0 {
		return nil
	}
	var rc, rr sql.NullInt64
	if ev.RevealCommand != nil {
		rc.Int64, err = w.changeRefID(conv, scope, *ev.RevealCommand)
		if err != nil {
			return err
		}
		rc.Valid = rc.Int64 != 0
		if !rc.Valid {
			return nil
		}
	}
	if ev.RevealResult != nil {
		rr.Int64, err = w.changeRefID(conv, scope, *ev.RevealResult)
		if err != nil {
			return err
		}
		rr.Valid = rr.Int64 != 0
		if !rr.Valid {
			return nil
		}
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	key := changeEventKey(scope.source, ev)

	_, err = w.exec(`INSERT INTO change_events(source_id,source_generation,conversation_id,event_key,observed_hash,command_id,result_id,reveal_command_id,reveal_result_id,evidence) VALUES(?,?,?,?,?,?,?,?,?,?)`, scope.source, scope.gen, conv, key, ev.Hash, cmd, res, rc, rr, string(b))
	return err
}

type changeNewRow struct {
	scope changeScope
	id    int64
	m     transcript.Message
}

// The JSON input contains at most 4096 fresh tool-row IDs. SQLite drives this
// fixed statement through primary-key lookups. NOT INDEXED excludes secondary
// conversation scans while permitting rowid seeks.
const changeFreshRowsSQL = `SELECT id,source_id,source_generation,ifnull(native_id,''),ordinal,part,kind,ifnull(tool_name,''),ifnull(tool_call_id,''),ifnull(is_error,0),text,ifnull(line_no,0),ifnull(byte_offset,0),ifnull(byte_len,0),ifnull(locator,''),ifnull(enrichment,'{}') FROM messages NOT INDEXED WHERE id IN (SELECT value FROM json_each(?)) AND conversation_id=? AND superseded=0 AND on_active_path IS NOT 0 AND kind IN ('tool_call','tool_result') AND EXISTS(SELECT 1 FROM conversations c WHERE c.id=messages.conversation_id AND c.deleted_in_generation IS NULL)`

func (w *writeTx) readFreshChangeRows(conv int64, ids []int64) ([]changeNewRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	st, err := w.stmt(changeFreshRowsSQL)
	if err != nil {
		return nil, err
	}
	rows, err := st.QueryContext(w.ctx, int64JSON(ids), conv)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fresh []changeNewRow
	for rows.Next() {
		var r changeNewRow
		var kind, enrichment string
		var text []byte
		var isError int
		if err := rows.Scan(&r.id, &r.scope.source, &r.scope.gen, &r.m.NativeID, &r.m.Ordinal, &r.m.Part, &kind, &r.m.ToolName, &r.m.ToolCallID, &isError, &text, &r.m.LineNo, &r.m.ByteOffset, &r.m.ByteLen, &r.m.Locator, &enrichment); err != nil {
			return nil, err
		}
		r.m.Kind, _ = transcript.ParseKind(kind)
		r.m.IsError = isError != 0
		if r.m.Text, err = decompress(text); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(enrichment), &r.m.Enrichment); err != nil {
			return nil, err
		}
		fresh = append(fresh, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return fresh, nil
}

// Only new physical rows are read here. Existing-row checks occur at the
// mutation, so validating a hit never scans the retained prefix.
func (w *writeTx) extendChangeEvents(conv int64, c *changeCheckpoint) (bool, error) {
	// Bound the transient list of fresh tool-row IDs; ordinary transcript rows
	// never enter this list. Over-budget requests use the existing full replay.
	if len(w.changeAdded[conv]) > 4096 || !w.changeCacheWithinBudget() {
		w.changeCache = nil
		return false, nil
	}
	fresh, err := w.readFreshChangeRows(conv, w.changeAdded[conv])
	if err != nil {
		return false, err
	}

	slices.SortFunc(fresh, func(a, b changeNewRow) int {
		if a.scope.source != b.scope.source {
			if a.scope.source < b.scope.source {
				return -1
			}
			return 1
		}
		if a.scope.gen != b.scope.gen {
			if a.scope.gen < b.scope.gen {
				return -1
			}
			return 1
		}
		if a.m.Ordinal != b.m.Ordinal {
			if a.m.Ordinal < b.m.Ordinal {
				return -1
			}
			return 1
		}
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})
	seen := map[string]bool{}
	// Prove all rows before touching state or writing events. Seed records every
	// retained group, so a previously absent scope has no old retained rows.
	for _, r := range fresh {
		identity := fmt.Sprintf("%d/%d/%s", r.scope.source, r.scope.gen, changeIdentity(digest.EvidenceRef{NativeID: r.m.NativeID, Part: r.m.Part, Locator: r.m.Locator, ByteOffset: r.m.ByteOffset}))
		if seen[identity] {
			delete(w.changeCache, conv)
			return false, nil
		}
		seen[identity] = true
		if g := c.groups[r.scope]; g != nil {
			if r.m.Ordinal < g.ordinal || (r.m.Ordinal == g.ordinal && r.id <= g.id) {
				delete(w.changeCache, conv)
				return false, nil
			}
			ref := digest.EvidenceRef{NativeID: r.m.NativeID, Part: r.m.Part, Locator: r.m.Locator, ByteOffset: r.m.ByteOffset}
			pred, args := changeRefPredicate(ref)
			args = append([]any{conv, r.scope.source, r.scope.gen, g.ordinal, g.ordinal, g.id}, args...)
			row, err := w.queryRow(`SELECT EXISTS(SELECT 1 FROM messages INDEXED BY `+changeRefIndex(ref)+` WHERE conversation_id=? AND source_id=? AND source_generation=? AND superseded=0 AND on_active_path IS NOT 0 AND kind IN ('tool_call','tool_result') AND (ordinal<? OR (ordinal=? AND id<=?)) AND `+pred+`)`, args...)
			if err != nil {
				return false, err
			}
			var collision bool
			if err = row.Scan(&collision); err != nil {
				return false, err
			}
			if collision {
				delete(w.changeCache, conv)
				return false, nil
			}
		}
	}
	type delta struct {
		scope changeScope
		ev    digest.CommitEvidence
	}
	var deltas []delta
	for _, r := range fresh {
		g := c.groups[r.scope]
		if g == nil {
			g = &changeStream{}
			c.groups[r.scope] = g
		}
		for _, ev := range g.state.Step(r.m) {
			deltas = append(deltas, delta{r.scope, ev})
		}
		g.ordinal, g.id = r.m.Ordinal, r.id
	}
	// Budget exhaustion discards the tentative state before any delta is stored.
	if !w.changeCacheWithinBudget() {
		w.changeCache = nil
		return false, nil
	}
	keys := map[string]bool{}
	for _, d := range deltas {
		key := changeEventKey(d.scope.source, d.ev)
		if keys[key] {
			delete(w.changeCache, conv)
			return false, nil
		}
		keys[key] = true
		row, err := w.queryRow(`SELECT EXISTS(SELECT 1 FROM change_events WHERE conversation_id=? AND event_key=?)`, conv, key)
		if err != nil {
			return false, err
		}
		var exists bool
		if err = row.Scan(&exists); err != nil {
			return false, err
		}
		if exists {
			delete(w.changeCache, conv)
			return false, nil
		}
	}
	for _, d := range deltas {
		if err := w.insertChangeDelta(conv, d.scope, d.ev); err != nil {
			return false, err
		}
	}
	delete(w.changeAdded, conv)
	if !w.changeCacheWithinBudget() {
		w.changeCache = nil
	}
	return true, nil
}
