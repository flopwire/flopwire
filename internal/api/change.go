package api

import (
	"context"
	"net/http"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

type changeRetrieval interface {
	Change(context.Context, format.ChangeQuery, format.Filters) (*format.Changes, error)
}

func (a *API) change(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filters(w, r)
	if !ok {
		return
	}
	q, err := format.ParseChangeQuery(r.URL.Query())
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	q, err = format.NormalizeChange(q, f)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	b, ok := a.retrieval.(changeRetrieval)
	if !ok {
		problem(w, 501, "commit evidence lookup is not configured")
		return
	}
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	meta := map[string]any{"commits": q.Commits, "filters": f}
	page, err := b.Change(ctx, q, f)
	if err != nil {
		a.retrievalFailed(w, r, "change", "search", "", meta, err)
		return
	}
	if page == nil {
		page = &format.Changes{}
	}
	ids := make([]string, 0, len(page.Matches))
	for _, match := range page.Matches {
		ids = append(ids, match.Session.ID)
	}
	meta["outcome"], meta["count"], meta["result_ids"] = "success", len(page.Matches), ids
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "change", "search", "", meta) {
		return
	}
	writeJSON(w, 200, page)
}
