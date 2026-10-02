package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uncapped makes the parsers store every message's text whole (D3): the
// server indexes all of it, and an empty (non-nil) cap map means no cap
// for any kind.
var uncapped = map[transcript.Kind]transcript.CapConfig{}

// hadStoredSQL reports whether any conversation names source $1.
const hadStoredSQL = `SELECT EXISTS(SELECT 1 FROM conversations WHERE source_id=$1)`

// retireCandidatesSQL lists the live rows of source $1 that a full parse
// at generation $2 with attempt $3 did not write: an older generation, or
// an older attempt. Those the parse found unchanged (job.kept) stay; the
// rest are absent from the replacement and retireIDsSQL supersedes them,
// returning their conversations. retirePreviousSQL supersedes every live
// row of a previous source $1.
const (
	retireCandidatesSQL = `SELECT id FROM messages WHERE source_id=$1 AND NOT superseded AND (source_generation<$2 OR parse_attempt<$3)`
	retireIDsSQL        = `WITH retired AS (UPDATE messages SET superseded=true,superseded_in_generation=$2
		WHERE id=ANY($1::uuid[]) AND NOT superseded RETURNING conversation_id)
		SELECT DISTINCT conversation_id::text FROM retired`
	retirePreviousSQL = `WITH retired AS (UPDATE messages SET superseded=true,superseded_in_generation=$2
		WHERE source_id=$1 AND NOT superseded RETURNING conversation_id)
		SELECT DISTINCT conversation_id::text FROM retired`
)

// job is one source's parse: its identity, stored cursor, and request.
type job struct {
	src                         source
	path, fileID, kind, parse   string
	previous                    *string
	parentID                    *string
	tombstoned                  bool
	cursor                      transcript.Cursor
	cursorGen                   int64
	seq                         int64
	reparse                     bool
	dev                         deviceDirs
	extraction                  *transcript.ExtractionCheckpoint
	appliedParser, appliedRules *string
	derivedParser               string
	// kept is the stored rows this parse found unchanged and left with an
	// older parse_attempt; recount is the conversations whose digests wait
	// for a recount (sink.kept, sink.dirty).
	kept    map[uuid.UUID]struct{}
	recount []string
	// touched is the conversations the parse wrote to.
	touched []string
}

// ParseSource brings one source's message rows up to date with its latest
// generation. Evidence is already durable; a failure leaves the request
// pending for a retry, never a re-upload.
func (q *Queue) parseSource(ctx context.Context, sourceID string) (err error) {
	j := &job{}
	var st, report []byte
	err = q.Pool.QueryRow(ctx, `SELECT s.id::text,s.device_id::text,d.user_id::text,s.agent,s.path,s.file_id,s.storage_kind,s.parser,
			s.previous_source_id::text,s.parent_source_id::text,s.tombstoned_at IS NOT NULL,
			p.generation,p.cursor_offset,p.cursor_line,p.cursor_state,p.requested_seq,p.reparse,COALESCE(d.home,''),COALESCE(d.claude_projects,''),p.extraction_report,p.applied_parser,p.applied_redaction_rules
		FROM sources s JOIN devices d ON d.id=s.device_id JOIN source_parse_state p ON p.source_id=s.id WHERE s.id=$1`, sourceID).
		Scan(&j.src.id, &j.src.deviceID, &j.src.userID, &j.src.agent, &j.path, &j.fileID, &j.kind, &j.parse, &j.previous, &j.parentID, &j.tombstoned,
			&j.cursorGen, &j.cursor.Offset, &j.cursor.LineNo, &st, &j.seq, &j.reparse, &j.dev.home, &j.dev.claudeProjects, &report, &j.appliedParser, &j.appliedRules)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted meanwhile
	}
	if err != nil {
		return err
	}
	j.cursor.State = st
	if report != nil {
		j.extraction = &transcript.ExtractionCheckpoint{}
		if err := json.Unmarshal(report, j.extraction); err != nil {
			return err
		}
	}
	if j.extraction != nil {
		j.extraction.Contract = transcript.ReparseKey(j.extraction.Contract)
	}
	if j.tombstoned {
		return q.done(ctx, j, j.cursorGen)
	}
	// Repair only work recorded by future uploads, before consuming their
	// bytes or acknowledging the source's durable parse request.
	if err := q.repairUploadedArchive(ctx, sourceID); err != nil {
		return err
	}
	// A companion or subagent file of a transcript the admin rules
	// refused goes with it.
	if rule, err := parentRefused(ctx, q.Pool, j.parentID); err != nil {
		return err
	} else if rule != "" {
		if err := refuseSource(ctx, q.Pool, j.src, j.path, nil, rule, map[string]any{"parent_source_id": *j.parentID}); err != nil {
			return err
		}
		return q.done(ctx, j, j.cursorGen)
	}
	if j.kind == string(transcript.StorageCompanion) {
		if err := q.companionChanged(ctx, j); err != nil {
			return err
		}
		return q.done(ctx, j, j.cursorGen)
	}
	// An append is read from its cursor: the manifest before it is loaded
	// only if the parse reaches back (a new generation, a full parse). The
	// byte before the cursor is in the window, since the redaction pass
	// looks back for the start of the line.
	g, err := LoadGenerationFrom(ctx, q.Pool, sourceID, -1, j.cursor.Offset-1)
	if errors.Is(err, ErrNoGeneration) {
		return q.done(ctx, j, j.cursorGen)
	}
	if err != nil {
		return err
	}
	j.src.generation = g.Generation
	// Each extraction gets a fresh witness, including retries of one request.
	// A previous binary may have committed rows before failing its checkpoint.
	if err := q.Pool.QueryRow(ctx, "SELECT nextval('extraction_attempt_seq')").Scan(&j.src.parseAttempt); err != nil {
		return err
	}
	isDevin := j.src.agent == string(transcript.AgentDevin)
	contract := serverExtractionContract(j.src.agent)
	version := serverParserVersion(j.src.agent)
	versionChanged := j.appliedParser == nil || transcript.ReparseKey(*j.appliedParser) != transcript.ReparseKey(version) || j.appliedRules == nil || *j.appliedRules != redact.RulesVersion
	full := g.Generation != j.cursorGen || j.reparse || versionChanged || (contract != "" && (j.extraction == nil || j.extraction.Contract != contract || j.extraction.Generation != g.Generation || j.extraction.Offset != j.cursor.Offset || j.extraction.LineNo != j.cursor.LineNo || j.extraction.Report.Validate() != nil))
	if full {
		j.cursor = transcript.Cursor{} // a new generation is parsed whole; Devin's cursor spans exports
	}
	rules, err := loadRules(ctx, q.Pool)
	if err != nil {
		return err
	}
	// A source with stored conversations has its covered sessions hidden,
	// not refused: they may be restored.
	var hadStored bool
	if err := q.Pool.QueryRow(ctx, hadStoredSQL, j.src.id).Scan(&hadStored); err != nil {
		return err
	}
	sink := newSink(ctx, q.Pool, j.src)
	// A parse that fails after replacing rows recounts their digests here:
	// the rows it committed stay visible until the retry.
	defer func() {
		if err != nil && len(sink.dirty) > 0 {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if rerr := pgx.BeginFunc(rctx, q.Pool, func(tx pgx.Tx) error { return recountDigests(rctx, tx, sink.dirtyConversations()) }); rerr != nil {
				q.Log.Warn("ingest: recounting digests after a failed parse", "source", sourceID, "error", rerr)
			}
		}
	}()
	if !rules.empty() {
		sink.gate = newGate(rules, j.src, j.path, j.dev, q.Pool, hadStored)
	}
	r := NewReader(ctx, q.Objects, g)
	// The server's own redaction pass (notes/redaction.md): bytes a device
	// already redacted pass unchanged; anything that arrived unredacted is
	// masked before it becomes a row. Offsets are unchanged.
	// A Devin export is read whole every time, so its count is replaced.
	rr := redact.NewReaderAt(r, redact.ModeFor(j.kind, j.path))
	countAll := full || isDevin
	masks, maskRevision, err := q.masks.lineMasks(ctx, q.Pool)
	if err != nil {
		return err
	}
	rr.SetLineMasks(masks)
	sink.maskRevision = maskRevision
	if afterLineMasks != nil {
		afterLineMasks()
	}
	if countAll {
		rr.CountFrom(0)
	} else {
		rr.CountFrom(j.cursor.Offset)
	}
	in := transcript.Input{
		Source: &transcript.Source{Agent: transcript.Agent(j.src.agent), Path: j.path, StorageKind: transcript.StorageKind(j.kind), Parser: j.parse},
		R:      rr,
		Size:   r.Size(),
	}
	originalOffset := j.cursor.Offset
	var next transcript.Cursor
	var result transcript.ParseResult
	switch transcript.Agent(j.src.agent) {
	case transcript.AgentClaude:
		p := &claude.Parser{FS: &archiveFS{ctx: ctx, pool: q.Pool, objects: q.Objects, deviceID: j.src.deviceID, masks: &q.masks}, Caps: uncapped}
		result, err = p.ParseWithReport(ctx, in, j.cursor, sink)
		next = result.Cursor
	case transcript.AgentCodex:
		archive := &archiveFS{ctx: ctx, pool: q.Pool, objects: q.Objects, deviceID: j.src.deviceID, masks: &q.masks}
		result, err = (&codex.Parser{Caps: uncapped, OpenRollout: archive.openRollout}).ParseWithReport(ctx, in, j.cursor, sink)
		next = result.Cursor
	case transcript.AgentDevin:
		next, err = parseDevinExport(ctx, in, j, sink)
	default:
		return q.done(ctx, j, g.Generation) // archived, no parser
	}
	if err == nil {
		err = sink.flush()
	}
	if sink.masksMoved {
		// However the parser reported the failed write: start again with
		// the new catalog.
		return errMasksMoved
	}
	j.kept, j.recount = sink.kept, sink.dirtyConversations()
	for _, id := range sink.convIDs {
		if id != "" {
			j.touched = append(j.touched, id)
		}
	}
	if ref := (*refusal)(nil); errors.As(err, &ref) {
		if err := refuseSource(ctx, q.Pool, j.src, j.path, sink.gate.sessions(), ruleName(ref.d), ref.detail()); err != nil {
			return err
		}
		return q.done(ctx, j, g.Generation)
	}
	if err != nil {
		return fmt.Errorf("parse %s generation %d: %w", j.path, g.Generation, err)
	}
	if contract != "" {
		j.extraction, err = transcript.FinalizeExtraction(j.extraction, g.Generation, contract, result)
		if err != nil {
			return err
		}
	}
	if j.appliedRules == nil || *j.appliedRules != redact.RulesVersion {
		if err := q.maskStoredVersions(ctx, j.src.id); err != nil {
			return err
		}
	}
	j.derivedParser = version
	if err := q.finish(ctx, j, sink); err != nil {
		return err
	}
	if err := q.recordRedactions(ctx, j.src.id, countAll, rr.Counts()); err != nil {
		return err
	}
	// Rules changed during the parse: what it stored is checked against
	// the new ones (the stored-session sweep may have run before it
	// committed). A new upload is refused; a source that had stored rows
	// has its covered conversations hidden, as the sweep hides them.
	if now, err := loadRules(ctx, q.Pool); err != nil {
		return err
	} else if now.version != rules.version {
		if hadStored {
			if _, _, err := q.applyRules(ctx, now, j.src.id); err != nil {
				return err
			}
		} else if !now.empty() {
			ref, sessions, err := recheckStored(ctx, q.Pool, now, j.src, j.path, j.dev)
			if err != nil {
				return err
			}
			if ref != nil {
				if err := refuseSource(ctx, q.Pool, j.src, j.path, sessions, ruleName(ref.d), ref.detail()); err != nil {
					return err
				}
				return q.done(ctx, j, g.Generation)
			}
		}
	}
	j.cursor = next
	return q.complete(ctx, j, g.Generation, full || (!isDevin && result.Report != nil && result.FromOffset == 0 && originalOffset > 0))
}

// parseDevinExport rebuilds the session's store from the export in a
// temporary directory and runs the Devin parser over it.
func parseDevinExport(ctx context.Context, in transcript.Input, j *job, sink *sink) (transcript.Cursor, error) {
	dir, err := os.MkdirTemp("", "flopwire-devin-")
	if err != nil {
		return j.cursor, err
	}
	defer os.RemoveAll(dir)
	session := devin.SessionOfExport(j.path)
	if session == "" {
		session = j.path
	}
	db, err := devin.LoadExport(ctx, io.NewSectionReader(in.R, 0, in.Size), session, dir)
	if err != nil {
		return j.cursor, err
	}
	in.Source.Path = db
	return (&devin.Parser{Caps: uncapped}).Parse(ctx, in, j.cursor, sink)
}

// finish applies generation-level supersession, the tombstone, and
// subagent links after a parse's rows are written.
func (q *Queue) finish(ctx context.Context, j *job, sink *sink) error {
	if sink.tombstoned {
		return tombstoneSource(ctx, q.Pool, j.src.id)
	}
	// D20: a newer source already replaced this one (it named this one as
	// its previous before this one arrived): this one's rows are history.
	// A source this one names as its own previous is older, not newer: a
	// path whose file identity came back (inode reuse) links both ways.
	var gen int64
	err := q.Pool.QueryRow(ctx, `SELECT COALESCE(max(g.generation),0) FROM sources s JOIN generations g ON g.source_id=s.id
			WHERE s.previous_source_id=$1 AND s.tombstoned_at IS NULL
			  AND s.id IS DISTINCT FROM (SELECT previous_source_id FROM sources WHERE id=$1) HAVING count(*)>0`, j.src.id).Scan(&gen)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return err
	default:
		// The flushes counted the rows this retires: the recount runs in
		// the same transaction, so a failure between the two cannot leave
		// digests counting retired rows (a retry would retire nothing and
		// not recount). The conversations are locked first, in the order
		// a flush upserts them (store.LockConversationsSQL's order, taken
		// here over a subquery), before any message row.
		if err := pgx.BeginFunc(ctx, q.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id::text FROM conversations WHERE id IN
				(SELECT conversation_id FROM messages WHERE source_id=$1 AND NOT superseded)
				ORDER BY session_id COLLATE "C",id FOR UPDATE`, j.src.id)
			if err != nil {
				return err
			}
			retired, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE messages SET superseded=true,superseded_in_generation=$2 WHERE source_id=$1 AND NOT superseded`, j.src.id, gen); err != nil {
				return err
			}
			if afterLateSupersede != nil {
				if err := afterLateSupersede(); err != nil {
					return err
				}
			}
			return recountDigests(ctx, tx, retired)
		}); err != nil {
			return err
		}
	}
	var touched []string
	for _, id := range sink.convIDs {
		if id != "" {
			touched = append(touched, id)
		}
	}
	slices.Sort(touched)
	return resolveLinks(ctx, q.Pool, j.src.deviceID, touched)
}

// afterLateSupersede, when set (tests), runs in finish between retiring
// a source a newer one replaced and recounting its digests.
var afterLateSupersede func() error

// recordRedactions stores what the server's pass masked in the source's
// latest generation: replaced on a full parse, added to on an append.
func (q *Queue) recordRedactions(ctx context.Context, sourceID string, full bool, counts map[string]int64) error {
	if !full && len(counts) == 0 {
		return nil
	}
	if counts == nil {
		counts = map[string]int64{}
	}
	_, err := q.Pool.Exec(ctx, `UPDATE source_parse_state SET server_redactions=
		(SELECT COALESCE(jsonb_object_agg(k, v),'{}') FROM (
			SELECT k, sum(v)::bigint AS v FROM (
				SELECT key AS k, value::bigint AS v FROM jsonb_each_text(CASE WHEN $2 THEN '{}'::jsonb ELSE server_redactions END)
				UNION ALL SELECT key, value::bigint FROM jsonb_each_text($3::jsonb)) x GROUP BY k) y)
		WHERE source_id=$1`, sourceID, full, counts)
	return err
}

// done saves the cursor and clears the request unless a newer one arrived.
func (q *Queue) done(ctx context.Context, j *job, gen int64) error {
	return q.complete(ctx, j, gen, false)
}

func (q *Queue) complete(ctx context.Context, j *job, gen int64, full bool) error {
	var report []byte
	if j.extraction != nil {
		var err error
		report, err = json.Marshal(j.extraction)
		if err != nil {
			return err
		}
	}
	return pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := checkpointDigests(ctx, tx, j, gen, full); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE source_parse_state SET generation=$2,cursor_offset=$3,cursor_line=$4,cursor_state=$5,
			parsed_seq=$6,reparse=reparse AND requested_seq<>$6,attempts=0,next_attempt_at=NULL,last_error='',parsed_at=now(),extraction_report=$7,
			applied_parser=COALESCE(NULLIF($8,''),applied_parser),
			applied_redaction_rules=CASE WHEN $8<>'' THEN $9 ELSE applied_redaction_rules END,
			refresh_requested_at=CASE WHEN $8<>'' THEN NULL ELSE refresh_requested_at END
		WHERE source_id=$1`, j.src.id, gen, j.cursor.Offset, j.cursor.LineNo, j.cursor.State, j.seq, report, j.derivedParser, redact.RulesVersion)
		return err
	})
}

// lockCheckpointSQL locks, in the order a flush upserts them, the
// conversations a checkpoint writes: $1, and those holding the rows $2 or
// the live rows of source $3 that it retires.
const lockCheckpointSQL = `SELECT 1 FROM conversations WHERE id IN (SELECT unnest($1::uuid[])
	UNION SELECT conversation_id FROM messages WHERE id=ANY($2::uuid[])
	UNION SELECT conversation_id FROM messages WHERE source_id=$3 AND NOT superseded)
	ORDER BY session_id COLLATE "C",id FOR UPDATE`

// checkpointDigests retires, on a full parse, the rows absent from the
// replacement and a previous source's live rows, then recounts the digests
// of the conversations that changed.
//
// A flush locks its conversations before their message rows, so the
// conversations are locked here before any row is retired: retiring first
// would hold message rows a concurrent flush (another source writing the
// same session) waits for, while the recount waits for that flush's
// conversation.
func checkpointDigests(ctx context.Context, tx pgx.Tx, j *job, gen int64, full bool) error {
	changed := map[string]bool{}
	for _, id := range j.recount {
		changed[id] = true
	}
	// A parse that died after replacing rows left their digests
	// stale; this one may have found the rows unchanged.
	stale, err := tx.Query(ctx, `SELECT c.id::text FROM conversations c JOIN conversation_activity a ON a.conversation_id=c.id
		WHERE a.digest_stale AND (c.id=ANY($1::uuid[]) OR c.source_id=$2)`, j.touched, j.src.id)
	if err != nil {
		return err
	}
	staleIDs, err := pgx.CollectRows(stale, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range staleIDs {
		changed[id] = true
	}
	if full {
		retire := func(sql string, args ...any) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				changed[id] = true
			}
			return rows.Err()
		}
		// Rows this parse neither wrote (parse_attempt) nor found
		// unchanged (kept) are absent from the replacement.
		rows, err := tx.Query(ctx, retireCandidatesSQL, j.src.id, gen, j.src.parseAttempt)
		if err != nil {
			return err
		}
		candidates, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		absent := slices.DeleteFunc(candidates, func(id uuid.UUID) bool { _, ok := j.kept[id]; return ok })
		if len(absent) > 0 || j.previous != nil {
			ids := make([]string, 0, len(changed))
			for id := range changed {
				ids = append(ids, id)
			}
			if _, err := tx.Exec(ctx, lockCheckpointSQL, ids, absent, j.previous); err != nil {
				return err
			}
		}
		if len(absent) > 0 {
			if err := retire(retireIDsSQL, absent, gen); err != nil {
				return err
			}
		}
		if j.previous != nil {
			if err := retire(retirePreviousSQL, *j.previous, gen); err != nil {
				return err
			}
		}
	}
	// Recount after retirement, including conversations wholly absent
	// from the replacement, and those whose rows the parse replaced.
	// Checkpoint failure rolls this back too.
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return recountDigests(ctx, tx, ids)
}

// companionChanged re-parses what a companion file feeds: a subagent whose
// meta.json arrived, or the transcripts whose <persisted-output> preview
// names a tool-results file that was missing when they were parsed.
func (q *Queue) companionChanged(ctx context.Context, j *job) error {
	var targets []string
	var reparse bool
	switch dir := path.Dir(j.path); {
	case strings.HasSuffix(j.path, ".meta.json"):
		// The sidecar describes the subagent transcript beside it. Find that
		// by path, not by the parent the device sent: a device that listed
		// the sidecar before the transcript existed names the session's main
		// transcript. The parser re-reads the sidecar on every call, so a
		// parse from the saved cursor applies it.
		var sub string
		err := q.Pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2 AND storage_kind<>'companion' AND tombstoned_at IS NULL
			ORDER BY first_seen_at DESC LIMIT 1`, j.src.deviceID, strings.TrimSuffix(j.path, ".meta.json")+".jsonl").Scan(&sub)
		if errors.Is(err, pgx.ErrNoRows) {
			break // not uploaded yet; its parse reads the sidecar from the archive
		}
		if err != nil {
			return err
		}
		targets = []string{sub}
		if j.parentID == nil || *j.parentID != sub {
			if _, err := q.Pool.Exec(ctx, `UPDATE sources SET parent_source_id=$2 WHERE id=$1`, j.src.id, sub); err != nil {
				return err
			}
		}
	case path.Base(dir) == "tool-results":
		session := path.Dir(dir)
		rows, err := q.Pool.Query(ctx, `SELECT DISTINCT m.source_id::text FROM messages m JOIN sources s ON s.id=m.source_id
			WHERE s.device_id=$1 AND (s.path=$2 OR s.path LIKE $3) AND NOT m.superseded AND m.enrichment->>'persisted_output_missing'=$4`,
			j.src.deviceID, session+".jsonl", likePrefix(session+"/subagents/"), "tool-results/"+path.Base(j.path))
		if err != nil {
			return err
		}
		if targets, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		reparse = true
	}
	for _, id := range targets {
		if err := pgx.BeginTxFunc(ctx, q.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error { return requestParse(ctx, tx, id, reparse) }); err != nil {
			return err
		}
		q.Notify(id)
	}
	return nil
}

// resolveLinks fills subagent links whose other end has arrived (spec
// §4.3), for the conversations a parse touched and the children waiting on
// them: parent conversation by native session id, then the spawning tool
// call, falling back for Claude to the parent's tool_result that names the
// child's agent or workflow run.
func resolveLinks(ctx context.Context, pool *pgxpool.Pool, deviceID string, touched []string) error {
	if len(touched) == 0 {
		return nil
	}
	for _, q := range []string{
		`UPDATE conversations c SET parent_conversation_id=p.id FROM conversations p
		 WHERE c.device_id=$1 AND c.parent_conversation_id IS NULL AND c.parent_native_session_id IS NOT NULL
		   AND p.device_id=c.device_id AND p.agent=c.agent AND p.session_id=c.parent_native_session_id AND p.id<>c.id
		   AND (c.id=ANY($2::uuid[]) OR p.id=ANY($2::uuid[]))`,
		`UPDATE conversations c SET spawned_by_native_id=r.tool_call_id FROM messages r
		 WHERE c.device_id=$1 AND c.agent='claude' AND c.spawned_by_native_id IS NULL AND c.parent_conversation_id IS NOT NULL
		   AND (c.id=ANY($2::uuid[]) OR c.parent_conversation_id=ANY($2::uuid[]))
		   AND r.conversation_id=c.parent_conversation_id AND NOT r.superseded AND r.kind='tool_result' AND r.tool_call_id IS NOT NULL
		   AND (r.enrichment->>'agent_id'=c.extra->>'agent_id' OR r.enrichment->>'workflow_run_id'=c.extra->>'workflow_run_id')`,
		`UPDATE conversations c SET spawned_by_message_id=m.id FROM messages m
		 WHERE c.device_id=$1 AND c.spawned_by_message_id IS NULL AND c.spawned_by_native_id IS NOT NULL AND c.parent_conversation_id IS NOT NULL
		   AND (c.id=ANY($2::uuid[]) OR c.parent_conversation_id=ANY($2::uuid[]))
		   AND m.conversation_id=c.parent_conversation_id AND NOT m.superseded AND m.kind='tool_call'
		   AND (m.tool_call_id=c.spawned_by_native_id OR m.native_id=c.spawned_by_native_id)`,
	} {
		if _, err := pool.Exec(ctx, q, deviceID, touched); err != nil {
			return err
		}
	}
	return nil
}

// tombstoneSource drops the raw evidence of a source whose conversation was
// deleted and marks it, so later uploads are acknowledged and discarded.
// Its companions go with it. Chunks left without a manifest reference are
// handed to the orphan reconciler. Like a deletion purge it holds the
// purge lock exclusively, so no backup is copying those chunks, and it is
// audited.
// errPurgeBusy is a parse that needs the purge lock while another holder
// has it. It is not a failure: the source is requeued without an attempt.
var errPurgeBusy = errors.New("ingest: purge lock busy")

func tombstoneSource(ctx context.Context, pool *pgxpool.Pool, sourceID string) error {
	return pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Try, not wait: a deletion job or backup can hold the purge lock
		// for minutes, and a parse that waited would time out and count
		// toward quarantine (V4). errPurgeBusy requeues it instead.
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, store.PurgeLockID).Scan(&got); err != nil {
			return err
		}
		if !got {
			return errPurgeBusy
		}
		ids, released, err := purgeSourceTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		return store.InsertAudit(ctx, tx, domain.AuditEvent{ID: uuid.NewString(), Action: "source.tombstoned", TargetType: "source", TargetID: sourceID,
			Metadata: map[string]any{"reason": "session_deleted", "sources": len(ids), "chunks_released": released}, CreatedAt: time.Now().UTC()})
	})
}

// purgeSourceTx tombstones a source and its companion and subagent files
// and drops their raw evidence: generations (manifests and tails cascade),
// and chunks left without a manifest reference, which go to the orphan
// reconciler. The caller holds the purge lock. It returns the sources and
// how many chunks it released.
func purgeSourceTx(ctx context.Context, tx pgx.Tx, sourceID string) ([]string, int64, error) {
	rows, err := tx.Query(ctx, `UPDATE sources SET tombstoned_at=COALESCE(tombstoned_at,now()) WHERE id=$1 OR parent_source_id=$1 RETURNING id::text`, sourceID)
	if err != nil {
		return nil, 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, 0, err
	}
	var hashes [][]byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT chunk_hash),'{}') FROM manifest_entries WHERE source_id=ANY($1::uuid[])`, ids).Scan(&hashes); err != nil {
		return nil, 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM generations WHERE source_id=ANY($1::uuid[])`, ids); err != nil {
		return nil, 0, err
	}
	// Lock, then test for references in a later statement (see
	// store.ProcessDeletionJobs).
	if _, err := tx.Exec(ctx, `SELECT 1 FROM chunks WHERE hash=ANY($1) AND state='committed' ORDER BY hash FOR UPDATE`, hashes); err != nil {
		return nil, 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE chunks SET state='cleanup_pending',cleanup_after=now(),updated_at=now()
		WHERE hash=ANY($1) AND state='committed' AND NOT EXISTS (SELECT 1 FROM manifest_entries m WHERE m.chunk_hash=chunks.hash)`, hashes)
	if err != nil {
		return nil, 0, err
	}
	return ids, tag.RowsAffected(), nil
}

func likePrefix(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// archiveFS serves a device's companion files (meta.json sidecars and
// tool-results/ outputs) to the Claude parser out of the archive, at their
// device paths. Transcripts are not served: the spawned-by fallback that
// would scan a parent transcript is resolved in SQL instead (resolveLinks).
type archiveFS struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	objects  Objects
	deviceID string
	masks    *maskCache
}

func (a *archiveFS) Open(p string) (claude.File, error) {
	var id string
	err := a.pool.QueryRow(a.ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2 AND storage_kind='companion' AND tombstoned_at IS NULL
		ORDER BY first_seen_at DESC LIMIT 1`, a.deviceID, p).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if err != nil {
		return nil, err
	}
	g, err := LoadGeneration(a.ctx, a.pool, id, -1)
	if errors.Is(err, ErrNoGeneration) {
		return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
	}
	if err != nil {
		return nil, err
	}
	return newRedactedFile(NewReader(a.ctx, a.objects, g), string(transcript.StorageCompanion), p), nil
}

// redactedFile is an archived file read through the server's redaction
// pass.
type redactedFile struct {
	*redact.ReaderAt
	r *Reader
}

func newRedactedFile(r *Reader, kind, path string) *redactedFile {
	return &redactedFile{ReaderAt: redact.NewReaderAt(r, redact.ModeFor(kind, path)), r: r}
}

func (f *redactedFile) Size() int64  { return f.r.Size() }
func (f *redactedFile) Close() error { return f.r.Close() }

// openRollout serves a fork parent's rollout to the Codex parser (D11) out
// of the archive: the device's Codex source whose file is named for the
// session id, at its latest generation. A parent not uploaded yet reads as
// missing, and the fork keeps the history it copied.
func (a *archiveFS) openRollout(_, sessionID string) (codex.File, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, `*?[\/%_`) {
		return nil, fs.ErrNotExist
	}
	var id string
	err := a.pool.QueryRow(a.ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND agent='codex' AND storage_kind<>'companion'
			AND tombstoned_at IS NULL AND path LIKE $2
		ORDER BY first_seen_at DESC LIMIT 1`, a.deviceID, "%/rollout-%-"+sessionID+".jsonl").Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	g, err := LoadGeneration(a.ctx, a.pool, id, -1)
	if errors.Is(err, ErrNoGeneration) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	f := newRedactedFile(NewReader(a.ctx, a.objects, g), string(transcript.StorageJSONLAppend), "")
	masks, _, err := a.masks.lineMasks(a.ctx, a.pool)
	if err != nil {
		return nil, err
	}
	f.SetLineMasks(masks)
	return f, nil
}

// Glob serves only the transcript lookups of the spawned-by fallback,
// which the archive does not answer.
func (a *archiveFS) Glob(string) ([]string, error) { return nil, nil }

func serverExtractionContract(agent string) string {
	switch transcript.Agent(agent) {
	case transcript.AgentClaude:
		return (&claude.Parser{Caps: uncapped}).ExtractionContract()
	case transcript.AgentCodex:
		return (&codex.Parser{Caps: uncapped}).ExtractionContract()
	}
	return ""
}

// afterLineMasks, when set (tests), runs after a parse loads the
// redacted-line catalog and before it reads the source.
var afterLineMasks func()

// maskCache keeps the redacted-line catalog between parses: every parse
// needs it, and redacted_lines grows with every redaction while a parse
// of an append reads a few lines. It reloads when
// redacted_lines_revision moved, reading the revision and the lines in
// one snapshot, so a cached catalog is the lines of its revision.
type maskCache struct {
	mu       sync.Mutex
	loaded   bool
	revision int64
	catalog  *redact.LineCatalog
}

// get returns the current catalog, never nil, and its revision.
func (c *maskCache) get(ctx context.Context, pool *pgxpool.Pool) (*redact.LineCatalog, int64, error) {
	var rev int64
	if err := pool.QueryRow(ctx, `SELECT revision FROM redacted_lines_revision WHERE singleton`).Scan(&rev); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	if c.loaded && c.revision == rev {
		cat := c.catalog
		c.mu.Unlock()
		return cat, rev, nil
	}
	c.mu.Unlock()
	var cat *redact.LineCatalog
	err := pgx.BeginTxFunc(ctx, pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT revision FROM redacted_lines_revision WHERE singleton`).Scan(&rev); err != nil {
			return err
		}
		var err error
		cat, err = loadLineMasks(ctx, tx)
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	if !c.loaded || rev > c.revision {
		c.loaded, c.revision, c.catalog = true, rev, cat
	}
	c.mu.Unlock()
	return cat, rev, nil
}

// lineMasks is LineMasks from the cache (nil when there are none) and the
// revision it is the lines of.
func (c *maskCache) lineMasks(ctx context.Context, pool *pgxpool.Pool) (*redact.LineCatalog, int64, error) {
	cat, rev, err := c.get(ctx, pool)
	if err != nil || cat.Empty() {
		return nil, rev, err
	}
	return cat, rev, nil
}

// redactedLinesLock orders changes to redacted_lines against the parse
// writes that mask by them. A message redaction holds it exclusively for
// its whole transaction (LockRedactedLines); every sink write transaction
// shares it from its first statement, then checks that redacted_lines
// has not moved since the parse loaded its catalog. A redaction that
// commits first therefore moves the revision before the write checks it,
// and one that commits later finds the written rows when it re-reads its
// targets.
const redactedLinesLock = "flopwire:redacted-lines"

// LockRedactedLines takes the redacted-lines lock exclusively until tx
// ends. A transaction that writes redacted_lines takes it first.
func LockRedactedLines(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, redactedLinesLock)
	return err
}

// errMasksMoved is a parse whose catalog went stale before it wrote: a
// line was redacted meanwhile. The parse starts again.
var errMasksMoved = errors.New("ingest: redacted lines changed during the parse; retry")

// maskAttempts bounds the restarts of a parse racing redactions.
const maskAttempts = 3

// LineMasks loads the redacted message lines (notes/redaction.md) for the
// server's redaction pass; nil when there are none.
func LineMasks(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (*redact.LineCatalog, error) {
	catalog, err := loadLineMasks(ctx, q)
	if err != nil {
		return nil, err
	}
	if catalog.Empty() {
		return nil, nil
	}
	return catalog, nil
}

func loadLineMasks(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) (*redact.LineCatalog, error) {
	rows, err := q.Query(ctx, `SELECT line_sha,spans,redaction_id::text,proof FROM redacted_lines`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []redact.LineMask
	for rows.Next() {
		var sum, proof []byte
		var m redact.LineMask
		if err := rows.Scan(&sum, &m.Spans, &m.RedactionID, &proof); err != nil {
			return nil, err
		}
		m.SHA = [32]byte(sum)
		if proof != nil {
			m.Proof = &redact.LineProof{}
			if err := json.Unmarshal(proof, m.Proof); err != nil {
				return nil, err
			}
		}
		records = append(records, m)
	}
	return redact.NewLineCatalog(records), rows.Err()
}
