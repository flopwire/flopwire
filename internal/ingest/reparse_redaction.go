package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"slices"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/jackc/pgx/v5"
)

// The rules upgrade reads every stored version of a source, superseded ones
// included, through messages_source_idx (source_id, superseded). Each query
// below reads the source's rows once: the ids to rewrite are listed first,
// then locked and rewritten 64 at a time by primary key. Rows a batch finds
// already stamped (a concurrent upgrade got there first) are skipped.
const (
	staleVersions = `SELECT id::text FROM messages WHERE source_id=$1
    AND redaction_rules IS DISTINCT FROM $2 ORDER BY messages.id`
	staleVersionsBatch = `SELECT id::text,text,enrichment FROM messages WHERE id=ANY($1::uuid[])
    AND redaction_rules IS DISTINCT FROM $2 ORDER BY messages.id FOR UPDATE NOWAIT`
	// A source's conversations: those that name it and those holding any
	// of its row versions.
	sourceConversations = `SELECT id::text FROM (SELECT id FROM conversations WHERE source_id=$1
    UNION SELECT conversation_id FROM messages WHERE source_id=$1) s ORDER BY s.id`
	sourceConversationsBatch = `SELECT id::text,COALESCE(title,''),digest FROM conversations
    WHERE id=ANY($1::uuid[]) ORDER BY conversations.id FOR UPDATE NOWAIT`
)

// maskStoredVersions applies new rules to historical row versions too. A
// reparse can supersede an old row; it must not leave its secret searchable.
// Batch commits are idempotent. Only derived data changes, never the archive.
// The sink writes every row under the current rules, so a row stored after
// the id list is taken needs no masking.
func (q *Queue) maskStoredVersions(ctx context.Context, source string) error {
	rows, err := q.Pool.Query(ctx, staleVersions, source, redact.RulesVersion)
	if err != nil {
		return err
	}
	stale, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for batch := range slices.Chunk(stale, 64) {
		err := pgx.BeginFunc(ctx, q.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, staleVersionsBatch, batch, redact.RulesVersion)
			if err != nil {
				return archiveLockError(err)
			}
			type record struct {
				id, text   string
				enrichment []byte
			}
			records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (record, error) {
				var r record
				err := row.Scan(&r.id, &r.text, &r.enrichment)
				return r, err
			})
			if err != nil {
				return archiveLockError(err)
			}
			for _, r := range records {
				text, _ := redact.Redact([]byte(r.text))
				enrichment, err := maskRuleJSON(r.enrichment)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(text)
				if _, err := tx.Exec(ctx, `UPDATE messages SET text=$2,content_sha=$3,enrichment=$4,redaction_rules=$5 WHERE id=$1`, r.id, string(text), sum[:], enrichment, redact.RulesVersion); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	// Cached summaries can retain strings no longer present in a live row.
	rows, err = q.Pool.Query(ctx, sourceConversations, source)
	if err != nil {
		return err
	}
	conversations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for batch := range slices.Chunk(conversations, 64) {
		err := pgx.BeginFunc(ctx, q.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sourceConversationsBatch, batch)
			if err != nil {
				return archiveLockError(err)
			}
			type conversation struct {
				id, title string
				digest    []byte
			}
			records, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (conversation, error) {
				var r conversation
				err := row.Scan(&r.id, &r.title, &r.digest)
				return r, err
			})
			if err != nil {
				return archiveLockError(err)
			}
			for _, r := range records {
				title, _ := redact.Redact([]byte(r.title))
				digest, err := maskRuleJSON(r.digest)
				if err != nil {
					return err
				}
				if string(title) != r.title || !bytes.Equal(digest, r.digest) {
					if _, err := tx.Exec(ctx, `UPDATE conversations SET title=$2,digest=$3 WHERE id=$1`, r.id, string(title), digest); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Decode strings before masking, including escaped PEM blocks in JSON.
func maskRuleJSON(raw []byte) ([]byte, error) {
	if raw == nil {
		return nil, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	changed := false
	var mask func(any) any
	mask = func(v any) any {
		switch x := v.(type) {
		case string:
			out, matches := redact.Redact([]byte(x))
			changed = changed || len(matches) > 0
			return string(out)
		case []any:
			for i, item := range x {
				x[i] = mask(item)
			}
		case map[string]any:
			for key, item := range x {
				x[key] = mask(item)
			}
		}
		return v
	}
	value = mask(value)
	if !changed {
		return raw, nil
	}
	return json.Marshal(value)
}
