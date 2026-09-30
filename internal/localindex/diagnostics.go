package localindex

import (
	"context"
	"strconv"

	"github.com/flopwire/flopwire/internal/transcript"
)

// ExtractionSummary aggregates counters in SQLite, without loading locator samples.
func (s *Store) ExtractionSummary(ctx context.Context) (*transcript.ExtractionSummary, error) {
	out := &transcript.ExtractionSummary{Counts: map[transcript.DiagnosticCode]uint64{}, Affected: []transcript.SourceReference{}}
	err := s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		// Newest identity at each device/path is current; replaced sources remain history.
		const current = `WITH current AS (SELECT s.* FROM sources s WHERE s.agent IN ('claude','codex') AND s.storage_kind='jsonl_append' AND NOT EXISTS (SELECT 1 FROM sources n WHERE n.device_id=s.device_id AND n.path=s.path AND n.id>s.id)) `
		if err := q.QueryRowContext(ctx, current+`SELECT count(extraction_report),count(*)-count(extraction_report),coalesce(sum(json_array_length(extraction_report,'$.report.issues')>0),0),coalesce(sum(EXISTS(SELECT 1 FROM json_each(extraction_report,'$.report.issues') WHERE json_extract(value,'$.code')<>'unknown_record_type')),0),coalesce(sum(EXISTS(SELECT 1 FROM json_each(extraction_report,'$.report.issues') WHERE json_extract(value,'$.code')='unknown_record_type')),0) FROM current`).Scan(&out.AssessedSources, &out.UnassessedSources, &out.AffectedSources, &out.WarningSources, &out.InfoSources); err != nil {
			return err
		}
		rows, err := q.QueryContext(ctx, current+`SELECT json_extract(j.value,'$.code'),sum(json_extract(j.value,'$.count')) FROM current,json_each(extraction_report,'$.report.issues') j GROUP BY json_extract(j.value,'$.code')`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var code transcript.DiagnosticCode
			var n uint64
			if err := rows.Scan(&code, &n); err != nil {
				rows.Close()
				return err
			}
			out.Counts[code] = n
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		rows, err = q.QueryContext(ctx, current+`SELECT cast(id AS text),path,agent FROM current WHERE json_array_length(extraction_report,'$.report.issues')>0 ORDER BY id DESC LIMIT 20`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref transcript.SourceReference
			if err := rows.Scan(&ref.SourceID, &ref.Path, &ref.Agent); err != nil {
				return err
			}
			out.Affected = append(out.Affected, ref)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Store) SourceDiagnostics(ctx context.Context, sourceID string) (*transcript.SourceDiagnostics, error) {
	id, err := strconv.ParseInt(sourceID, 10, 64)
	if err != nil {
		return nil, err
	}
	var st SourceState
	err = s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		var err error
		st, err = scanSource(q.QueryRowContext(ctx, sourceColsNoState+"WHERE id=?", id))
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &transcript.SourceDiagnostics{SourceReference: transcript.SourceReference{SourceID: sourceID, Path: st.Source.Path, Agent: st.Source.Agent}, Extraction: st.Extraction}
	err = s.readSources(ctx, func(ctx context.Context, q dbtx) error {
		return q.QueryRowContext(ctx, `SELECT s.device_id,(SELECT count(*) FROM messages WHERE source_id=s.id AND NOT superseded AND json_extract(enrichment,'$.persisted_output_missing') IS NOT NULL),(SELECT count(*) FROM messages WHERE source_id=s.id AND NOT superseded AND json_extract(enrichment,'$.persisted_output_truncated')=1) FROM sources s WHERE s.id=?`, id).Scan(&out.DeviceID, &out.MissingCompanions, &out.TruncatedCompanions)
	})
	return out, err
}
