package api

import (
	"context"
	"net/http"

	"github.com/flopwire/flopwire/internal/transcript"
)

type diagnosticReader interface {
	ExtractionSummary(context.Context) (*transcript.ExtractionSummary, error)
	SourceDiagnostics(context.Context, string) (*transcript.SourceDiagnostics, error)
}

func (a *API) diagnostics(w http.ResponseWriter, r *http.Request) {
	backend, ok := a.retrieval.(diagnosticReader)
	if !ok {
		problem(w, 501, "this server has no extraction diagnostics")
		return
	}
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	id := r.URL.Query().Get("source_id")
	var out any
	var err error
	if id == "" {
		out, err = backend.ExtractionSummary(ctx)
	} else {
		out, err = backend.SourceDiagnostics(ctx, id)
	}
	if err != nil {
		a.retrievalFailed(w, r, "diagnostics.read", "source", id, map[string]any{}, err)
		return
	}
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "diagnostics.read", "source", id, map[string]any{"outcome": "success"}) {
		return
	}
	writeJSON(w, 200, out)
}
