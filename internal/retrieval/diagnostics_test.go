package retrieval_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

func TestDiagnosticsHTTPVisibilityAndAudit(t *testing.T) {
	s := newServer(t)
	s.ingestFixtures()
	ctx := context.Background()
	var summary transcript.ExtractionSummary
	if err := s.client.JSON(ctx, http.MethodGet, "/v1/diagnostics", nil, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.AssessedSources == 0 {
		t.Fatalf("no assessments: %+v", summary)
	}
	var id string
	if err := s.pool.QueryRow(ctx, `SELECT p.source_id::text FROM source_parse_state p JOIN sources s ON s.id=p.source_id JOIN conversations c ON c.source_id=s.id WHERE p.extraction_report IS NOT NULL AND c.hidden_at IS NULL LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Replacement identities keep historical reports available by source ID,
	// but the current summary must stop counting them.
	newID := uuid.NewString()
	if _, err := s.pool.Exec(ctx, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at,previous_source_id) SELECT $2,device_id,agent,path,file_id||'-replacement',storage_kind,parser,now(),id FROM sources WHERE id=$1`, id, newID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO source_parse_state(source_id,generation,cursor_offset,cursor_line,extraction_report) SELECT $2,generation,cursor_offset,cursor_line,extraction_report FROM source_parse_state WHERE source_id=$1`, id, newID); err != nil {
		t.Fatal(err)
	}
	var replaced transcript.ExtractionSummary
	if err := s.client.JSON(ctx, http.MethodGet, "/v1/diagnostics", nil, &replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.AssessedSources != summary.AssessedSources {
		t.Fatal("replacement report counted twice")
	}
	// The old identity returning (inode reuse) names its own replacement.
	if _, err := s.pool.Exec(ctx, `UPDATE sources SET previous_source_id=$2 WHERE id=$1`, id, newID); err != nil {
		t.Fatal(err)
	}
	if err := s.client.JSON(ctx, http.MethodGet, "/v1/diagnostics", nil, &replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.AssessedSources != summary.AssessedSources+1 {
		t.Fatal("mutual replacement excluded returning source")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sources WHERE id=$1`, newID); err != nil {
		t.Fatal(err)
	}
	var detail transcript.SourceDiagnostics
	path := "/v1/diagnostics?source_id=" + id
	if err := s.client.JSON(ctx, http.MethodGet, path, nil, &detail); err != nil || detail.Extraction == nil {
		t.Fatalf("detail %+v %v", detail, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE conversations SET hidden_at=now() WHERE source_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	err := s.client.JSON(ctx, http.MethodGet, path, nil, &detail)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
		t.Fatalf("hidden detail: %v", err)
	}
	var after transcript.ExtractionSummary
	if err := s.client.JSON(ctx, http.MethodGet, "/v1/diagnostics", nil, &after); err != nil {
		t.Fatal(err)
	}
	if after.AssessedSources >= summary.AssessedSources {
		t.Fatal("hidden source remains in summary")
	}
	if s.count(`SELECT count(*) FROM audit_events WHERE action='diagnostics.read'`) < 4 {
		t.Fatal("diagnostics reads not audited")
	}
	anon := s.client
	anon.Token = ""
	err = anon.JSON(ctx, http.MethodGet, path, nil, &detail)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 {
		t.Fatalf("unauthenticated detail: %v", err)
	}
}
