package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/digest"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/jackc/pgx/v5"
)

// Change returns indexed observations, including all submitted IDs that share
// an abbreviated observation. Prefix observations never establish uniqueness.
func (s *Store) Change(ctx context.Context, request format.ChangeQuery, f format.Filters) (*format.Changes, error) {
	q, err := format.NormalizeChange(request, f)
	if err != nil {
		return nil, err
	}
	b := budget(ctx)
	unresolved := f
	resolveCtx, resolveCancel := context.WithTimeout(ctx, b)
	defer resolveCancel()
	if err := s.resolveFilterRepo(resolveCtx, &f); err != nil {
		return nil, spent(ctx, b, err)
	}
	backfillWhere := query{}
	convWhere(&backfillWhere, f)
	convFilters(&backfillWhere, f)
	backfillWhere.where("c.hidden_at IS NULL AND NOT dirty.limited AND (dirty.retry_after IS NULL OR dirty.retry_after<=now())")
	rows, err := s.db().Query(resolveCtx, `SELECT c.id::text FROM conversations c JOIN commit_evidence_dirty dirty ON dirty.conversation_id=c.id
	  JOIN devices d ON d.id=c.device_id JOIN users u ON u.id=c.user_id WHERE `+backfillWhere.sql()+` ORDER BY c.id LIMIT 8`, backfillWhere.args...)
	if err != nil {
		return nil, spent(ctx, b, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// The projection is derived from retained rows and needs no archive or GitHub
	// access. Bound historical migration work; unfinished work is explicit below.
	if len(ids) > 0 {
		backfillCtx, backfillCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err = ingest.BackfillCommitEvidenceFor(backfillCtx, s.Pool, ids, 8)
		backfillCancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Defer the selected backlog after a spent migration budget so a
			// slow first conversation cannot starve other requested sources.
			deferCtx, deferCancel := context.WithTimeout(ctx, 100*time.Millisecond)
			_, deferErr := s.Pool.Exec(deferCtx, `UPDATE commit_evidence_dirty SET retry_after=now()+interval '30 seconds' WHERE conversation_id=ANY($1::uuid[])`, ids)
			deferCancel()
			if deferErr != nil && !errors.Is(deferErr, context.DeadlineExceeded) {
				return nil, deferErr
			}
		}
	}
	workctx, cancel := context.WithTimeout(ctx, b)
	defer cancel()
	out := &format.Changes{Kind: "change", Matches: []format.ChangeMatch{}, Coverage: format.ChangeCoverage{VisibleIndex: true}}
	err = s.readTransaction(workctx, b, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		backend := *s
		backend.tx = tx
		f = unresolved
		if err := backend.resolveFilterRepo(workctx, &f); err != nil {
			return err
		}
		scope := format.ChangeScope(q, f)
		var cursor format.ChangeCursor
		var secret []byte
		if err := tx.QueryRow(workctx, `SELECT index_id::text,revision,watermark,cursor_secret FROM commit_evidence_state WHERE singleton`).Scan(&cursor.IndexID, &cursor.Revision, &cursor.Cutoff, &secret); err != nil {
			return err
		}
		cursor.Scope = scope
		if q.Cursor != "" {
			instance, err := format.ChangeCursorIndex(q.Cursor)
			if err != nil {
				return err
			}
			if instance != cursor.IndexID {
				out.RestartRequired = true
				return nil
			}
			prior, err := format.VerifyChangeCursor(q.Cursor, secret)
			if err != nil {
				return err
			}
			if prior.Scope != scope || prior.IndexID != cursor.IndexID {
				return fmt.Errorf("%w: cursor belongs to another change lookup", ErrBadRequest)
			}
			if prior.Revision != cursor.Revision {
				out.RestartRequired = true
				return nil
			}
			if prior.Cutoff > cursor.Cutoff {
				return fmt.Errorf("%w: invalid change cutoff", ErrBadRequest)
			}
			cursor = prior
		}
		where := query{}
		convWhere(&where, f)
		convFilters(&where, f)
		// Change deliberately includes the caller's sessions and descendants.
		where.where("c.hidden_at IS NULL")
		var lag, limited, sourceTextTruncated bool
		var unavailable int
		if err := tx.QueryRow(workctx, `SELECT COALESCE(bool_or(EXISTS(SELECT 1 FROM commit_evidence_dirty dirty WHERE dirty.conversation_id=c.id AND NOT dirty.limited)
    OR EXISTS(SELECT 1 FROM source_parse_state p WHERE p.source_id=c.source_id AND (p.requested_seq>p.parsed_seq OR p.refresh_requested_at IS NOT NULL))),false),
			count(*) FILTER(WHERE EXISTS(SELECT 1 FROM source_parse_state p WHERE p.source_id=c.source_id AND p.quarantined_at IS NOT NULL)),
			COALESCE(bool_or(EXISTS(SELECT 1 FROM commit_evidence_dirty dirty WHERE dirty.conversation_id=c.id AND dirty.limited)),false),
			COALESCE(bool_or(EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND NOT m.superseded AND m.on_active_path IS NOT FALSE
			  AND (m.text_len>octet_length(m.text) OR m.enrichment->'text_truncated'='true'::jsonb))),false)
			FROM conversations c JOIN devices d ON d.id=c.device_id JOIN users u ON u.id=c.user_id WHERE `+where.sql(), where.args...).Scan(&lag, &unavailable, &limited, &sourceTextTruncated); err != nil {
			return err
		}
		out.Coverage.IndexingLag = lag
		out.Coverage.UnavailableSources = unavailable
		out.Coverage.Truncated = limited
		out.Coverage.SourceTextTruncated = sourceTextTruncated
		prefixes := []string{}
		seen := map[string]bool{}
		for _, h := range q.Commits {
			for n := 7; n <= len(h); n++ {
				prefix := h[:n]
				if !seen[prefix] {
					seen[prefix] = true
					prefixes = append(prefixes, prefix)
				}
			}
		}
		prefixArg := where.arg(prefixes)
		where.where("e.observed_hash=p.hash")
		where.where("e.id>" + where.arg(cursor.After) + " AND e.id<=" + where.arg(cursor.Cutoff))
		where.where("s.tombstoned_at IS NULL")
		// Probe the (hash,id) index for each submitted prefix. Applying the
		// visible scope inside each probe makes the per-prefix limit safe,
		// and avoids scanning the event-ID index until a rare hash appears.
		rows, err := tx.Query(workctx, `SELECT hit.* FROM unnest(`+prefixArg+`::text[]) p(hash) CROSS JOIN LATERAL (
			SELECT e.id AS evidence_id,e.observed_hash,e.evidence,e.command_id::text,e.result_id::text,
    COALESCE(e.reveal_command_id::text,''),COALESCE(e.reveal_result_id::text,''),
    c.id::text,c.agent,c.session_id,left(COALESCE(c.title,''),256),left(COALESCE(c.repo_root,c.cwd,''),4096),d.name,u.email,
    COALESCE(NULLIF(rs.remote,''),rs.checkout,''),COALESCE(c.repo_root,'')
    FROM commit_evidence e JOIN conversations c ON c.id=e.conversation_id JOIN sources s ON s.id=e.source_id
    JOIN devices d ON d.id=c.device_id JOIN users u ON u.id=c.user_id LEFT JOIN sources rs ON rs.id=c.source_id
			WHERE `+where.sql()+` ORDER BY e.id LIMIT `+fmt.Sprint(q.Limit+1)+`) hit ORDER BY hit.evidence_id LIMIT `+fmt.Sprint(q.Limit+1), where.args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		bytes := 0
		for rows.Next() {
			var id int64
			var data []byte
			var match format.ChangeMatch
			if err := rows.Scan(&id, &match.ObservedHash, &data, &match.CommandAddress, &match.ResultAddress, &match.RevealCommandAddress, &match.RevealResultAddress,
				&match.Session.ID, &match.Session.Agent, &match.Session.SessionID, &match.Session.Title, &match.Session.Repo, &match.Session.Device, &match.Session.User, &match.Session.RepoIdentity, &match.Session.Worktree); err != nil {
				return err
			}
			if len(out.Matches) == q.Limit {
				out.HasMore = true
				break
			}
			var evidence digest.CommitEvidence
			if err := json.Unmarshal(data, &evidence); err != nil {
				return err
			}
			match.Session.Address = match.Session.SessionID
			match.CommandRaw = match.CommandAddress == match.ResultAddress
			match.RevealCommandRaw = match.RevealCommandAddress != "" && match.RevealCommandAddress == match.RevealResultAddress
			match.Operation = evidence.Operation
			match.Outcome = evidence.Outcome
			match.CommandDirectory = evidence.CommandDirectory
			match.DirectoryKnown = evidence.DirectoryKnown
			match.Match = "prefix"
			for _, h := range q.Commits {
				if strings.HasPrefix(h, match.ObservedHash) {
					match.InputCommits = append(match.InputCommits, h)
					if h == match.ObservedHash {
						match.Match = "exact"
					}
				}
			}
			match, size := format.BoundedChangeMatch(match)
			if bytes+size > format.ChangePageBudget && len(out.Matches) > 0 {
				out.HasMore = true
				out.Coverage.Truncated = true
				break
			}
			bytes += size
			out.Matches = append(out.Matches, match)
			cursor.After = id
			out.Coverage.UnknownDirectory = out.Coverage.UnknownDirectory || !match.DirectoryKnown
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if out.HasMore {
			out.NextCursor = format.SignChangeCursor(cursor, secret)
		}
		return nil
	})
	if err != nil {
		return nil, spent(ctx, b, err)
	}
	return out, nil
}
