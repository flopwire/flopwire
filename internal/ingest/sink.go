package ingest

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bound ordinary write batches by both rows and retained message text.
// This is not a total heap limit: parser buffers, metadata, and database
// encoding also use memory. A single larger message is written alone.
const (
	sinkBatch     = 500
	sinkTextBytes = 8 << 20
)

// source is what a parse knows about the source it writes for.
type source struct {
	id, deviceID, userID, agent string
	generation                  int64
	parseAttempt                int64
}

// sink writes parser output as conversation and message rows (spec §4.2).
// Every write transaction takes each touched conversation's natural-key
// lock and checks conversation_tombstones first (internal/store
// ConversationLockKey), so a deletion and a re-parse never interleave.
type sink struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	src     source
	convs   map[string]*transcript.Conversation // records not yet written
	convIDs map[string]string                   // session → conversation id; "" when tombstoned
	// replaced marks the conversations whose existing rows a flush
	// changed: their digests are recounted, not added to.
	replaced map[string]bool
	// repeated marks the messages a flush found stored already, unchanged
	// (a parse run again from an older cursor): the digest has them.
	repeated map[*transcript.Message]bool
	// kept is every stored row a flush left as it was, which keeps its
	// older parse_attempt: a full parse's checkpoint must not retire it.
	kept map[uuid.UUID]struct{}
	// dirty marks the conversations whose rows a flush replaced. Their
	// digests are recounted once, when the parse completes, not per batch.
	dirty map[string]bool
	// held marks the sessions whose natural-key locks the flush's
	// transaction holds (lockSession); nil outside a flush.
	held map[string]bool
	// parentless marks the sessions a committed flush found stored
	// without a parent and given none: later flushes need not lock a
	// parent for them (lockFlushSQL). Only another source writing the
	// same session can give it a parent meanwhile; refreshDigest then
	// locks that parent late, as before lockFlushSQL.
	parentless map[string]bool
	msgs       []*transcript.Message
	textBytes  int
	tombstoned bool // some session of this source is deleted
	written    int
	gate       *gate           // the admin path rules; nil when there are none
	tagged     map[string]bool // Codex sessions whose <cwd> tag this parse has recorded
	// maskRevision is the redacted_lines_revision of the catalog the parse
	// reads through. masksMoved is set when a write found it stale.
	maskRevision int64
	masksMoved   bool
}

func newSink(ctx context.Context, pool *pgxpool.Pool, src source) *sink {
	return &sink{ctx: ctx, pool: pool, src: src, convs: map[string]*transcript.Conversation{}, convIDs: map[string]string{}}
}

func (s *sink) Conversation(c *transcript.Conversation) error {
	s.convs[c.SessionID] = c
	return nil
}

func (s *sink) Message(m *transcript.Message) error {
	// Flush before retaining the next message. On failure the existing
	// batch stays intact and the caller receives the error.
	if len(s.msgs) > 0 && len(m.Text) > sinkTextBytes-s.textBytes {
		if err := s.flush(); err != nil {
			return err
		}
	}
	s.msgs = append(s.msgs, m)
	s.textBytes += len(m.Text)
	if len(s.msgs) >= sinkBatch || s.textBytes >= sinkTextBytes {
		return s.flush()
	}
	return nil
}

// SupersedeSession marks the session's live rows superseded (a deleted
// Devin session, or one that lost rows); rows emitted after it revive.
func (s *sink) SupersedeSession(agent transcript.Agent, sessionID string) error {
	if err := s.flush(); err != nil {
		return err
	}
	return pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		id, err := s.conversation(tx, sessionID, false)
		if err != nil || id == "" {
			return err
		}
		_, err = tx.Exec(s.ctx, `UPDATE messages SET superseded=true,superseded_in_generation=$2 WHERE conversation_id=$1 AND NOT superseded`, id, s.src.generation)
		return err
	})
}

// flush writes pending conversations and messages in one transaction.
func (s *sink) flush() error {
	if len(s.msgs) == 0 && len(s.convs) == 0 {
		return nil
	}
	s.placeTagged()
	if s.gate != nil {
		if err := s.gate.check(s.ctx, s); err != nil {
			return err
		}
	}
	defer func() { s.held = nil }()
	var learned map[string]bool // sessions found with (false) or without a parent
	err := pgx.BeginTxFunc(s.ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := s.checkMasks(tx); err != nil {
			return err
		}
		sessions := map[string]bool{}
		for id := range s.convs {
			sessions[id] = true
		}
		for _, m := range s.msgs {
			sessions[m.SessionID] = true
		}
		ids := make([]string, 0, len(sessions))
		for id := range sessions {
			ids = append(ids, id)
		}
		slices.Sort(ids) // lock order
		// The session locks first, then the stored conversations with
		// their parents in store.LockConversationsSQL's order: a
		// subagent's digest refresh updates its parent's count, and a
		// hide or deletion of the parent's tree locks the parent and its
		// subagents in that order. The upserts then hold every row they
		// lock already. Sessions an earlier flush of this parse found
		// without a parent need no such lock.
		s.held = map[string]bool{}
		var parents []string
		for _, id := range ids {
			if err := s.lockSession(tx, id, true); err != nil {
				return err
			}
			if c := s.convs[id]; c != nil && c.ParentSessionID != "" && c.ParentSessionID != id {
				parents = append(parents, c.ParentSessionID)
			}
		}
		learned = nil
		if len(parents) > 0 || slices.ContainsFunc(ids, func(id string) bool { return !s.parentless[id] }) {
			rows, err := tx.Query(s.ctx, lockFlushSQL, s.src.deviceID, s.src.agent, ids, parents)
			if err != nil {
				return err
			}
			stored := map[string]bool{}
			var session string
			var parented bool
			if _, err := pgx.ForEachRow(rows, []any{&session, &parented}, func() error {
				stored[session] = parented
				return nil
			}); err != nil {
				return err
			}
			learned = map[string]bool{}
			for _, id := range ids {
				c := s.convs[id]
				learned[id] = !stored[id] && (c == nil || c.ParentSessionID == "" || c.ParentSessionID == id)
			}
		}
		for _, id := range ids {
			if _, err := s.conversation(tx, id, true); err != nil {
				return err
			}
		}
		if err := s.writeMessages(tx); err != nil {
			return err
		}
		// Every append refreshes the digests of the conversations it
		// touched.
		byConv := map[string][]*transcript.Message{}
		for _, m := range s.msgs {
			if !s.repeated[m] {
				byConv[m.SessionID] = append(byConv[m.SessionID], m)
			}
		}
		for _, id := range ids {
			conv := s.convIDs[id]
			if conv == "" {
				continue
			}
			if s.replaced[conv] {
				if s.dirty == nil {
					s.dirty = map[string]bool{}
				}
				s.dirty[conv] = true
			}
			mode := digestAppend
			if s.dirty[conv] {
				mode = digestFold // counted once at the end (dirtyConversations)
			}
			var last time.Time
			if c := s.convs[id]; c != nil {
				last = c.LastActivityAt
			}
			if err := refreshDigest(s.ctx, tx, conv, byConv[id], mode, last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if learned != nil && s.parentless == nil {
		s.parentless = map[string]bool{}
	}
	for id, none := range learned {
		s.parentless[id] = none
	}
	s.written += len(s.msgs)
	clear(s.msgs) // release text retained by the reusable backing array
	s.msgs = s.msgs[:0]
	s.textBytes = 0
	clear(s.convs)
	clear(s.replaced)
	clear(s.repeated)
	return nil
}

// checkMasks shares the redacted-lines lock until tx ends, then checks
// that no line was redacted since the parse loaded its catalog: rows
// written from a stale catalog could hold a redacted line unmasked. The
// two statements go in one round trip; the second reads after the lock.
func (s *sink) checkMasks(tx pgx.Tx) error {
	b := &pgx.Batch{}
	b.Queue(`SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, redactedLinesLock)
	b.Queue(`SELECT revision FROM redacted_lines_revision WHERE singleton`)
	br := tx.SendBatch(s.ctx, b)
	if _, err := br.Exec(); err != nil {
		br.Close()
		return err
	}
	var rev int64
	if err := br.QueryRow().Scan(&rev); err != nil {
		br.Close()
		return err
	}
	if err := br.Close(); err != nil {
		return err
	}
	if rev != s.maskRevision {
		s.masksMoved = true
		return errMasksMoved
	}
	return nil
}

// dirtyConversations lists, sorted, the conversations whose digests wait
// for a full recount (see sink.dirty).
func (s *sink) dirtyConversations() []string {
	ids := make([]string, 0, len(s.dirty))
	for id := range s.dirty {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// placeTagged records the directory an older Codex rollout names only in
// the <cwd> tag of its injected <environment_context> as its
// conversation's cwd, as the agent places it, so the admin path rules
// (at parse time and in the sweep after a rule change) see it.
func (s *sink) placeTagged() {
	if s.src.agent != string(transcript.AgentCodex) {
		return
	}
	for _, m := range s.msgs {
		if s.tagged[m.SessionID] {
			continue
		}
		c := s.convs[m.SessionID]
		if c != nil && c.Cwd != "" {
			continue
		}
		sm := cwdTag.FindStringSubmatch(m.Text)
		if sm == nil {
			continue
		}
		if s.tagged == nil {
			s.tagged = map[string]bool{}
		}
		s.tagged[m.SessionID] = true
		if c == nil {
			// A stub upsert fills cwd and leaves the stored record be.
			c = &transcript.Conversation{Agent: transcript.AgentCodex, SessionID: m.SessionID}
			s.convs[m.SessionID] = c
		}
		c.Cwd = strings.TrimSpace(sm[1])
	}
}

// conversation locks the session's natural key (per user, across
// devices), checks its tombstone, and upserts the pending record (or a
// stub). It returns "" for a deleted session: one the user deleted, or a
// subagent session whose parent they deleted, which is then tombstoned
// too so it stays gone. Without create, a missing conversation is not
// created.
func (s *sink) conversation(tx pgx.Tx, sessionID string, create bool) (string, error) {
	ctx := s.ctx
	c := s.convs[sessionID]
	parent := ""
	if c != nil && create && c.ParentSessionID != sessionID {
		parent = c.ParentSessionID
	}
	if err := s.lockSession(tx, sessionID, create); err != nil {
		return "", err
	}
	var dead bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_tombstones WHERE user_id=$1 AND agent=$2 AND session_id=$3 AND (scope='user' OR device_id=$4))`,
		s.src.userID, s.src.agent, sessionID, s.src.deviceID).Scan(&dead); err != nil {
		return "", err
	}
	if !dead && parent != "" {
		tag, err := tx.Exec(ctx, `INSERT INTO conversation_tombstones(id,user_id,device_id,agent,session_id,conversation_id,job_id,requested_by,requested_at,scope)
			SELECT $1,user_id,$2,agent,$3,$4,job_id,requested_by,now(),scope FROM conversation_tombstones WHERE user_id=$5 AND agent=$6 AND session_id=$7 AND (scope='user' OR device_id=$2)
			ORDER BY (scope='user') DESC LIMIT 1
			ON CONFLICT DO NOTHING`, uuid.NewString(), s.src.deviceID, sessionID, uuid.NewString(), s.src.userID, s.src.agent, parent)
		if err != nil {
			return "", err
		}
		dead = tag.RowsAffected() > 0
	}
	if dead {
		s.tombstoned = true
		s.convIDs[sessionID] = ""
		return "", nil
	}
	if !create {
		var id string
		err := tx.QueryRow(ctx, `SELECT id::text FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=$3`, s.src.deviceID, s.src.agent, sessionID).Scan(&id)
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return id, err
	}
	if c == nil {
		c = &transcript.Conversation{}
	}
	extra, err := json.Marshal(c.Extra)
	if err != nil || c.Extra == nil {
		extra = []byte("{}")
	}
	// A session the admin rules cover that is stored all the same (see
	// gate.check) is written hidden, as a rule change hides it.
	var hideAt *time.Time
	var hideRule, hideBy string
	var hideVersion int64
	var hideD pathpolicy.Decision
	if s.gate != nil {
		if d, ok := s.gate.hide[sessionID]; ok {
			at := time.Now().UTC().Truncate(time.Microsecond)
			hideAt, hideRule, hideBy, hideVersion, hideD = &at, ruleName(d), s.gate.rules.updatedBy, s.gate.rules.version, d
		}
	}
	var id string
	var newlyHidden bool
	branches := c.Branches
	if branches == nil {
		branches = []string{}
	}
	// The upsert writes the cold columns; conversations_skip_noop drops
	// it when they are as stored, and the row is then not returned (but
	// still locked), so the second branch returns the stored id: it reads
	// the snapshot from before the statement, which holds the stored row
	// when the upsert skipped it. The hot last activity goes to
	// conversation_activity with the digest (flush, refreshDigest).
	err = tx.QueryRow(ctx, `WITH up AS (INSERT INTO conversations(id,source_id,agent,session_id,device_id,user_id,cwd,title,started_at,
			parent_native_session_id,spawned_by_native_id,depth,extra,hidden_at,hidden_rule,hidden_rules_version,hidden_root,hidden_by,other_cwds,branches)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::timestamptz,$15,$16,CASE WHEN $14::timestamptz IS NULL THEN NULL ELSE $1::uuid END,NULLIF($17,'')::uuid,$18::text[],$19)
		ON CONFLICT (device_id,agent,session_id) DO UPDATE SET source_id=excluded.source_id,
			cwd=COALESCE(excluded.cwd,conversations.cwd),
			other_cwds=(SELECT COALESCE(array_agg(d ORDER BY o),'{}') FROM (SELECT d,min(o) o
				FROM unnest(conversations.other_cwds||excluded.other_cwds||ARRAY[conversations.cwd]) WITH ORDINALITY u(d,o)
				WHERE d IS NOT NULL AND d IS DISTINCT FROM COALESCE(excluded.cwd,conversations.cwd) GROUP BY d) x),
			title=COALESCE(excluded.title,conversations.title),
			started_at=COALESCE(excluded.started_at,conversations.started_at),
			parent_native_session_id=COALESCE(excluded.parent_native_session_id,conversations.parent_native_session_id),
			spawned_by_native_id=COALESCE(excluded.spawned_by_native_id,conversations.spawned_by_native_id),
			depth=GREATEST(excluded.depth,conversations.depth), extra=conversations.extra||excluded.extra,
			branches=CASE WHEN cardinality(excluded.branches)>0 THEN excluded.branches ELSE conversations.branches END,
			hidden_root=CASE WHEN conversations.hidden_at IS NULL AND excluded.hidden_at IS NOT NULL THEN conversations.id ELSE conversations.hidden_root END,
			hidden_rule=CASE WHEN conversations.hidden_at IS NULL THEN excluded.hidden_rule ELSE conversations.hidden_rule END,
			hidden_rules_version=CASE WHEN conversations.hidden_at IS NULL THEN excluded.hidden_rules_version ELSE conversations.hidden_rules_version END,
			hidden_by=CASE WHEN conversations.hidden_at IS NULL THEN excluded.hidden_by ELSE conversations.hidden_by END,
			hidden_at=COALESCE(conversations.hidden_at,excluded.hidden_at)
		RETURNING id, $14::timestamptz IS NOT NULL AND hidden_at=$14::timestamptz AS hid)
	SELECT id::text,hid FROM up UNION ALL
		SELECT id::text,false FROM conversations WHERE device_id=$5 AND agent=$3 AND session_id=$4 AND NOT EXISTS (SELECT 1 FROM up)`,
		uuid.NewString(), s.src.id, s.src.agent, sessionID, s.src.deviceID, s.src.userID, nullStr(c.Cwd), nullStr(clean(c.Title)),
		nullTime(c.StartedAt), nullStr(c.ParentSessionID), nullStr(c.SpawnedByToolCallID), c.Depth, extra,
		hideAt, nullStr(hideRule), hiddenVersion(hideAt, hideVersion), hideBy, otherCwds(c), branches).Scan(&id, &newlyHidden)
	if err != nil {
		return "", err
	}
	if newlyHidden {
		if err := store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), ActorID: hideBy, DeviceID: s.src.deviceID,
			Action: "conversation.hidden", TargetType: "conversation", TargetID: id,
			Metadata: map[string]any{"rule": hideRule, "mode": hideD.Mode.String(), "unplaceable": hideD.Unplaceable, "rules_version": hideVersion,
				"user_id": s.src.userID, "device_id": s.src.deviceID, "source_id": s.src.id, "at": "parse", "conversations": 1,
				"purge_after": hideAt.Add(HiddenPurgeAfter)},
			CreatedAt: *hideAt}); err != nil {
			return "", err
		}
	}
	s.convIDs[sessionID] = id
	return id, nil
}

// lockSession takes the session's natural-key lock (store
// ConversationLockKey), after its parent's when the pending record names
// one and create is set: a deletion locks the parent before its
// subagents. A session the flush locked already (held) is skipped.
func (s *sink) lockSession(tx pgx.Tx, sessionID string, create bool) error {
	if s.held[sessionID] {
		return nil
	}
	keys := []string{sessionID}
	if c := s.convs[sessionID]; c != nil && create && c.ParentSessionID != "" && c.ParentSessionID != sessionID {
		keys = []string{c.ParentSessionID, sessionID}
	}
	for _, k := range keys {
		if _, err := tx.Exec(s.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, store.ConversationLockKey(s.src.userID, s.src.agent, k)); err != nil {
			return err
		}
	}
	if s.held != nil {
		s.held[sessionID] = true
	}
	return nil
}

// lockFlushSQL locks the stored conversations of sessions $3 on device $1
// and agent $2, their stored parents, and the parents $4 the pending
// records name, in store.LockConversationsSQL's order, and returns whether
// each has a stored parent. NO KEY UPDATE is the lock the upsert and the
// parent count update take.
const lockFlushSQL = `SELECT session_id,parent_native_session_id IS NOT NULL FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=ANY($3::text[]||$4::text[]||ARRAY(
		SELECT parent_native_session_id FROM conversations WHERE device_id=$1 AND agent=$2 AND session_id=ANY($3::text[]) AND parent_native_session_id IS NOT NULL))
	ORDER BY session_id COLLATE "C",id FOR NO KEY UPDATE`

// otherCwds is the conversation's other directories, as stored.
func otherCwds(c *transcript.Conversation) []string {
	out := make([]string, 0, len(c.OtherCwds))
	for _, d := range c.OtherCwds {
		if d != "" && d != c.Cwd && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// hiddenVersion is the rules version a hide records, NULL for no hide.
func hiddenVersion(at *time.Time, v int64) any {
	if at == nil {
		return nil
	}
	return v
}

// row is the stored state of a message the batch may replace.
type row struct {
	id         string
	version    int
	superseded bool
	sha        []byte
	textMD5    string  // md5 of the stored text, hex
	text       *string // the stored text, loaded only for a growth check
	meta       rowMeta
}

// rowMeta is everything an in-place update writes besides the text and
// parse_attempt. A live row whose rowMeta and text match the message is
// left as it is.
type rowMeta struct {
	sourceID                                          *string
	generation, ordinal                               int64
	onPath, isError                                   *bool
	offset, lineNo, byteLen                           *int64
	parentNative, toolName, role, toolCallID, locator *string
	kind, parser                                      string
	rules                                             *string
	ts                                                *time.Time
	enrichment                                        []byte
	detached                                          bool // superseded_by or superseded_in_generation is set
}

func metaOf(m *transcript.Message, src source, enrichment []byte) rowMeta {
	rules := redact.RulesVersion
	var ts *time.Time
	if !m.TS.IsZero() {
		t := m.TS.Truncate(time.Microsecond) // timestamptz precision
		ts = &t
	}
	return rowMeta{sourceID: &src.id, generation: src.generation, ordinal: m.Ordinal, onPath: m.OnActivePath, isError: errPtr(m),
		offset: offPtr(m), lineNo: intPtr(m.LineNo), byteLen: intPtr(m.ByteLen), parentNative: strPtr(clean(m.ParentNativeID)),
		toolName: strPtr(clean(m.ToolName)), role: strPtr(clean(m.Role)), toolCallID: strPtr(clean(m.ToolCallID)), locator: strPtr(locator(m)),
		kind: m.Kind.String(), parser: m.Parser, rules: &rules, ts: ts, enrichment: enrichment}
}

// same reports whether an update to want would leave the row unchanged.
// A message without a timestamp keeps the stored one.
func (r rowMeta) same(want rowMeta) bool {
	return !r.detached && eqPtr(r.sourceID, want.sourceID) && r.generation == want.generation && r.ordinal == want.ordinal &&
		eqPtr(r.onPath, want.onPath) && eqPtr(r.isError, want.isError) && eqPtr(r.offset, want.offset) && eqPtr(r.lineNo, want.lineNo) &&
		eqPtr(r.byteLen, want.byteLen) && eqPtr(r.parentNative, want.parentNative) && eqPtr(r.toolName, want.toolName) &&
		eqPtr(r.role, want.role) && eqPtr(r.toolCallID, want.toolCallID) && eqPtr(r.locator, want.locator) && r.kind == want.kind &&
		r.parser == want.parser && eqPtr(r.rules, want.rules) && (want.ts == nil || r.ts != nil && r.ts.Equal(*want.ts)) &&
		sameJSON(r.enrichment, want.enrichment)
}

// updated is the stored meta after an update to want.
func (r rowMeta) updated(want rowMeta) rowMeta {
	if want.ts == nil {
		want.ts = r.ts
	}
	return want
}

func textMD5(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// messageKey is a row's identity: native id and part within the
// conversation, else locator and part within the source.
func messageKey(conv string, m *transcript.Message) string {
	if m.NativeID != "" {
		return "n|" + conv + "|" + m.NativeID + "|" + strconv.Itoa(m.Part)
	}
	return "l|" + locator(m) + "|" + strconv.Itoa(m.Part)
}

func locator(m *transcript.Message) string {
	if m.Locator != "" || m.NativeID != "" {
		return m.Locator
	}
	return "@" + strconv.FormatInt(m.ByteOffset, 10)
}

func (s *sink) writeMessages(tx pgx.Tx) error {
	ctx := s.ctx
	var convs, natives, locs []string
	var nparts, lparts []int32
	for _, m := range s.msgs {
		conv := s.convIDs[m.SessionID]
		if conv == "" {
			continue
		}
		if m.NativeID != "" {
			convs, natives, nparts = append(convs, conv), append(natives, clean(m.NativeID)), append(nparts, int32(m.Part))
		} else {
			locs, lparts = append(locs, locator(m)), append(lparts, int32(m.Part))
		}
	}
	have := map[string]*row{}
	// The stored text is compared by hash: reading it back would move every
	// row's text, most of it unchanged, to the client. Each key is looked up
	// on its own (LATERAL ... LIMIT 1, the live row or else the latest
	// version): a semi-join lets the planner walk a whole index instead.
	const cols = `id::text,version,superseded,content_sha,md5(text),on_active_path,source_id::text,source_generation,byte_offset,is_error,enrichment,
		ordinal,line_no,byte_len,parent_native_id,tool_name,role,tool_call_id,kind,parser,redaction_rules,ts,
		superseded_by IS NOT NULL OR superseded_in_generation IS NOT NULL`
	scan := func(rows pgx.Rows, key func(conv, native, loc string, part int) string) error {
		defer rows.Close()
		for rows.Next() {
			r := &row{}
			mt := &r.meta
			var conv, native, loc *string
			var part int
			if err := rows.Scan(&r.id, &r.version, &r.superseded, &r.sha, &r.textMD5, &mt.onPath, &mt.sourceID, &mt.generation, &mt.offset, &mt.isError, &mt.enrichment,
				&mt.ordinal, &mt.lineNo, &mt.byteLen, &mt.parentNative, &mt.toolName, &mt.role, &mt.toolCallID, &mt.kind, &mt.parser, &mt.rules, &mt.ts, &mt.detached,
				&conv, &native, &loc, &part); err != nil {
				return err
			}
			mt.locator = loc
			have[key(deref(conv), deref(native), deref(loc), part)] = r
		}
		return rows.Err()
	}
	if len(natives) > 0 {
		rows, err := tx.Query(ctx, `SELECT r.* FROM unnest($1::uuid[],$2::text[],$3::int[]) k(c,n,p)
			CROSS JOIN LATERAL (SELECT `+cols+`,conversation_id::text,native_id,locator,part FROM messages
				WHERE conversation_id=k.c AND native_id=k.n AND part=k.p ORDER BY superseded,version DESC LIMIT 1) r`, convs, natives, nparts)
		if err != nil {
			return err
		}
		if err := scan(rows, func(conv, native, _ string, part int) string {
			return "n|" + conv + "|" + native + "|" + strconv.Itoa(part)
		}); err != nil {
			return err
		}
	}
	if len(locs) > 0 {
		rows, err := tx.Query(ctx, `SELECT r.* FROM unnest($2::text[],$3::int[]) k(l,p)
			CROSS JOIN LATERAL (SELECT `+cols+`,conversation_id::text,native_id,locator,part FROM messages
				WHERE source_id=$1 AND native_id IS NULL AND locator=k.l AND part=k.p ORDER BY superseded,version DESC LIMIT 1) r`, s.src.id, locs, lparts)
		if err != nil {
			return err
		}
		if err := scan(rows, func(_, _, loc string, part int) string { return "l|" + loc + "|" + strconv.Itoa(part) }); err != nil {
			return err
		}
	}

	// A growth check needs the stored text of the live rows whose content
	// changed; only those are read.
	var grown []string
	for _, m := range s.msgs {
		if conv := s.convIDs[m.SessionID]; conv != "" {
			if old := have[messageKey(conv, m)]; old != nil && !old.superseded && old.text == nil && !bytes.Equal(old.sha, m.ContentSHA[:]) {
				grown = append(grown, old.id)
			}
		}
	}
	if len(grown) > 0 {
		rows, err := tx.Query(ctx, `SELECT id::text,text FROM messages WHERE id=ANY($1::uuid[])`, grown)
		if err != nil {
			return err
		}
		texts := map[string]*string{}
		for rows.Next() {
			var id, text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return err
			}
			texts[id] = &text
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range have {
			if t := texts[r.id]; t != nil {
				r.text = t
			}
		}
	}

	b := &pgx.Batch{}
	for _, m := range s.msgs {
		conv := s.convIDs[m.SessionID]
		if conv == "" {
			continue
		}
		key := messageKey(conv, m)
		search := clean(m.Text)
		enrichment := enrichmentJSON(m.Enrichment)
		want := metaOf(m, s.src, enrichment)
		old := have[key]
		switch {
		case old == nil:
			have[key] = s.insert(b, conv, m, search, want, 1)
		case bytes.Equal(old.sha, m.ContentSHA[:]):
			sum := textMD5(search)
			if !old.superseded && old.textMD5 == sum && old.meta.same(want) {
				// Unchanged: not rewritten, so it keeps its parse_attempt.
				if s.repeated == nil {
					s.repeated = map[*transcript.Message]bool{}
				}
				s.repeated[m] = true
				if s.kept == nil {
					s.kept = map[uuid.UUID]struct{}{}
				}
				s.kept[uuid.MustParse(old.id)] = struct{}{}
			} else {
				s.markReplaced(conv)
				s.update(b, old.id, m, search, enrichment, old.textMD5 != sum)
				old.superseded, old.textMD5, old.text, old.meta = false, sum, &search, old.meta.updated(want)
			}
		case !old.superseded && old.text != nil && len(search) > len(*old.text) && strings.HasPrefix(search, *old.text):
			// Growth of the same record (streaming, a result filled in):
			// replace in place.
			s.markReplaced(conv)
			s.update(b, old.id, m, search, enrichment, true)
			old.sha, old.textMD5, old.text, old.meta = m.ContentSHA[:], textMD5(search), &search, old.meta.updated(want)
		default:
			// A different version: the old row stays, superseded.
			s.markReplaced(conv)
			have[key] = s.insert(b, conv, m, search, want, old.version+1)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return tx.SendBatch(ctx, b).Close()
}

func (s *sink) markReplaced(conv string) {
	if s.replaced == nil {
		s.replaced = map[string]bool{}
	}
	s.replaced[conv] = true
}

// insert queues a new live row; an existing live row of the same key is
// superseded first (the unique live index allows one), linked to the new
// row in the same update: one row version, not two. superseded_by is a
// deferred foreign key, so it may name the row inserted after it.
func (s *sink) insert(b *pgx.Batch, conv string, m *transcript.Message, search string, meta rowMeta, version int) *row {
	enrichment := meta.enrichment
	id := uuid.NewString()
	if version > 1 {
		b.Queue(`UPDATE messages SET superseded=true,superseded_in_generation=$3,superseded_by=$7 WHERE conversation_id=$1 AND native_id IS NOT DISTINCT FROM $2 AND NOT superseded AND part=$4 AND ($2::text IS NOT NULL OR (source_id=$5 AND locator=$6))`,
			conv, nullStr(clean(m.NativeID)), s.src.generation, m.Part, s.src.id, locator(m), id)
	}
	r := &row{id: id, version: version, sha: m.ContentSHA[:], textMD5: textMD5(search), text: &search, meta: meta}
	b.Queue(`INSERT INTO messages(id,conversation_id,source_id,native_id,parent_native_id,part,ordinal,kind,role,tool_name,tool_call_id,is_error,ts,
			text,text_len,content_sha,version,on_active_path,enrichment,source_generation,line_no,byte_offset,byte_len,locator,parser,parse_attempt,redaction_rules)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		r.id, conv, s.src.id, nullStr(clean(m.NativeID)), nullStr(clean(m.ParentNativeID)), m.Part, m.Ordinal, m.Kind.String(), nullStr(clean(m.Role)),
		nullStr(clean(m.ToolName)), nullStr(clean(m.ToolCallID)), errPtr(m), nullTime(m.TS), search, m.FullLen,
		m.ContentSHA[:], version, m.OnActivePath, enrichment, s.src.generation, nullInt(m.LineNo), offPtr(m), nullInt(m.ByteLen), nullStr(locator(m)), m.Parser, s.src.parseAttempt, redact.RulesVersion)
	return r
}

// update queues an in-place update: metadata always, text when withText.
func (s *sink) update(b *pgx.Batch, id string, m *transcript.Message, search string, enrichment []byte, withText bool) {
	// first_seen_at is when the server first stored the record's raw
	// bytes (a redaction's first-uploader rule). New text from the same
	// byte range of the same source generation (text filled in from
	// context, such as a persisted tool output that arrived later) of a row
	// keyed by its native id keeps it. The bytes themselves are not pinned:
	// a whole provisional tail replaces the tail's bytes in place in the
	// same generation. The native id is what holds the row to its record:
	// it comes from the record's bytes, so planting the row before the
	// record's first upload needs that id. A row keyed by its offset (a
	// Codex item without an id) could be planted at any offset and switched
	// to copied bytes, so new text resets it, as it does for other bytes or
	// a row without a byte range (a Devin row). The SET expressions read the
	// row before the update.
	b.Queue(`UPDATE messages SET superseded=false,superseded_by=NULL,superseded_in_generation=NULL,source_id=$2,source_generation=$3,
			on_active_path=$4,is_error=$5,enrichment=$6,line_no=$7,byte_offset=$8,byte_len=$9,parent_native_id=$10,tool_name=$11,ts=COALESCE($12,ts),parse_attempt=$13,parser=$14,redaction_rules=$15,kind=$16,role=$17,ordinal=$18,tool_call_id=$19,locator=$20,
			first_seen_at=CASE WHEN $21::bool AND NOT COALESCE(native_id IS NOT NULL AND byte_offset=$8 AND byte_len=$9 AND source_id=$2::uuid AND source_generation=$3,false)
				THEN now() ELSE first_seen_at END
		WHERE id=$1`, id, s.src.id, s.src.generation, m.OnActivePath, errPtr(m), enrichment, nullInt(m.LineNo), offPtr(m), nullInt(m.ByteLen),
		nullStr(clean(m.ParentNativeID)), nullStr(clean(m.ToolName)), nullTime(m.TS), s.src.parseAttempt, m.Parser, redact.RulesVersion, m.Kind.String(), nullStr(clean(m.Role)), m.Ordinal, nullStr(clean(m.ToolCallID)), nullStr(locator(m)), withText)
	if withText {
		b.Queue(`UPDATE messages SET text=$2,text_len=$3,content_sha=$4 WHERE id=$1`, id, search, m.FullLen, m.ContentSHA[:])
	}
}

// clean makes a string storable as Postgres text: no NUL bytes, valid UTF-8.
func clean(s string) string {
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return strings.ToValidUTF8(s, "�")
}

func enrichmentJSON(m map[string]any) []byte {
	if len(m) == 0 {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return bytes.ReplaceAll(b, []byte(`\u0000`), nil)
}

func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func errPtr(m *transcript.Message) *bool {
	if m.Kind != transcript.KindToolResult && !m.IsError {
		return nil
	}
	return &m.IsError
}

func offPtr(m *transcript.Message) *int64 {
	if m.LineNo == 0 && m.ByteOffset == 0 && m.ByteLen == 0 {
		return nil
	}
	return &m.ByteOffset
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func intPtr(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func eqPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
