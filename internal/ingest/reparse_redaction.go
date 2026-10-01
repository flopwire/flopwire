package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/jackc/pgx/v5"
)

// maskStoredVersions applies new rules to historical row versions too. A
// reparse can supersede an old row; it must not leave its secret searchable.
// Batch commits are idempotent. Only derived data changes, never the archive.
func (q *Queue) maskStoredVersions(ctx context.Context, source string) error {
	for {
		n := 0
		err := pgx.BeginFunc(ctx, q.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT id::text,text,enrichment FROM messages WHERE source_id=$1
    AND redaction_rules IS DISTINCT FROM $2 ORDER BY id LIMIT 64 FOR UPDATE NOWAIT`, source, redact.RulesVersion)
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
			n = len(records)
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
		if n == 0 {
			break
		}
	}
	// Cached summaries can retain strings no longer present in a live row.
	last := ""
	for {
		var ids []string
		err := pgx.BeginFunc(ctx, q.Pool, func(tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT c.id::text,COALESCE(c.title,''),c.digest FROM conversations c
    WHERE c.id::text>$2 AND (c.source_id=$1 OR EXISTS(SELECT 1 FROM messages m WHERE m.source_id=$1 AND m.conversation_id=c.id))
    ORDER BY c.id::text LIMIT 64 FOR UPDATE NOWAIT`, source, last)
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
				ids = append(ids, r.id)
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
		if len(ids) == 0 {
			return nil
		}
		last = ids[len(ids)-1]
	}
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
