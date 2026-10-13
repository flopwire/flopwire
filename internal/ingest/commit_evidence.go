package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/jackc/pgx/v5"
)

// CommitEvidenceMessageLimit and CommitEvidenceTextLimit bound a projection
// rebuild. An oversized conversation stays dirty and lookup reports lag.
const CommitEvidenceMessageLimit = 100000
const CommitEvidenceTextLimit = 64 << 20

// RefreshCommitEvidence atomically replaces one conversation's projection from
// retained live rows. The caller holds its conversation row lock. Appended rows
// can complete an older call; replay is source scoped, never cross-source.
// Unchanged observations keep their IDs so appends do not disturb open pages.
func RefreshCommitEvidence(ctx context.Context, tx pgx.Tx, conv string) error {
	var dirty bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commit_evidence_dirty WHERE conversation_id=$1)`, conv).Scan(&dirty); err != nil || !dirty {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT m.id::text,m.source_id::text,m.source_generation,COALESCE(m.native_id,''),m.ordinal,m.part,m.kind,
		COALESCE(m.tool_name,''),COALESCE(m.tool_call_id,''),COALESCE(m.is_error,false),
		CASE WHEN octet_length(m.text)+COALESCE(octet_length(m.enrichment::text),0)<=$3 THEN m.text ELSE '' END,
		COALESCE(m.line_no,0),COALESCE(m.byte_offset,0),COALESCE(m.byte_len,0),COALESCE(m.locator,''),
		CASE WHEN octet_length(m.text)+COALESCE(octet_length(m.enrichment::text),0)<=$3 THEN m.enrichment ELSE NULL END,
		octet_length(m.text)+COALESCE(octet_length(m.enrichment::text),0)
   FROM messages m JOIN sources s ON s.id=m.source_id
   WHERE m.conversation_id=$1 AND NOT m.superseded AND m.on_active_path IS NOT FALSE AND s.tombstoned_at IS NULL
		AND m.kind IN ('tool_call','tool_result') ORDER BY m.source_id,m.ordinal,m.id LIMIT $2`, conv, CommitEvidenceMessageLimit+1, CommitEvidenceTextLimit)
	if err != nil {
		return err
	}
	type sourceMessages struct {
		generation int64
		messages   []transcript.Message
		refs       map[digest.EvidenceRef]string
	}
	type sourceGeneration struct {
		source     string
		generation int64
	}
	sources := map[sourceGeneration]*sourceMessages{}
	total, count := 0, 0
	for rows.Next() {
		var id, source, kind string
		var generation int64
		var m transcript.Message
		var enrichment []byte
		var textBytes int
		if err := rows.Scan(&id, &source, &generation, &m.NativeID, &m.Ordinal, &m.Part, &kind, &m.ToolName, &m.ToolCallID, &m.IsError, &m.Text, &m.LineNo, &m.ByteOffset, &m.ByteLen, &m.Locator, &enrichment, &textBytes); err != nil {
			rows.Close()
			return err
		}
		count++
		total += textBytes
		if count > CommitEvidenceMessageLimit || total > CommitEvidenceTextLimit {
			rows.Close()
			_, err := tx.Exec(ctx, `UPDATE commit_evidence_dirty SET limited=true WHERE conversation_id=$1`, conv)
			return err
		}
		m.Kind, err = transcript.ParseKind(kind)
		if err != nil {
			rows.Close()
			return err
		}
		if len(enrichment) > 0 {
			if err := json.Unmarshal(enrichment, &m.Enrichment); err != nil {
				rows.Close()
				return err
			}
		}
		sourceKey := sourceGeneration{source, generation}
		group := sources[sourceKey]
		if group == nil {
			group = &sourceMessages{refs: map[digest.EvidenceRef]string{}}
			sources[sourceKey] = group
		}
		if generation > group.generation {
			group.generation = generation
		}
		ref := digest.EvidenceRef{NativeID: m.NativeID, Ordinal: m.Ordinal, Part: m.Part, LineNo: m.LineNo, ByteOffset: m.ByteOffset, ByteLen: m.ByteLen, Locator: m.Locator}
		group.refs[ref] = id
		group.messages = append(group.messages, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	fingerprints := []string{}
	for source, group := range sources {
		for _, e := range digest.ExtractCommitEvidence(group.messages) {
			command, result := group.refs[e.Command], group.refs[e.Result]
			if command == "" || result == "" {
				return fmt.Errorf("commit evidence: extractor returned an unavailable message")
			}
			var revealCommand, revealResult *string
			if e.RevealCommand != nil {
				id := group.refs[*e.RevealCommand]
				if id == "" {
					return fmt.Errorf("commit evidence: unavailable reveal command")
				}
				revealCommand = &id
			}
			if e.RevealResult != nil {
				id := group.refs[*e.RevealResult]
				if id == "" {
					return fmt.Errorf("commit evidence: unavailable reveal result")
				}
				revealResult = &id
			}
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			key, _ := json.Marshal(struct {
				Source, Command, Result     string
				Generation                  int64
				RevealCommand, RevealResult *string
				Evidence                    json.RawMessage
			}{source.source, command, result, source.generation, revealCommand, revealResult, data})
			sum := sha256.Sum256(key)
			fingerprint := hex.EncodeToString(sum[:])
			fingerprints = append(fingerprints, fingerprint)
			if _, err := tx.Exec(ctx, `INSERT INTO commit_evidence(conversation_id,source_id,source_generation,fingerprint,observed_hash,command_id,result_id,reveal_command_id,reveal_result_id,evidence)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(conversation_id,fingerprint) DO NOTHING`, conv, source.source, source.generation, fingerprint, e.Hash, command, result, revealCommand, revealResult, data); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM commit_evidence WHERE conversation_id=$1 AND fingerprint<>ALL($2::text[])`, conv, fingerprints); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM commit_evidence_dirty WHERE conversation_id=$1`, conv)
	return err
}

// BackfillCommitEvidence advances the rebuildable index by a bounded batch.
// Each projection commits independently; cancellation preserves earlier work.
func BackfillCommitEvidence(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, limit int) error {
	return BackfillCommitEvidenceFor(ctx, pool, nil, limit)
}

// BackfillCommitEvidenceFor restricts migration work to the requested visible
// scope. A nil conversations slice selects all conversations.
func BackfillCommitEvidenceFor(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, conversations []string, limit int) error {
	for i := 0; i < limit; i++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		var conv string
		err = tx.QueryRow(ctx, `SELECT c.id::text FROM conversations c JOIN commit_evidence_dirty d ON d.conversation_id=c.id
			WHERE c.hidden_at IS NULL AND NOT d.limited AND (d.retry_after IS NULL OR d.retry_after<=now()) AND ($1::uuid[] IS NULL OR c.id=ANY($1)) ORDER BY c.id FOR UPDATE OF c SKIP LOCKED LIMIT 1`, conversations).Scan(&conv)
		if err == pgx.ErrNoRows {
			_ = tx.Rollback(ctx)
			return nil
		}
		if err == nil {
			err = RefreshCommitEvidence(ctx, tx, conv)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
