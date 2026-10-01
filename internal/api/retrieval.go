package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/go-chi/chi/v5"
)

// Retrieval answers the retrieval tools (spec §7.3); internal/retrieval
// implements it over Postgres and the chunk archive.
//
// Every call runs under the query budget of its context's deadline (the
// request's timeout parameter, default DefaultBudget, at most MaxBudget).
// Grep and Search answer a truncated page, never an error, when it runs
// out; the others fail with an error wrapping context.DeadlineExceeded.
type Retrieval interface {
	Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error)
	Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error)
	// Sessions lists one page of sessions after cursor (format.SessionCursor).
	Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error)
	// Read resolves an address (a path address prefers the caller's
	// device) and returns the message with its neighbours.
	Read(ctx context.Context, deviceID string, q format.ReadQuery, f format.Filters) (*format.Context, error)
	Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, format.Attribution, error)
	// RawByPath resolves the source by the caller's device and the path and
	// file id the device knows it by; generation -1 is the latest.
	RawByPath(ctx context.Context, deviceID, path, fileID string, generation, offset, length int64) ([]byte, format.Attribution, error)
	// RawAt returns the transcript bytes of the message an address names.
	RawAt(ctx context.Context, deviceID, address string) ([]byte, format.Attribution, error)
	// RedactMessage masks a message after the fact (notes/redaction.md):
	// the owner's own messages, or any with admin.
	RedactMessage(ctx context.Context, userID, deviceID string, admin bool, req format.RedactRequest) (format.RedactResult, error)
}

// RetrievalConcurrency bounds retrieval requests served at once; the rest
// are refused with 429 rather than queued behind slow queries. The
// database pool is sized above it (store.PoolConfig).
const RetrievalConcurrency = 16

// DefaultBudget and MaxBudget bound one retrieval request (D8); the
// timeout parameter picks a budget in between.
const (
	DefaultBudget = 10 * time.Second
	MaxBudget     = 60 * time.Second
)

func (a *API) retrievalRoutes(r chi.Router) {
	r.Get("/v1/search", a.readerOnly(a.limited(a.search)))
	r.Get("/v1/grep", a.readerOnly(a.limited(a.grep)))
	r.Get("/v1/sessions", a.readerOnly(a.limited(a.sessions)))
	r.Get("/v1/read", a.readerOnly(a.limited(a.read)))
	r.Get("/v1/diagnostics", a.readerOnly(a.limited(a.diagnostics)))
	r.Get("/v1/raw", a.readerOnly(a.limited(a.raw)))
	r.Post("/v1/redactions", a.memberOnly(a.limited(a.redactOwn)))
}

// redactOwn and redactAny mask a message after the fact: the caller's
// own, or (admin route) anyone's. The redaction audits itself in its
// transaction.
func (a *API) redactOwn(w http.ResponseWriter, r *http.Request) { a.redactMessage(w, r, false) }
func (a *API) redactAny(w http.ResponseWriter, r *http.Request) { a.redactMessage(w, r, true) }

func (a *API) redactMessage(w http.ResponseWriter, r *http.Request, admin bool) {
	if a.retrieval == nil {
		problem(w, 501, "retrieval is not configured")
		return
	}
	var req format.RedactRequest
	if !decode(w, r, &req) {
		return
	}
	p := mustPrincipal(r)
	res, err := a.retrieval.RedactMessage(r.Context(), p.User.ID, p.Credential.DeviceID, admin, req)
	if err != nil {
		a.retrievalFailed(w, r, "message.redaction", "message", req.Address, map[string]any{"all_copies": req.AllCopies, "by_admin": admin}, err)
		return
	}
	writeJSON(w, 200, res)
}

// limited applies the retrieval concurrency limit and answers 501 when the
// server has no retrieval backend.
func (a *API) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.retrieval == nil {
			problem(w, http.StatusNotImplemented, "this server has no message index")
			return
		}
		select {
		case a.retrievalSlots <- struct{}{}:
			defer func() { <-a.retrievalSlots }()
		default:
			w.Header().Set("Retry-After", "1")
			problem(w, http.StatusTooManyRequests, "too many retrieval requests in flight")
			return
		}
		next(w, r)
	}
}

func (a *API) filters(w http.ResponseWriter, r *http.Request) (format.Filters, bool) {
	f, err := format.ParseFilters(r.URL.Query())
	if err != nil {
		problem(w, 400, err.Error())
		return f, false
	}
	// Self (session "self") matches only the caller's own sessions.
	f.Owner = mustPrincipal(r).User.ID
	return f, true
}

// budget parses the timeout parameter: a duration ("30s") or seconds,
// clamped to MaxBudget; DefaultBudget when absent.
func budget(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("timeout")
	if v == "" {
		return DefaultBudget, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.ParseFloat(v, 64)
		if nerr != nil {
			return 0, fmt.Errorf("timeout: bad value %q", v)
		}
		d = time.Duration(n * float64(time.Second))
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout: must be positive")
	}
	return min(d, MaxBudget), nil
}

// budgetContext is the request's context bounded by its budget (see
// budget); a bad timeout parameter is answered with 400.
func budgetContext(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc, bool) {
	b, err := budget(r)
	if err != nil {
		problem(w, 400, err.Error())
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), b)
	return ctx, cancel, true
}

// retrievalFailed maps a backend error, auditing the failed attempt.
func (a *API) retrievalFailed(w http.ResponseWriter, r *http.Request, action, targetType, target string, meta map[string]any, err error) {
	status, detail := 500, "retrieval failed"
	var retryable interface{ Retryable() bool }
	switch {
	case errors.Is(err, format.ErrNotFound):
		status, detail = 404, "not found"
	case errors.Is(err, format.ErrBadRequest):
		status, detail = 400, strings.TrimPrefix(err.Error(), "retrieval: bad request: ")
	case errors.Is(err, format.ErrForbidden):
		status, detail = 403, "only the owner or an admin can redact this message"
	case errors.As(err, &retryable) && retryable.Retryable():
		status, detail = 503, "archive is busy or changed; retry the redaction"
	case errors.Is(err, context.DeadlineExceeded):
		status, detail = 504, "timed out: the request's budget ran out (raise it with timeout=, up to "+MaxBudget.String()+")"
	default:
		a.log.Error("retrieval failed", "action", action, "error", err)
	}
	meta["outcome"], meta["status"] = "failed", status
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, action, targetType, target, meta) {
		return
	}
	problem(w, status, detail)
}

func hitIDs(hits []format.Hit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.MessageID
	}
	return ids
}

// search and grep audit the full query and every result id before
// answering (launch decision: permanent forensic replay).
func (a *API) search(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filters(w, r)
	if !ok {
		return
	}
	v := r.URL.Query()
	sq := format.SearchQuery{Query: v.Get("q"), Limit: f.Limit}
	var err error
	if sq.Offset, err = strconv.Atoi(orZero(v.Get("offset"))); err != nil || sq.Offset < 0 {
		problem(w, 400, "offset: bad value")
		return
	}
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	meta := map[string]any{"mode": "ranked", "query": sq.Query, "offset": sq.Offset, "filters": f}
	page, err := a.retrieval.Search(ctx, sq, f)
	a.answerHits(w, r, "search", meta, page, err)
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func (a *API) grep(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filters(w, r)
	if !ok {
		return
	}
	gq, err := format.ParseGrepQuery(r.URL.Query())
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	meta := map[string]any{"mode": "grep", "query": gq.Pattern, "fixed": gq.Fixed, "case_sensitive": gq.CaseSensitive, "output": gq.Mode, "offset": gq.Offset, "filters": f}
	page, err := a.retrieval.Grep(ctx, gq, f)
	a.answerHits(w, r, "search", meta, page, err)
}

func (a *API) answerHits(w http.ResponseWriter, r *http.Request, action string, meta map[string]any, page *format.Page, err error) {
	if err != nil {
		a.retrievalFailed(w, r, action, "search", "", meta, err)
		return
	}
	if page == nil {
		page = &format.Page{}
	}
	if page.Hits == nil {
		page.Hits = []format.Hit{}
	}
	ids := hitIDs(page.Hits)
	for _, s := range page.Sessions {
		ids = append(ids, s.ID)
	}
	meta["outcome"], meta["count"], meta["result_ids"] = "success", len(page.Hits)+len(page.Sessions), ids
	if page.Truncated {
		meta["truncated"] = page.Reason
	}
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, action, "search", "", meta) {
		return
	}
	writeJSON(w, 200, page)
}

// sessions lists conversations; the audit keeps the glob and the ids
// listed.
func (a *API) sessions(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filters(w, r)
	if !ok {
		return
	}
	v := r.URL.Query()
	cursor := v.Get("cursor")
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	glob := v.Get("glob")
	meta := map[string]any{"glob": glob, "cursor": cursor, "filters": f}
	out, err := a.retrieval.Sessions(ctx, glob, cursor, f)
	if err != nil {
		a.retrievalFailed(w, r, "sessions.list", "conversation", "", meta, err)
		return
	}
	ids := make([]string, len(out.Sessions))
	for i, c := range out.Sessions {
		ids[i] = c.ID
	}
	meta["outcome"], meta["result_ids"] = "success", ids
	p := mustPrincipal(r)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "sessions.list", "conversation", "", meta) {
		return
	}
	writeJSON(w, 200, out)
}

// read resolves an address and returns the message with its neighbours;
// the audit keeps the address and every message id returned.
func (a *API) read(w http.ResponseWriter, r *http.Request) {
	f, ok := a.filters(w, r)
	if !ok {
		return
	}
	rq, err := format.ParseReadQuery(r.URL.Query())
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	meta := map[string]any{"address": rq.Address, "before": rq.Before, "after": rq.After, "filters": f}
	p := mustPrincipal(r)
	out, err := a.retrieval.Read(ctx, p.Credential.DeviceID, rq, f)
	if err != nil {
		a.retrievalFailed(w, r, "message.read", "message", "", meta, err)
		return
	}
	ids := make([]string, len(out.Messages))
	for i, m := range out.Messages {
		ids[i] = m.ID
	}
	meta["outcome"], meta["conversation_id"], meta["result_ids"] = "success", out.Conversation.ID, ids
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "message.read", "message", out.Focus, meta) {
		return
	}
	writeJSON(w, 200, out)
}

// raw returns archived bytes, named by source_id and generation, by the
// caller's own device path (path, file_id, optional generation), or by a
// message address (the message's record). The
// response headers attribute the bytes (D10). The audit event keeps the
// target and the outcome, never the bytes.
func (a *API) raw(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	source, path, address := v.Get("source_id"), v.Get("path"), v.Get("address")
	num := func(k string, missing int64) int64 {
		if v.Get(k) == "" {
			return missing
		}
		n, err := strconv.ParseInt(v.Get(k), 10, 64)
		if err != nil {
			return -2
		}
		return n
	}
	off, n := num("offset", -2), num("length", -2)
	ctx, cancel, ok := budgetContext(w, r)
	if !ok {
		return
	}
	defer cancel()
	meta := map[string]any{"offset": off, "length": n}
	p := mustPrincipal(r)
	var data []byte
	var attr format.Attribution
	var err error
	target := source
	switch {
	case source == "" && address != "":
		meta["address"], target = address, ""
		data, attr, err = a.retrieval.RawAt(ctx, p.Credential.DeviceID, address)
	case source == "" && path != "":
		gen := num("generation", -1)
		meta["generation"], meta["path"], meta["file_id"] = gen, path, v.Get("file_id")
		target = ""
		if p.Credential.Kind != domain.CredentialDevice || p.Credential.DeviceID == "" {
			a.retrievalFailed(w, r, "raw.read", "source", target, meta, fmt.Errorf("%w: reading by path needs a device credential", format.ErrBadRequest))
			return
		}
		if gen < -1 {
			a.retrievalFailed(w, r, "raw.read", "source", target, meta, fmt.Errorf("%w: bad generation", format.ErrBadRequest))
			return
		}
		data, attr, err = a.retrieval.RawByPath(ctx, p.Credential.DeviceID, path, v.Get("file_id"), gen, off, n)
	default:
		gen := num("generation", -2)
		meta["generation"] = gen
		data, attr, err = a.retrieval.Raw(ctx, source, gen, off, n)
	}
	if err != nil {
		a.retrievalFailed(w, r, "raw.read", "source", target, meta, err)
		return
	}
	target = attr.SourceID
	meta["outcome"], meta["bytes"] = "success", len(data)
	if !a.auditOK(w, r, p.User.ID, p.Credential.DeviceID, "raw.read", "source", target, meta) {
		return
	}
	h := w.Header()
	for k, val := range map[string]string{
		"User": attr.User, "Device": attr.Device, "Agent": attr.Agent, "Session": attr.SessionID,
		"Repo": attr.Repo, "Path": attr.Path, "Source-Id": attr.SourceID, "Generation": strconv.FormatInt(attr.Generation, 10),
	} {
		if val != "" {
			h.Set("X-Flopwire-"+k, headerSafe(val))
		}
	}
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

// headerSafe makes s a valid header value: URL-escaped when it holds
// bytes a header cannot carry (control characters, non-ASCII).
func headerSafe(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] >= 0x7f {
			return url.PathEscape(s)
		}
	}
	return s
}
