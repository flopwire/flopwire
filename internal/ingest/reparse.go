package ingest

import (
	"context"
	"errors"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/jackc/pgx/v5"
)

func serverParserVersion(agent string) string {
	switch transcript.Agent(agent) {
	case transcript.AgentClaude:
		return claude.ParserName
	case transcript.AgentCodex:
		return codex.Name
	case transcript.AgentDevin:
		return devin.Name
	}
	return ""
}

// The applied stamps are the durable work state. Minor releases do not
// change the key; extraction policy or redaction changes still require work.
const staleParseSQL = `s.tombstoned_at IS NULL AND s.storage_kind<>'companion'
 AND s.agent IN ('claude','codex','devin') AND (
 regexp_replace(p.applied_parser,'@([0-9]+)\.[0-9]+','@\1','g') IS DISTINCT FROM
 CASE s.agent WHEN 'claude' THEN $1 WHEN 'codex' THEN $2 ELSE $3 END
 OR p.applied_redaction_rules IS DISTINCT FROM $4
 OR (s.agent IN ('claude','codex') AND (
 regexp_replace(p.extraction_report->>'contract','@([0-9]+)\.[0-9]+','@\1','g') IS DISTINCT FROM CASE s.agent WHEN 'claude' THEN $5 ELSE $6 END
 OR p.extraction_report->'report'->>'version' IS DISTINCT FROM '1')))`

func reparseArgs() []any {
	return []any{transcript.ReparseKey(claude.ParserName), transcript.ReparseKey(codex.Name), transcript.ReparseKey(devin.Name), redact.RulesVersion, serverExtractionContract("claude"), serverExtractionContract("codex")}
}

// refreshPickSQL selects idle stale work: not live ingestion, and not in
// backoff or quarantine. Sources without a generation have no work.
const refreshPickSQL = `SELECT s.id::text FROM source_parse_state p JOIN sources s ON s.id=p.source_id
 JOIN LATERAL (SELECT captured_at FROM generations WHERE source_id=s.id ORDER BY generation DESC LIMIT 1) g ON true
 WHERE p.requested_seq=p.parsed_seq AND p.quarantined_at IS NULL
 AND (p.next_attempt_at IS NULL OR p.next_attempt_at<=now()) AND ` + staleParseSQL

// nextRefresh selects idle work without turning an upgrade into live-ingest
// backpressure. Backoff and quarantine apply to refreshes too.
//
// A source a read prioritized comes first (an indexed query). Otherwise the
// next source comes from the backlog, newest capture first. Staleness is
// relative to this binary's versions, so no index can find it: the backlog
// is listed by one scan and then consumed one source per call, each
// checked again by id, so that refreshing S sources costs one scan rather
// than S. The scan is repeated when the backlog runs out; a source whose
// backoff ended meanwhile waits for that.
func (q *Queue) nextRefresh(ctx context.Context) (string, error) {
	q.refreshMu.Lock()
	defer q.refreshMu.Unlock()
	args := reparseArgs()
	var id string
	err := q.Pool.QueryRow(ctx, refreshPickSQL+` AND p.refresh_requested_at IS NOT NULL
 ORDER BY p.refresh_requested_at DESC,g.captured_at DESC,s.id LIMIT 1`, args...).Scan(&id)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) {
		return id, err
	}
	listed := false
	for {
		if len(q.refreshBacklog) == 0 {
			if listed {
				return "", nil
			}
			listed = true
			rows, err := q.Pool.Query(ctx, refreshPickSQL+` ORDER BY g.captured_at DESC,s.id`, args...)
			if err != nil {
				return "", err
			}
			if q.refreshBacklog, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
				return "", err
			}
			continue
		}
		id := q.refreshBacklog[0]
		var due bool
		if err := q.Pool.QueryRow(ctx, `SELECT EXISTS(`+refreshPickSQL+` AND s.id=$7::uuid)`, append(args, id)...).Scan(&due); err != nil {
			return "", err
		}
		q.refreshBacklog = q.refreshBacklog[1:]
		if due {
			return id, nil
		}
	}
}

// RefreshSession prioritizes stale sources only after retrieval authorized
// the conversation. It performs no extraction and never delays a read for it.
func (q *Queue) RefreshSession(ctx context.Context, conversation string) {
	q.init()
	args := append(reparseArgs(), conversation)
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	tag, err := q.Pool.Exec(ctx, `UPDATE source_parse_state p SET refresh_requested_at=now()
 FROM sources s WHERE s.id=p.source_id AND `+staleParseSQL+`
 AND EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=$7 AND m.source_id=s.id)`, args...)
	if err != nil {
		q.Log.Warn("ingest: prioritizing stale session refresh", "error", err)
	} else if tag.RowsAffected() > 0 {
		select {
		case q.refreshWake <- struct{}{}:
		default:
		}
	}
}

// refresh runs one idle extraction at a time, pausing between sources.
// Live upload workers continue independently. Version stamps survive restart.
func (q *Queue) refresh(ctx context.Context) {
	for {
		id, err := q.nextRefresh(ctx)
		if err != nil && ctx.Err() == nil {
			q.Log.Warn("ingest: selecting stale source", "error", err)
		}
		if id != "" {
			q.runParse(ctx, id, q.refreshSource)
		}
		pause := q.RefreshInterval
		if id == "" {
			pause = max(pause, time.Minute)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-q.refreshWake:
			if id != "" {
				// Priority changes must not bypass pacing after a parse.
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			} else {
				timer.Stop()
			}
		case <-timer.C:
		}
	}
}
