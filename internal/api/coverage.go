package api

import (
	"context"
	"net/http"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/domain"
)

type deviceCoverageReader interface {
	DeviceParseCoverage(context.Context, string) (coverage.ParseSnapshot, error)
}

func (a *API) deviceCoverage(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	if p.Credential.Kind == domain.CredentialSession || p.Credential.DeviceID == "" {
		if a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "device_credential_required"}) {
			problem(w, http.StatusForbidden, "coverage requires a device credential")
		}
		return
	}
	if r.URL.RawQuery != "" {
		problem(w, http.StatusBadRequest, "coverage accepts no query parameters")
		return
	}
	backend, ok := a.parse.(deviceCoverageReader)
	if !ok {
		problem(w, http.StatusNotImplemented, "device parse coverage is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	out, err := backend.DeviceParseCoverage(ctx, p.Credential.DeviceID)
	if err != nil {
		problem(w, http.StatusServiceUnavailable, "device parse coverage is unavailable")
		return
	}
	// Bind even an alternative backend's response to the authenticated scope.
	out.DeviceID = p.Credential.DeviceID
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "coverage.read", "device", p.Credential.DeviceID, map[string]any{"outcome": "success"}) {
		return
	}
	writeJSON(w, http.StatusOK, out)
}
