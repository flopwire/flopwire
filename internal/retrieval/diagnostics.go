package retrieval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

// Apply the same visibility rule as raw evidence, including parent sources.
const diagnosticSources = `WITH current AS (SELECT s.id,s.path,s.agent,p.extraction_report FROM sources s JOIN source_parse_state p ON p.source_id=s.id
 WHERE s.tombstoned_at IS NULL AND s.agent IN ('claude','codex') AND s.storage_kind='jsonl_append'
 AND NOT EXISTS(SELECT 1 FROM sources newer WHERE newer.previous_source_id=s.id AND newer.tombstoned_at IS NULL AND newer.id IS DISTINCT FROM s.previous_source_id)
 AND NOT EXISTS(SELECT 1 FROM conversations c WHERE c.hidden_at IS NOT NULL AND (c.source_id IN (s.id,s.parent_source_id) OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=c.id AND m.source_id IN (s.id,s.parent_source_id))))) `

func (s *Store) ExtractionSummary(ctx context.Context) (*transcript.ExtractionSummary, error) {
	out := &transcript.ExtractionSummary{Counts: map[transcript.DiagnosticCode]uint64{}, Affected: []transcript.SourceReference{}}
	err := s.db().QueryRow(ctx, diagnosticSources+`SELECT count(extraction_report),count(*)-count(extraction_report),count(*) FILTER(WHERE jsonb_array_length(extraction_report->'report'->'issues')>0),count(*) FILTER(WHERE EXISTS(SELECT 1 FROM jsonb_array_elements(extraction_report->'report'->'issues') j WHERE j->>'code'<>'unknown_record_type')),count(*) FILTER(WHERE EXISTS(SELECT 1 FROM jsonb_array_elements(extraction_report->'report'->'issues') j WHERE j->>'code'='unknown_record_type')) FROM current`).Scan(&out.AssessedSources, &out.UnassessedSources, &out.AffectedSources, &out.WarningSources, &out.InfoSources)
	if err != nil {
		return nil, err
	}
	rows, err := s.db().Query(ctx, diagnosticSources+`SELECT j->>'code',sum((j->>'count')::numeric)::text FROM current,jsonb_array_elements(extraction_report->'report'->'issues') j GROUP BY j->>'code'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var code transcript.DiagnosticCode
		var n string
		if err := rows.Scan(&code, &n); err != nil {
			rows.Close()
			return nil, err
		}
		var count uint64
		if err := json.Unmarshal([]byte(n), &count); err != nil {
			rows.Close()
			return nil, err
		}
		out.Counts[code] = count
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db().Query(ctx, diagnosticSources+`SELECT id::text,path,agent FROM current WHERE jsonb_array_length(extraction_report->'report'->'issues')>0 ORDER BY id LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref transcript.SourceReference
		if err := rows.Scan(&ref.SourceID, &ref.Path, &ref.Agent); err != nil {
			return nil, err
		}
		out.Affected = append(out.Affected, ref)
	}
	return out, rows.Err()
}
func (s *Store) SourceDiagnostics(ctx context.Context, sourceID string) (*transcript.SourceDiagnostics, error) {
	if _, err := uuid.Parse(sourceID); err != nil {
		return nil, fmt.Errorf("%w: invalid source_id", ErrBadRequest)
	}
	if _, err := s.attribution(ctx, sourceID, 0, 0, 1); err != nil {
		return nil, err
	}
	out := &transcript.SourceDiagnostics{SourceReference: transcript.SourceReference{SourceID: sourceID}}
	var raw []byte
	err := s.db().QueryRow(ctx, `SELECT s.path,s.agent,s.device_id::text,p.extraction_report,(SELECT count(*) FROM messages m WHERE m.source_id=s.id AND NOT m.superseded AND m.enrichment->>'persisted_output_missing' IS NOT NULL),(SELECT count(*) FROM messages m WHERE m.source_id=s.id AND NOT m.superseded AND m.enrichment->>'persisted_output_truncated'='true') FROM sources s LEFT JOIN source_parse_state p ON p.source_id=s.id WHERE s.id=$1`, sourceID).Scan(&out.Path, &out.Agent, &out.DeviceID, &raw, &out.MissingCompanions, &out.TruncatedCompanions)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		out.Extraction = &transcript.ExtractionCheckpoint{}
		err = json.Unmarshal(raw, out.Extraction)
	}
	return out, err
}
