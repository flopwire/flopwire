package api

// Identity, audit, rate-limit, rotation, and admin-scope behaviour is ported
// from the CASS-era hardening stack (#6: 031cf6f, 4d2e741, d4eb6a1, 4c3ee87;
// #8: 62d5a64, 004879b). Every handler that reads organization data or
// changes state writes an audit event first and fails closed (503) when the
// audit store is unavailable.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/auth"
	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Config struct {
	Logger   *slog.Logger
	Registry *prometheus.Registry
	// TrustedProxyCIDRs lists reverse proxies whose X-Forwarded-For is
	// believed for rate limiting and audit.
	TrustedProxyCIDRs []string
	// AuthRate limits login, invite claim, and rotation commit per client IP
	// and, independently, per claimed identity. The identity limit is what
	// bounds password guessing: behind a NAT or Docker Desktop's port
	// forwarder every client shares one address, and the IP limit then
	// only caps the whole deployment (docs/runbook.md).
	AuthRate Rate
	// Sync serves the device sync protocol (internal/ingest); nil answers
	// 501.
	Sync SyncServer
	// Parse reports the parse backlog for the admin status.
	Parse ParseStatus
	// Retrieval serves search, find, context, conversation and raw; nil
	// answers 501.
	Retrieval Retrieval
	// Bus serves the message bus (internal/bus); nil answers 501.
	Bus Bus
	// Now is the clock for credential lifetimes; nil is time.Now. Tests
	// move it to check expiry.
	Now func() time.Time
}

// ParseStatus reports pending parses, the backlog at which uploads pause,
// the age of the oldest request, failing and quarantined sources, and
// releases a quarantined source.
type ParseStatus interface {
	Status() (pending, capacity int64, lag time.Duration, failing, quarantined int64)
	Quarantined(ctx context.Context, limit int) ([]domain.QuarantinedSource, error)
	Release(ctx context.Context, sourceID string, audit domain.AuditEvent) (bool, error)
	Redactions(ctx context.Context) (domain.RedactionStatus, error)
	// Refused counts sources refused or purged by an admin path rule.
	Refused(ctx context.Context) (int64, error)
	// Hidden previews the sessions a path-rule change hid (D18).
	Hidden(ctx context.Context) (domain.HiddenSummary, error)
	// PurgeHidden purges the hidden sessions (those under rule, when not
	// ""), as the administrator actor, each re-checked against the current
	// rules (restored instead when nothing covers it).
	PurgeHidden(ctx context.Context, actor, deviceID, rule string) (purged, restored int, err error)
}

// SyncServer serves POST /v1/sync/has and /v1/sync/flush for an
// authenticated device.
type SyncServer interface {
	ServeSync(w http.ResponseWriter, r *http.Request, deviceID string)
}

type API struct {
	store           store.Store
	log             *slog.Logger
	trustedProxies  []netip.Prefix
	ipLimiter       *tokenBuckets
	identityLimiter *tokenBuckets
	sync            SyncServer
	parse           ParseStatus
	retrieval       Retrieval
	retrievalSlots  chan struct{}
	bus             Bus
	clock           func() time.Time
}

func (a *API) now() time.Time { return a.clock().UTC() }

type principal struct {
	User       domain.User
	Credential domain.Credential
}

type principalKey struct{}

func New(s store.Store, cfg Config) *API {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AuthRate.Burst <= 0 {
		cfg.AuthRate.Burst = 10
	}
	if cfg.AuthRate.Refill <= 0 {
		cfg.AuthRate.Refill = time.Minute
	}
	var proxies []netip.Prefix
	for _, raw := range cfg.TrustedProxyCIDRs {
		if p, err := netip.ParsePrefix(strings.TrimSpace(raw)); err == nil {
			proxies = append(proxies, p)
		} else {
			cfg.Logger.Warn("ignoring invalid trusted proxy CIDR", "cidr", raw)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &API{store: s, log: cfg.Logger, trustedProxies: proxies, sync: cfg.Sync, parse: cfg.Parse, clock: cfg.Now,
		retrieval: cfg.Retrieval, bus: cfg.Bus, retrievalSlots: make(chan struct{}, RetrievalConcurrency),
		ipLimiter: newTokenBuckets(cfg.AuthRate), identityLimiter: newTokenBuckets(cfg.AuthRate)}
}

func (a *API) Handler(reg *prometheus.Registry) http.Handler {
	if reg == nil {
		reg = prometheus.NewRegistry()
	}
	r := chi.NewRouter()
	r.Use(SecurityHeaders)
	r.Get("/livez", a.liveness)
	r.Get("/readyz", a.readiness)
	r.Get("/healthz", a.readiness)
	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	ip := func(r *http.Request) string { return a.clientIP(r) }
	r.With(a.rateLimit("login", ip, func(r *http.Request) string { return identityHint(r, "email") })).Post("/v1/login", a.login)
	r.With(a.rateLimit("claim", ip, func(r *http.Request) string { return identityHint(r, "code") })).Post("/v1/invites/claim", a.claimInvite)
	r.With(a.rateLimit("rotation.commit", ip, func(r *http.Request) string { return chi.URLParam(r, "id") })).Post("/v1/device-rotations/{id}/commit", a.commitDeviceRotation)
	r.Group(func(r chi.Router) {
		r.Use(a.authenticate)
		r.Post("/v1/devices", a.createDevice)
		r.Post("/v1/devices/{id}/reauth", a.reauthDevice)
		r.Post("/v1/devices/{id}/rotation/prepare", a.prepareDeviceRotation)
		r.Post("/v1/tokens", a.mintToken)
		r.Get("/v1/policy", a.getPolicy)
		r.Post(syncproto.PathHas, a.syncDevice)
		r.Post(syncproto.PathFlush, a.syncDevice)
		r.Get(syncproto.PathCapabilities, a.syncDevice)
		r.Get(coverage.Path, a.readerOnly(a.deviceCoverage))
		r.Post(syncproto.PathPolicyPlacements, a.syncDevice)
		a.retrievalRoutes(r)
		a.busRoutes(r)
		r.Delete("/v1/conversations/{id}", a.memberOnly(a.deleteOwnConversation))
		r.Post("/v1/conversations/withhold", a.withholdOnly(a.withholdOwnSession))
		r.Get("/v1/deletions/{id}", a.memberOnly(a.getOwnDeletion))
		r.Route("/v1/admin", func(r chi.Router) {
			r.Use(a.requireAdmin)
			r.Post("/invites", a.createInvite)
			r.Get("/users", a.listUsers)
			r.Get("/devices", a.listDevices)
			r.Post("/devices/{id}/revoke", a.revokeDevice)
			r.Post("/users/{id}/revoke", a.revokeUser)
			r.Post("/service-accounts", a.createServiceAccount)
			r.Get("/audit", a.listAudit)
			r.Get("/status", a.adminStatus)
			r.Put("/policy", a.updatePolicy)
			r.Get("/policy/hidden", a.hiddenSessions)
			r.Post("/policy/hidden/purge", a.purgeHidden)
			r.Delete("/conversations/{id}", a.deleteConversation)
			r.Get("/deletions/{id}", a.getDeletion)
			r.Post("/deletions/{id}/retry", a.retryDeletion)
			r.Post("/sources/{id}/reparse", a.releaseSource)
			r.Post("/redactions", a.redactAny)
		})
	})
	return r
}

func (a *API) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !decode(w, r, &in) {
		return
	}
	u, err := a.store.UserByEmail(r.Context(), strings.ToLower(strings.TrimSpace(in.Email)))
	if err != nil || u.Disabled || !auth.CheckPassword(u.PasswordHash, in.Password) {
		if !a.auditOK(w, r, "", "", "auth.failed", "login", "", map[string]any{"reason": "invalid_credentials", "account": hashIdentifier(in.Email), "client_ip": a.clientIP(r)}) {
			return
		}
		problem(w, 401, "invalid email or password")
		return
	}
	plain, c := a.newCredential(u.ID, "", domain.CredentialSession, 24*time.Hour)
	if err = a.store.CreateCredentialWithAudit(r.Context(), c, event(u.ID, "", "login", "user", u.ID, map[string]any{"client_ip": a.clientIP(r)})); err != nil {
		problem(w, 500, "cannot create session")
		return
	}
	writeJSON(w, 200, map[string]any{"user": u, "token": plain, "expires_at": c.ExpiresAt})
}

func (a *API) createInvite(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	var in struct {
		Email string
		Role  domain.Role
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Role == "" {
		in.Role = domain.RoleMember
	}
	if in.Role != domain.RoleMember && in.Role != domain.RoleAdmin {
		problem(w, 400, "role must be admin or member")
		return
	}
	code, hash, err := auth.NewToken()
	if err != nil {
		problem(w, 500, "cannot create invite")
		return
	}
	v := domain.Invite{ID: uuid.NewString(), Email: strings.ToLower(strings.TrimSpace(in.Email)), Role: in.Role, CodeHash: hash, CreatedBy: p.User.ID, ExpiresAt: time.Now().UTC().Add(72 * time.Hour)}
	if v.Email == "" {
		problem(w, 400, "email is required")
		return
	}
	if err = a.store.CreateInviteWithAudit(r.Context(), p.Credential.ID, v, event(p.User.ID, p.Credential.DeviceID, "invite.create", "invite", v.ID, map[string]any{"email": v.Email, "role": v.Role})); err != nil {
		problem(w, 500, "cannot create invite")
		return
	}
	writeJSON(w, 201, map[string]any{"invite": v, "code": code})
}

func (a *API) claimInvite(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code, Name, Password string }
	if !decode(w, r, &in) {
		return
	}
	h := auth.HashToken(in.Code)
	v, err := a.store.InviteByCodeHash(r.Context(), h)
	now := time.Now().UTC()
	if err != nil || !v.ClaimedAt.IsZero() || now.After(v.ExpiresAt) {
		if !a.auditOK(w, r, "", "", "invite.claim.failed", "invite", "", map[string]any{"reason": "invalid_or_expired", "invite": shortHash(h), "client_ip": a.clientIP(r)}) {
			return
		}
		problem(w, 400, "invite is invalid or expired")
		return
	}
	ph, err := auth.HashPassword(in.Password)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	u := domain.User{ID: uuid.NewString(), Email: v.Email, Name: strings.TrimSpace(in.Name), Role: v.Role, IdentityType: domain.IdentityHuman, PasswordHash: ph, CreatedAt: now}
	if u.Name == "" {
		problem(w, 400, "name is required")
		return
	}
	plain, c := a.newCredential(u.ID, "", domain.CredentialSession, 24*time.Hour)
	if err = a.store.ClaimInviteIdentity(r.Context(), h, now, u, c, event(u.ID, "", "invite.claim", "user", u.ID, map[string]any{"invite_id": v.ID})); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			problem(w, 409, "invite already claimed or account exists")
		} else {
			problem(w, 500, "cannot claim invite")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"user": u, "token": plain, "expires_at": c.ExpiresAt})
}

func (a *API) createDevice(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	var in struct{ Name, Platform string }
	if !decode(w, r, &in) {
		return
	}
	if p.Credential.Kind != domain.CredentialSession {
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "device", "", map[string]any{"reason": "login_session_required"}) {
			return
		}
		problem(w, 403, "device enrollment requires a login session")
		return
	}
	now := a.now()
	d := domain.Device{ID: uuid.NewString(), UserID: p.User.ID, Name: strings.TrimSpace(in.Name), Platform: strings.TrimSpace(in.Platform), Kind: domain.DeviceEnrolled, LastSeen: now, CreatedAt: now}
	if d.Name == "" || d.Platform == "" {
		problem(w, 400, "name and platform are required")
		return
	}
	// The credential lives DeviceReauthAfter from this interactive login;
	// rotation keeps the deadline.
	plain, c := a.newCredential(p.User.ID, d.ID, domain.CredentialDevice, domain.DeviceReauthAfter)
	if err := a.store.CreateDeviceWithCredential(r.Context(), p.Credential.ID, d, c, event(p.User.ID, d.ID, "device.enroll", "device", d.ID, nil)); err != nil {
		problem(w, 500, "cannot enroll device")
		return
	}
	writeJSON(w, 201, map[string]any{"device": d, "token": plain, "expires_at": c.ExpiresAt, "idle_expires_at": now.Add(domain.DeviceIdleAfter)})
}

// reauthDevice gives the caller's enrolled device a fresh credential after
// an interactive login (`flopwire login` on an enrolled device): the 90-day
// deadline restarts and every other credential of the device is revoked,
// including one a thief rotated to. The device keeps its id, so its
// sources stay its own. A revoked device cannot come back; enroll a new
// one.
func (a *API) reauthDevice(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	deviceID := chi.URLParam(r, "id")
	if p.Credential.Kind != domain.CredentialSession {
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "device", deviceID, map[string]any{"reason": "login_session_required"}) {
			return
		}
		problem(w, 403, "re-authenticating a device requires a login session")
		return
	}
	if _, err := uuid.Parse(deviceID); err != nil {
		problem(w, 404, "device not found")
		return
	}
	plain, c := a.newCredential(p.User.ID, deviceID, domain.CredentialDevice, domain.DeviceReauthAfter)
	err := a.store.ReauthDevice(r.Context(), p.Credential.ID, c, event(p.User.ID, deviceID, "device.reauth", "device", deviceID, map[string]any{"client_ip": a.clientIP(r)}))
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "device not found")
	case errors.Is(err, store.ErrForbidden):
		problemCode(w, 409, "device_revoked", "this device was revoked or is not an enrolled device; run flopwire enroll")
	case err != nil:
		problem(w, 500, "cannot re-authenticate device")
	default:
		writeJSON(w, 201, map[string]any{"device_id": deviceID, "token": plain, "expires_at": c.ExpiresAt, "idle_expires_at": c.CreatedAt.Add(domain.DeviceIdleAfter)})
	}
}

// mintToken issues a short-lived scoped token (`flopwire token mint`) for a
// sandbox or CI job. The caller is an enrolled device, a service
// identity's device, or a login session; a minted token cannot mint. The
// token belongs to the caller's user, so its uploads are attributed to
// that user (or service identity), and it can grant only scopes the caller
// holds. Its TTL is at most the policy's cap and never outlives the
// minting credential.
func (a *API) mintToken(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	var in struct {
		TTLSeconds int64    `json:"ttl_seconds"`
		Scopes     []string `json:"scopes"`
		Label      string   `json:"label"`
	}
	if !decode(w, r, &in) {
		return
	}
	deny := func(reason, detail string) {
		if a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "credential", "", map[string]any{"reason": reason}) {
			problem(w, 403, detail)
		}
	}
	if p.Credential.Kind == domain.CredentialMinted {
		deny("minted_token_cannot_mint", "a minted token cannot mint another")
		return
	}
	label := strings.TrimSpace(in.Label)
	if label == "" || len(label) > 100 {
		problem(w, 400, "label is required (at most 100 characters)")
		return
	}
	scopes, ok := normalizeScopes(in.Scopes)
	if !ok {
		problem(w, 400, "scopes must name upload, read or both")
		return
	}
	held := p.Credential.EffectiveScopes(p.User.IdentityType)
	if p.Credential.Kind == domain.CredentialSession {
		held = []string{domain.ScopeUpload, domain.ScopeRead} // a person logged in holds both
	}
	for _, s := range scopes {
		if !slices.Contains(held, s) {
			deny("scope_not_held", "cannot grant the "+s+" scope: the minting credential does not hold it")
			return
		}
	}
	policy, err := a.store.GetPolicy(r.Context())
	if err != nil {
		problem(w, 500, "cannot read collection policy")
		return
	}
	limit := policy.MaxTokenTTL()
	ttl := time.Duration(in.TTLSeconds) * time.Second
	switch {
	case in.TTLSeconds < 0:
		problem(w, 400, "ttl must be positive")
		return
	case in.TTLSeconds == 0:
		ttl = min(domain.MintDefaultTTL, limit)
	case in.TTLSeconds > int64(limit/time.Second):
		problemCode(w, 400, "ttl_exceeds_max", fmt.Sprintf("ttl exceeds the maximum of %s (an administrator sets it in the policy)", limit))
		return
	}
	now := a.now()
	plain, c := a.newCredential(p.User.ID, "", domain.CredentialMinted, ttl)
	if !p.Credential.ExpiresAt.IsZero() && c.ExpiresAt.After(p.Credential.ExpiresAt) {
		c.ExpiresAt = p.Credential.ExpiresAt
	}
	c.Scopes, c.Label = scopes, label
	if p.Credential.Kind == domain.CredentialDevice {
		c.MintedFromDevice = p.Credential.DeviceID
	}
	ev := event(p.User.ID, p.Credential.DeviceID, "token.mint", "credential", c.ID, map[string]any{"label": label, "scopes": scopes,
		"ttl_seconds": int64(c.ExpiresAt.Sub(now) / time.Second), "expires_at": c.ExpiresAt, "minted_by_credential": p.Credential.ID})
	if err := a.store.MintCredential(r.Context(), p.Credential.ID, c, ev); err != nil {
		if errors.Is(err, store.ErrConflict) {
			problem(w, 409, "the minting credential is no longer valid")
		} else {
			problem(w, 500, "cannot mint token")
		}
		return
	}
	writeJSON(w, 201, map[string]any{"token": plain, "credential_id": c.ID, "label": label, "scopes": scopes, "expires_at": c.ExpiresAt})
}

// normalizeScopes dedupes scopes into upload-then-read order; false when
// one is unknown or none is named.
func normalizeScopes(in []string) ([]string, bool) {
	var up, rd bool
	for _, s := range in {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case domain.ScopeUpload:
			up = true
		case domain.ScopeRead:
			rd = true
		default:
			return nil, false
		}
	}
	var out []string
	if up {
		out = append(out, domain.ScopeUpload)
	}
	if rd {
		out = append(out, domain.ScopeRead)
	}
	return out, len(out) > 0
}

// prepareDeviceRotation issues an inactive replacement credential and a
// one-time commit token. The client saves both before committing, so a crash
// at any point leaves it with a credential that works.
func (a *API) prepareDeviceRotation(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	deviceID := chi.URLParam(r, "id")
	if p.Credential.Kind != domain.CredentialDevice || p.Credential.DeviceID != deviceID {
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "device", deviceID, map[string]any{"reason": "own_device_credential_required"}) {
			return
		}
		problem(w, 403, "a device can rotate only its own credential")
		return
	}
	// The replacement keeps the old credential's scopes, label and
	// deadline: rotation never extends the 90-day interactive re-login.
	newPlain, next := a.newCredential(p.User.ID, deviceID, domain.CredentialDevice, 0)
	next.Active = false
	next.Scopes, next.Label, next.ExpiresAt = p.Credential.Scopes, p.Credential.Label, p.Credential.ExpiresAt
	commitPlain, commitHash, err := auth.NewToken()
	if err != nil {
		problem(w, 500, "cannot prepare rotation")
		return
	}
	now := a.now()
	rotation := domain.DeviceRotation{ID: uuid.NewString(), DeviceID: deviceID, OldCredentialID: p.Credential.ID, NewCredentialID: next.ID, CommitTokenHash: commitHash, State: domain.DeviceRotationPrepared, ExpiresAt: now.Add(15 * time.Minute), CreatedAt: now}
	ev := event(p.User.ID, deviceID, "device.rotation.prepare", "device", deviceID, map[string]any{"rotation_id": rotation.ID, "expires_at": rotation.ExpiresAt})
	if err = a.store.PrepareDeviceRotation(r.Context(), rotation, next, ev); err != nil {
		problem(w, 409, "cannot prepare device rotation")
		return
	}
	out := map[string]any{"rotation_id": rotation.ID, "token": newPlain, "commit_token": commitPlain, "expires_at": rotation.ExpiresAt,
		"idle_expires_at": now.Add(domain.DeviceIdleAfter)}
	if !next.ExpiresAt.IsZero() {
		out["credential_expires_at"] = next.ExpiresAt
	}
	writeJSON(w, 201, out)
}

func (a *API) commitDeviceRotation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CommitToken string `json:"commit_token"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.CommitToken == "" {
		problem(w, 400, "commit_token is required")
		return
	}
	id := chi.URLParam(r, "id")
	ev := event("", "", "device.rotation.commit", "device", "", map[string]any{"rotation_id": id})
	rotation, err := a.store.CommitDeviceRotation(r.Context(), id, auth.HashToken(in.CommitToken), a.now(), ev)
	if err != nil {
		invalid := errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict)
		reason := "internal"
		if invalid {
			reason = "invalid_or_expired"
		}
		if !a.auditOK(w, r, "", "", "device.rotation.commit.failed", "rotation", id, map[string]any{"reason": reason, "client_ip": a.clientIP(r)}) {
			return
		}
		if invalid {
			problemCode(w, 400, "rotation_invalid_or_expired", "rotation is invalid or expired")
		} else {
			problem(w, 500, "cannot commit rotation")
		}
		return
	}
	writeJSON(w, 200, map[string]any{"rotation_id": rotation.ID, "device_id": rotation.DeviceID, "state": rotation.State, "committed_at": rotation.CommittedAt})
}

// syncDevice hands a sync request to the ingest server. Only a credential
// with the upload scope and a device syncs (an enrolled device, a service
// identity, or a minted upload token's ephemeral device); the device id
// comes from the credential, never from the request.
func (a *API) syncDevice(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	if p.Credential.Kind == domain.CredentialSession || p.Credential.DeviceID == "" {
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "device_credential_required"}) {
			return
		}
		problem(w, 403, "sync requires a device credential")
		return
	}
	if !p.Credential.Allows(domain.ScopeUpload, p.User.IdentityType) {
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "scope_required", "scope": domain.ScopeUpload}) {
			return
		}
		problemCode(w, 403, "scope_required", "this credential lacks the upload scope")
		return
	}
	if a.sync == nil {
		problem(w, http.StatusNotImplemented, "this server does not accept uploads")
		return
	}
	a.sync.ServeSync(w, r, p.Credential.DeviceID)
}

func (a *API) liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "alive"})
}

// readiness probes Postgres and object storage when the store can. Search
// is reported unavailable on a server without a message index; that does
// not make the server unready.
func (a *API) readiness(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"status": "ok", "database": "ready", "object_store": "ready", "search": "unavailable"}
	if a.retrieval != nil {
		out["search"] = "ready"
	}
	code := http.StatusOK
	if checker, ok := a.store.(interface {
		CheckDependencies(context.Context) store.DependencyStatus
	}); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		deps := checker.CheckDependencies(ctx)
		if deps.Database != nil {
			out["status"], out["database"], code = "not_ready", "unavailable", http.StatusServiceUnavailable
		}
		if deps.ObjectStore != nil {
			out["status"], out["object_store"], code = "not_ready", "unavailable", http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, out)
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	u, err := a.store.ListUsers(r.Context())
	if err != nil {
		problem(w, 500, "cannot list users")
		return
	}
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "users.list", "user", "", map[string]any{"outcome": "success", "count": len(u)}) {
		return
	}
	writeJSON(w, 200, map[string]any{"users": u})
}
func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.ListDevices(r.Context())
	if err != nil {
		problem(w, 500, "cannot list devices")
		return
	}
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "devices.list", "device", "", map[string]any{"outcome": "success", "count": len(d)}) {
		return
	}
	writeJSON(w, 200, map[string]any{"devices": d})
}
func (a *API) revokeDevice(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	id := chi.URLParam(r, "id")
	now := a.now()
	if err := a.store.RevokeDeviceWithAudit(r.Context(), p.Credential.ID, id, now, event(p.User.ID, p.Credential.DeviceID, "device.revoke", "device", id, nil)); err != nil {
		problem(w, 404, "device not found or already revoked")
		return
	}
	writeJSON(w, 200, map[string]any{"device_id": id, "revoked_at": now})
}

// revokeUser (admin) revokes every credential a user or service identity
// holds or minted, and every device it owns. The account stays, so a
// person can log in and enroll again.
func (a *API) revokeUser(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		problem(w, 404, "user not found")
		return
	}
	now := a.now()
	creds, devices, err := a.store.RevokeUserWithAudit(r.Context(), p.Credential.ID, id, now, event(p.User.ID, p.Credential.DeviceID, "principal.revoke", "user", id, nil))
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "user not found")
	case errors.Is(err, store.ErrConflict):
		problem(w, 409, "the administrator session is no longer valid")
	case err != nil:
		problem(w, 500, "cannot revoke user")
	default:
		writeJSON(w, 200, map[string]any{"user_id": id, "revoked_credentials": creds, "revoked_devices": devices, "revoked_at": now})
	}
}
func (a *API) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		problem(w, 400, "name is required")
		return
	}
	now := time.Now().UTC()
	userID := uuid.NewString()
	u := domain.User{ID: userID, Email: "service-" + userID + "@flopwire.invalid", Name: in.Name, Role: domain.RoleMember, IdentityType: domain.IdentityService, CreatedAt: now}
	d := domain.Device{ID: uuid.NewString(), UserID: u.ID, Name: in.Name + " service", Platform: "service", Kind: domain.DeviceService, CreatedAt: now}
	plain, c := a.newCredential(u.ID, d.ID, domain.CredentialDevice, 0)
	if err := a.store.CreateServiceIdentity(r.Context(), p.Credential.ID, u, d, c, event(p.User.ID, p.Credential.DeviceID, "service.create", "user", u.ID, map[string]any{"device_id": d.ID})); err != nil {
		problem(w, 500, "cannot create service account")
		return
	}
	writeJSON(w, 201, map[string]any{"user": u, "device": d, "token": plain, "scope": "upload-only"})
}
func (a *API) listAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := a.store.ListAudit(r.Context(), limit)
	if err != nil {
		problem(w, 500, "cannot list audit events")
		return
	}
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "audit.list", "audit", "", map[string]any{"limit": limit, "outcome": "success", "count": len(events)}) {
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

func (a *API) adminStatus(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	policy, err := a.store.GetPolicy(r.Context())
	if err != nil {
		problem(w, 500, "cannot read collection policy")
		return
	}
	total, user, err := a.store.Usage(r.Context(), p.User.ID)
	if err != nil {
		problem(w, 500, "cannot read storage usage")
		return
	}
	events, err := a.store.ListAudit(r.Context(), 500)
	if err != nil {
		problem(w, 500, "cannot read operational history")
		return
	}
	var latest, backup *domain.AuditEvent
	for i := range events {
		if latest == nil && events[i].Action != "audit.list" && events[i].Action != "status.read" {
			latest = &events[i]
		}
		if backup == nil && events[i].Action == "backup.complete" {
			backup = &events[i]
		}
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "status.read", "status", "deployment", map[string]any{"outcome": "success"}) {
		return
	}
	auditCount, err := a.store.CountAudit(r.Context())
	if err != nil {
		problem(w, 500, "cannot count audit history")
		return
	}
	diagnostics := map[string]any{"queue_depth": 0, "queue_capacity": 0}
	if backend, ok := a.retrieval.(diagnosticReader); ok {
		summary, err := backend.ExtractionSummary(r.Context())
		if err != nil {
			problem(w, 500, "cannot read extraction diagnostics")
			return
		}
		diagnostics["extraction"] = summary
	}
	if a.parse != nil {
		pending, capacity, lag, failing, quarantined := a.parse.Status()
		refused, err := a.parse.Refused(r.Context())
		if err != nil {
			problem(w, 500, "cannot count refused sources")
			return
		}
		hidden, err := a.parse.Hidden(r.Context())
		if err != nil {
			problem(w, 500, "cannot count hidden sessions")
			return
		}
		extraction := diagnostics["extraction"]
		diagnostics = map[string]any{"queue_depth": pending, "queue_capacity": capacity, "parse_lag_seconds": int64(lag.Seconds()),
			"failing_sources": failing, "quarantined_sources": quarantined, "refused_sources": refused, "hidden_sessions": hidden.Total}
		if extraction != nil {
			diagnostics["extraction"] = extraction
		}
		if quarantined > 0 {
			list, err := a.parse.Quarantined(r.Context(), 20)
			if err != nil {
				problem(w, 500, "cannot list quarantined sources")
				return
			}
			diagnostics["quarantined"] = list
		}
	}
	var redaction any
	if a.parse != nil {
		rs, err := a.parse.Redactions(r.Context())
		if err != nil {
			problem(w, 500, "cannot read redaction counts")
			return
		}
		redaction = rs
	}
	writeJSON(w, 200, map[string]any{
		"storage": map[string]any{"used_bytes": total, "admin_bytes": user, "quota_bytes": policy.MaxStorageBytes},
		"index":   diagnostics, "last_transition": latest, "last_backup": backup,
		"audit":     map[string]any{"retention": "indefinite", "event_count": auditCount},
		"redaction": redaction,
	})
}
func (a *API) getPolicy(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	policy, err := a.store.GetPolicy(r.Context())
	if err != nil {
		problem(w, 500, "cannot read collection policy")
		return
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "policy.read", "policy", "collection", map[string]any{"outcome": "success"}) {
		return
	}
	writeJSON(w, 200, policy)
}
func (a *API) updatePolicy(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	var in struct {
		MaxStorageBytes int64    `json:"max_storage_bytes"`
		MaxUserBytes    int64    `json:"max_user_bytes"`
		PathRules       []string `json:"path_rules"`
		Unplaceable     string   `json:"unplaceable"`
		// MaxTokenTTLSeconds, when present, sets the cap on minted
		// tokens (0: the default); absent keeps the current cap.
		MaxTokenTTLSeconds *int64 `json:"max_token_ttl_seconds"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.MaxStorageBytes < 0 || in.MaxUserBytes < 0 || (in.MaxTokenTTLSeconds != nil && *in.MaxTokenTTLSeconds < 0) {
		problem(w, 400, "quota and TTL values must be zero or positive")
		return
	}
	// A minted token never outlives what an enrolled device could; the
	// bound also keeps the cap inside a time.Duration.
	if in.MaxTokenTTLSeconds != nil && *in.MaxTokenTTLSeconds > int64(domain.DeviceReauthAfter/time.Second) {
		problem(w, 400, fmt.Sprintf("max_token_ttl_seconds must be at most %d (90 days)", int64(domain.DeviceReauthAfter/time.Second)))
		return
	}
	current, err := a.store.GetPolicy(r.Context())
	if err != nil {
		problem(w, 500, "cannot read collection policy")
		return
	}
	maxTTL := current.MaxTokenTTLSeconds
	if in.MaxTokenTTLSeconds != nil {
		maxTTL = *in.MaxTokenTTLSeconds
	}
	unplaceable := strings.ToLower(strings.TrimSpace(in.Unplaceable))
	if _, ok := pathpolicy.ParseUnplaceable(unplaceable); unplaceable != "" && !ok {
		problem(w, 400, "unplaceable must be local, upload, exclude or empty")
		return
	}
	clean := pathpolicy.NormalizeRules(in.PathRules)
	policy := domain.Policy{MaxStorageBytes: in.MaxStorageBytes, MaxUserBytes: in.MaxUserBytes, PathRules: clean, Unplaceable: unplaceable, MaxTokenTTLSeconds: maxTTL, UpdatedBy: p.User.ID, UpdatedAt: time.Now().UTC()}
	if err := a.store.UpdatePolicyWithAudit(r.Context(), p.Credential.ID, policy, event(p.User.ID, p.Credential.DeviceID, "policy.update", "policy", "collection", map[string]any{"max_storage_bytes": policy.MaxStorageBytes, "max_user_bytes": policy.MaxUserBytes, "path_rules": len(clean), "unplaceable": unplaceable, "max_token_ttl_seconds": maxTTL})); err != nil {
		problem(w, 500, "cannot update collection policy")
		return
	}
	writeJSON(w, 200, policy)
}

// hiddenSessions (admin) previews the stored sessions a path-rule change
// hid (D18): how many per user and per rule, and when the oldest is
// purged unless a rule change restores it.
func (a *API) hiddenSessions(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	if a.parse == nil {
		problem(w, http.StatusNotImplemented, "this server does not parse")
		return
	}
	h, err := a.parse.Hidden(r.Context())
	if err != nil {
		problem(w, 500, "cannot summarize hidden sessions")
		return
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "policy.hidden.read", "policy", "collection", map[string]any{"total": h.Total}) {
		return
	}
	writeJSON(w, 200, h)
}

// purgeHidden (admin) confirms the purge of hidden sessions now (all, or
// those one rule hid) through the deletion machinery. The body must say
// {"confirm": true}.
func (a *API) purgeHidden(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	if a.parse == nil {
		problem(w, http.StatusNotImplemented, "this server does not parse")
		return
	}
	var in struct {
		Rule    string `json:"rule"`
		Confirm bool   `json:"confirm"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !in.Confirm {
		problem(w, 400, "purging hidden sessions deletes them for good; send confirm: true")
		return
	}
	purged, restored, err := a.parse.PurgeHidden(r.Context(), p.User.ID, p.Credential.DeviceID, strings.TrimSpace(in.Rule))
	if err != nil {
		a.log.Error("purge hidden sessions", "err", err)
		problem(w, 500, "cannot purge hidden sessions")
		return
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "policy.hidden.purge", "policy", "collection",
		map[string]any{"rule": in.Rule, "purged": purged, "restored": restored}) {
		return
	}
	writeJSON(w, 200, map[string]any{"purged": purged, "restored": restored})
}

// deleteConversation (admin) tombstones any conversation and queues the
// purge of its raw evidence; deleteOwnConversation is the member's route
// for their own. Both answer 202 with the job; repeating returns the same
// job. Deletion is always a person's request: nothing deletes on a
// schedule (D9).
func (a *API) deleteConversation(w http.ResponseWriter, r *http.Request) {
	a.requestDeletion(w, r, false, "/v1/admin/deletions/")
}

func (a *API) deleteOwnConversation(w http.ResponseWriter, r *http.Request) {
	a.requestDeletion(w, r, true, "/v1/deletions/")
}

func (a *API) requestDeletion(w http.ResponseWriter, r *http.Request, ownerOnly bool, location string) {
	p := mustPrincipal(r)
	deletions, ok := a.store.(store.DeletionStore)
	if !ok {
		problem(w, http.StatusNotImplemented, "this store does not hold conversations")
		return
	}
	id := chi.URLParam(r, "id")
	job, err := deletions.RequestConversationDeletion(r.Context(), id, p.User.ID, p.Credential.DeviceID, ownerOnly)
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "conversation not found")
		return
	case errors.Is(err, store.ErrForbidden):
		if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "conversation", id, map[string]any{"reason": "not_owner"}) {
			return
		}
		problem(w, 403, "only the conversation's owner or an administrator can delete it")
		return
	case err != nil:
		problem(w, 500, "cannot queue conversation deletion")
		return
	}
	w.Header().Set("Location", location+job.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"deletion": job})
}

// withholdOwnSession is the member's self-delete addressed by session:
// the device agent calls it when a directory a session named later moved
// the session to local or deny under a path rule after some of it was
// uploaded. It deletes the member's conversation of the session like
// deleteOwnConversation (subagents, every device, tombstone), or
// tombstones a session whose uploads are not parsed yet, and audits
// conversation.withheld with the rule. Acceptance records a tombstone even
// before the first upload commits.
func (a *API) withholdOwnSession(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	deletions, ok := a.store.(store.DeletionStore)
	if !ok {
		problem(w, http.StatusNotImplemented, "this store does not hold conversations")
		return
	}
	var in struct {
		Agent     string `json:"agent"`
		SessionID string `json:"session_id"`
		Mode      string `json:"mode"`
		Rule      string `json:"rule"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Agent == "" || in.SessionID == "" || len(in.SessionID) > 1024 || len(in.Rule) > 4096 {
		problem(w, 400, "agent and session_id are required")
		return
	}
	if in.Mode != "local" && in.Mode != "deny" {
		problem(w, 400, "mode must be local or deny")
		return
	}
	var job domain.DeletionJob
	var conv string
	var err error
	scope := "user"
	if p.Credential.Kind == domain.CredentialMinted || p.User.IdentityType == domain.IdentityService || !p.Credential.Allows(domain.ScopeRead, p.User.IdentityType) {
		scope = "device"
		job, conv, err = deletions.WithholdDeviceSession(r.Context(), p.User.ID, p.Credential.DeviceID, in.Agent, in.SessionID)
	} else {
		job, conv, err = deletions.WithholdSession(r.Context(), p.User.ID, p.Credential.DeviceID, in.Agent, in.SessionID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "no conversation of this session is stored")
		return
	case errors.Is(err, store.ErrForbidden):
		problem(w, 403, "only the conversation's owner or an administrator can delete it")
		return
	case err != nil:
		a.log.Error("withhold session", "err", err)
		problem(w, 500, "cannot queue conversation deletion")
		return
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "conversation.withheld", "conversation", conv,
		map[string]any{"agent": in.Agent, "session_id": in.SessionID, "mode": in.Mode, "rule": in.Rule, "job_id": job.ID, "scope": scope}) {
		return
	}
	var out any
	if job.ID != "" {
		w.Header().Set("Location", "/v1/deletions/"+job.ID)
		out = job
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"deletion": out})
}

func (a *API) getDeletion(w http.ResponseWriter, r *http.Request) { a.readDeletion(w, r, false) }

// getOwnDeletion shows a member the jobs they requested.
func (a *API) getOwnDeletion(w http.ResponseWriter, r *http.Request) { a.readDeletion(w, r, true) }

func (a *API) readDeletion(w http.ResponseWriter, r *http.Request, own bool) {
	p := mustPrincipal(r)
	deletions, ok := a.store.(store.DeletionStore)
	if !ok {
		problem(w, 404, "deletion job not found")
		return
	}
	job, err := deletions.DeletionJobByID(r.Context(), chi.URLParam(r, "id"))
	if err == nil && own && job.RequestedBy != p.User.ID {
		err = store.ErrNotFound
	}
	if errors.Is(err, store.ErrNotFound) {
		problem(w, 404, "deletion job not found")
		return
	}
	if err != nil {
		problem(w, 500, "cannot read deletion job")
		return
	}
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "conversation.deletion.read", "conversation", job.ConversationID, map[string]any{"job_id": job.ID, "state": job.State}) {
		return
	}
	writeJSON(w, 200, map[string]any{"deletion": job})
}

func (a *API) retryDeletion(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	deletions, ok := a.store.(store.DeletionStore)
	if !ok {
		problem(w, 404, "deletion job not found")
		return
	}
	job, err := deletions.RetryDeletionJob(r.Context(), chi.URLParam(r, "id"), p.User.ID, p.Credential.DeviceID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		problem(w, 404, "deletion job not found")
	case errors.Is(err, store.ErrConflict):
		problem(w, 409, "only failed deletion jobs can be retried")
	case err != nil:
		problem(w, 500, "cannot retry deletion job")
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"deletion": job})
	}
}

// releaseSource lifts a source's parse quarantine and asks for a full
// re-parse (after a parser fix, say).
func (a *API) releaseSource(w http.ResponseWriter, r *http.Request) {
	p := mustPrincipal(r)
	if a.parse == nil {
		problem(w, http.StatusNotImplemented, "this server does not parse")
		return
	}
	id := chi.URLParam(r, "id")
	released, err := a.parse.Release(r.Context(), id, event(p.User.ID, p.Credential.DeviceID, "source.parse.released", "source", id, nil))
	if err != nil {
		problem(w, 500, "cannot release source")
		return
	}
	if !released {
		problem(w, 404, "source is not quarantined")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"source_id": id, "reparse": true})
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, "Bearer ") {
			if !a.auditOK(w, r, "", "", "auth.failed", "request", "", map[string]any{"reason": "missing_bearer", "path": r.URL.Path, "client_ip": a.clientIP(r)}) {
				return
			}
			problem(w, 401, "bearer token required")
			return
		}
		c, err := a.store.CredentialByTokenHash(r.Context(), auth.HashToken(strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))))
		now := a.now()
		reject := func(actor, device, reason, code, detail string, meta map[string]any) {
			meta = withMeta(withMeta(withMeta(meta, "reason", reason), "path", r.URL.Path), "client_ip", a.clientIP(r))
			if a.auditOK(w, r, actor, device, "auth.failed", "request", "", meta) {
				problemCode(w, 401, code, detail)
			}
		}
		if err != nil || !c.Active {
			reject("", "", "invalid_or_expired_credential", "credential_invalid", "credential is invalid or expired", nil)
			return
		}
		if !c.RevokedAt.IsZero() {
			rejectRevoked(c, now, reject)
			return
		}
		if !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
			code := "credential_expired"
			if c.Kind == domain.CredentialDevice {
				code = "reauth_required"
			}
			reject(c.UserID, c.DeviceID, "credential_expired", code, "credential expired", map[string]any{"credential_id": c.ID})
			return
		}
		u, err := a.store.UserByID(r.Context(), c.UserID)
		if err != nil || u.Disabled {
			reject(c.UserID, c.DeviceID, "disabled_or_missing_user", "credential_invalid", "user is disabled or missing", nil)
			return
		}
		if c.Kind == domain.CredentialMinted && c.DeviceID == "" {
			if c.DeviceID, err = a.registerEphemeral(r.Context(), c, now); err != nil {
				a.log.Error("register ephemeral device", "error_class", fmt.Sprintf("%T", err))
				problem(w, 503, "cannot register this token's device")
				return
			}
		}
		if c.DeviceID != "" {
			d, err := a.store.DeviceByID(r.Context(), c.DeviceID)
			if err != nil || !d.RevokedAt.IsZero() {
				reject(c.UserID, c.DeviceID, "device_revoked", "credential_revoked", "device was revoked", nil)
				return
			}
			seen := d.LastSeen
			if seen.IsZero() {
				seen = d.CreatedAt
			}
			if d.Kind == domain.DeviceEnrolled && c.Kind == domain.CredentialDevice && !now.Before(seen.Add(domain.DeviceIdleAfter)) {
				reject(c.UserID, c.DeviceID, "device_idle", "reauth_required", "device was idle too long; log in again", map[string]any{"last_seen_at": seen})
				return
			}
			if ip := a.clientIP(r); now.Sub(d.LastSeen) >= touchEvery || d.LastIP != ip {
				if err := a.store.TouchDevice(r.Context(), d.ID, now, ip); err != nil {
					a.log.Warn("touch device", "error_class", fmt.Sprintf("%T", err))
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal{User: u, Credential: c})))
	})
}

// touchEvery bounds how often a device's last_seen_at is written.
const touchEvery = time.Minute

// rotationGrace: a rotated-away credential presented this long after its
// rotation is audited as reuse. Inside it, an in-flight request of the
// device's own agent is the likely sender.
const rotationGrace = 2 * time.Minute

// rejectRevoked answers a revoked credential with a code that tells the
// agent why, so it can say "re-login required". A rotated-away device
// credential presented after the grace means two holders of one token:
// the audit reason (rotated_credential_reused) names the device so an
// administrator can look.
func rejectRevoked(c domain.Credential, now time.Time, reject func(actor, device, reason, code, detail string, meta map[string]any)) {
	meta := map[string]any{"credential_id": c.ID, "revoke_reason": c.RevokeReason}
	switch c.RevokeReason {
	case domain.RevokeRotated:
		reason := "rotated_credential"
		if now.Sub(c.RevokedAt) > rotationGrace {
			reason = "rotated_credential_reused"
		}
		reject(c.UserID, c.DeviceID, reason, "credential_rotated", "credential was rotated; log in again", meta)
	case domain.RevokeExpired, domain.RevokeIdle:
		code := "reauth_required"
		if c.Kind != domain.CredentialDevice {
			code = "credential_expired"
		}
		reject(c.UserID, c.DeviceID, "credential_expired", code, "credential expired", meta)
	default:
		reject(c.UserID, c.DeviceID, "credential_revoked", "credential_revoked", "credential was revoked", meta)
	}
}

// registerEphemeral creates the ephemeral device of a minted token on its
// first use. The device carries the token's label and belongs to the
// minting user.
func (a *API) registerEphemeral(ctx context.Context, c domain.Credential, now time.Time) (string, error) {
	d := domain.Device{ID: uuid.NewString(), UserID: c.UserID, Name: c.Label, Platform: "ephemeral", Kind: domain.DeviceEphemeral, Label: c.Label, LastSeen: now, CreatedAt: now}
	return a.store.RegisterEphemeralDevice(ctx, c.ID, d, event(c.UserID, d.ID, "device.register", "device", d.ID, map[string]any{"credential_id": c.ID, "label": c.Label, "scopes": c.Scopes}))
}

func withMeta(m map[string]any, k string, v any) map[string]any {
	if m == nil {
		m = map[string]any{}
	}
	m[k] = v
	return m
}

// requireAdmin admits an administrator's login session only. A device
// credential never carries administrative authority, even for an admin.
func (a *API) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		reason := ""
		if p.User.Role != domain.RoleAdmin {
			reason = "admin_required"
		} else if p.Credential.Kind != domain.CredentialSession {
			reason = "admin_session_required"
		}
		if reason != "" {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": reason}) {
				return
			}
			problem(w, 403, "administrator login session required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readerOnly admits a credential with the read scope: not an upload-only
// service identity or minted token.
func (a *API) readerOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		if p.User.IdentityType == domain.IdentityService {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "service_upload_only"}) {
				return
			}
			problem(w, 403, "service accounts are upload-only")
			return
		}
		if !p.Credential.Allows(domain.ScopeRead, p.User.IdentityType) {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "scope_required", "scope": domain.ScopeRead}) {
				return
			}
			problemCode(w, 403, "scope_required", "this credential lacks the read scope")
			return
		}
		next(w, r)
	}
}

// Withholding is upload control. An upload token can affect only its bound
// device; the handler chooses device-scoped deletion for delegated callers.
func (a *API) withholdOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		if p.Credential.DeviceID == "" || p.Credential.Kind == domain.CredentialSession || !p.Credential.Allows(domain.ScopeUpload, p.User.IdentityType) {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "upload_device_required"}) {
				return
			}
			problemCode(w, 403, "scope_required", "withhold requires a device credential with upload scope")
			return
		}
		next(w, r)
	}
}

// memberOnly admits a member's own device or login session: a reader that
// is not a minted token. Deleting a conversation is the person's act, not
// a sandbox's.
func (a *API) memberOnly(next http.HandlerFunc) http.HandlerFunc {
	return a.readerOnly(func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		if p.Credential.Kind == domain.CredentialMinted {
			if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "authorization.failed", "request", r.URL.Path, map[string]any{"reason": "minted_token"}) {
				return
			}
			problem(w, 403, "a minted token cannot delete or redact conversations")
			return
		}
		next(w, r)
	})
}
func mustPrincipal(r *http.Request) principal { return r.Context().Value(principalKey{}).(principal) }

func (a *API) newCredential(userID, deviceID string, kind domain.CredentialKind, ttl time.Duration) (string, domain.Credential) {
	plain, hash, _ := auth.NewToken()
	now := a.now()
	c := domain.Credential{ID: uuid.NewString(), UserID: userID, DeviceID: deviceID, Kind: kind, TokenHash: hash, CreatedAt: now, Active: true}
	if ttl > 0 {
		c.ExpiresAt = now.Add(ttl)
	}
	return plain, c
}
func event(actor, device, action, targetType, targetID string, meta map[string]any) domain.AuditEvent {
	return domain.AuditEvent{ID: uuid.NewString(), ActorID: actor, DeviceID: device, Action: action, TargetType: targetType, TargetID: targetID, Metadata: meta, CreatedAt: time.Now().UTC()}
}

// auditOK appends an audit event and, when that fails, answers 503 and
// returns false. The log records only the error's type: audit errors can
// echo request values.
func (a *API) auditOK(w http.ResponseWriter, r *http.Request, actor, device, action, targetType, targetID string, meta map[string]any) bool {
	if err := a.store.AppendAudit(r.Context(), event(actor, device, action, targetType, targetID, meta)); err != nil {
		a.log.Error("audit append failed", "action", action, "error_class", fmt.Sprintf("%T", err))
		problem(w, 503, "audit service unavailable")
		return false
	}
	return true
}

// clientIP is the peer address, or the first X-Forwarded-For hop when the
// peer is a trusted proxy.
func (a *API) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	for _, prefix := range a.trustedProxies {
		if prefix.Contains(peer) {
			if raw := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); raw != "" {
				if forwarded, err := netip.ParseAddr(raw); err == nil {
					return forwarded.String()
				}
			}
			break
		}
	}
	return peer.String()
}
func hashIdentifier(raw string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(raw))))
	return hex.EncodeToString(sum[:])
}
func shortHash(raw string) string { return hashIdentifier(raw)[:16] }

func (a *API) rateLimit(scope string, ipKey, identityKey func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ipAllowed, identityAllowed := admitDimensions(a.ipLimiter, scope+":"+ipKey(r), a.identityLimiter, scope+":"+identityKey(r))
			if !ipAllowed || !identityAllowed {
				dimensions := []string{}
				if !ipAllowed {
					dimensions = append(dimensions, "ip")
				}
				if !identityAllowed {
					dimensions = append(dimensions, "identity")
				}
				if !a.auditOK(w, r, "", "", "rate_limit.reject", "request", r.URL.Path, map[string]any{"scope": scope, "dimensions": dimensions, "client_ip": a.clientIP(r)}) {
					return
				}
				w.Header().Set("Retry-After", "60")
				problem(w, 429, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		problem(w, 400, "invalid JSON body")
		return false
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		problem(w, 400, "JSON body must contain one value")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
}
func problemCode(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "code": code, "detail": detail})
}
