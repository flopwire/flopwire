package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

type HTTP struct {
	Server, Token string
	Client        *http.Client
}

// APIError is a non-2xx response. Code is the problem document's stable
// machine-readable code, when the server sends one.
type APIError struct {
	StatusCode int
	Code       string
	// Detail is the problem document's detail for a 400 or 404 from a
	// Flopwire server (a retrieval request it could not answer: a bad
	// regex, an ambiguous address); empty otherwise.
	Detail string
}

func (e *APIError) Error() string {
	if e.StatusCode == http.StatusUnauthorized {
		// Print only recognized codes, never arbitrary server response text.
		switch e.Code {
		case "credential_rotated", "credential_revoked", "reauth_required", "credential_invalid":
			return fmt.Sprintf("flopwire API 401 Unauthorized (%s); run flopwire login or renew FLOPWIRE_TOKEN if set", e.Code)
		case "credential_expired":
			return "flopwire API 401 Unauthorized (credential_expired); run flopwire login or mint a new FLOPWIRE_TOKEN"
		default:
			return "flopwire API 401 Unauthorized; run flopwire login or renew FLOPWIRE_TOKEN if set"
		}
	}
	if e.Detail != "" {
		return fmt.Sprintf("flopwire API %d: %s", e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("flopwire API %d %s", e.StatusCode, http.StatusText(e.StatusCode))
}

func (c HTTP) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Server, "/")+path, body)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.Client
	if hc == nil {
		hc = DefaultHTTPClient()
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The body is not echoed: a proxy or server error page can carry
		// request values, and callers print this error.
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		var problem struct {
			Type   string `json:"type"`
			Status int    `json:"status"`
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(raw, &problem)
		e := &APIError{StatusCode: res.StatusCode, Code: problem.Code}
		usedToken := c.Token
		if res.Request != nil {
			usedToken = strings.TrimPrefix(res.Request.Header.Get("Authorization"), "Bearer ")
		}
		// Only a Flopwire problem document's own detail is shown, never a
		// body a proxy wrote, and never one holding the credential.
		if (res.StatusCode == 400 || res.StatusCode == 404) && problem.Type == "about:blank" && problem.Status == res.StatusCode &&
			len(problem.Detail) <= 500 && (c.Token == "" || !strings.Contains(problem.Detail, c.Token)) &&
			(usedToken == "" || !strings.Contains(problem.Detail, usedToken)) {
			e.Detail = problem.Detail
		}
		return e
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*rawBody); ok {
		b, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
		*raw = b
		return err
	}
	return json.NewDecoder(res.Body).Decode(out)
}
func (c HTTP) JSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	return c.do(ctx, method, path, body, map[string]string{"Content-Type": "application/json"}, out)
}

// Grep runs grep on the server (GET /v1/grep).
func (c HTTP) Grep(ctx context.Context, q format.GrepQuery, f format.Filters) (*format.Page, error) {
	var out format.Page
	err := c.do(ctx, "GET", "/v1/grep?"+q.Values(f.Values()).Encode(), nil, nil, &out)
	return &out, err
}

// Search runs ranked search on the server (GET /v1/search).
func (c HTTP) Search(ctx context.Context, q format.SearchQuery, f format.Filters) (*format.Page, error) {
	if q.Limit > 0 {
		f.Limit = q.Limit
	}
	v := f.Values()
	v.Set("q", q.Query)
	if q.Offset > 0 {
		v.Set("offset", strconv.Itoa(q.Offset))
	}
	if q.Timeout > 0 {
		v.Set("timeout", q.Timeout.String())
	}
	var out format.Page
	err := c.do(ctx, "GET", "/v1/search?"+v.Encode(), nil, nil, &out)
	return &out, err
}

// Sessions lists conversations on the server (GET /v1/sessions).
func (c HTTP) Sessions(ctx context.Context, glob, cursor string, f format.Filters) (*format.Sessions, error) {
	v := f.Values()
	if glob != "" {
		v.Set("glob", glob)
	}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	var out format.Sessions
	err := c.do(ctx, "GET", "/v1/sessions?"+v.Encode(), nil, nil, &out)
	return &out, err
}

// Read resolves an address on the server (GET /v1/read).
func (c HTTP) Read(ctx context.Context, q format.ReadQuery, f format.Filters) (*format.Context, error) {
	var out format.Context
	err := c.do(ctx, "GET", "/v1/read?"+q.Values(f.Values()).Encode(), nil, nil, &out)
	return &out, err
}

// Raw returns archived bytes of a source generation.
func (c HTTP) Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, error) {
	v := url.Values{"source_id": {sourceID}, "generation": {strconv.FormatInt(generation, 10)},
		"offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.FormatInt(length, 10)}}
	var out rawBody
	err := c.do(ctx, "GET", "/v1/raw?"+v.Encode(), nil, nil, &out)
	return out, err
}

// RawByPath returns archived bytes of this device's source at path (and
// file id, when known); generation -1 is the latest.
func (c HTTP) RawByPath(ctx context.Context, path, fileID string, generation, offset, length int64) ([]byte, error) {
	v := url.Values{"path": {path}, "offset": {strconv.FormatInt(offset, 10)}, "length": {strconv.FormatInt(length, 10)}}
	if fileID != "" {
		v.Set("file_id", fileID)
	}
	if generation >= 0 {
		v.Set("generation", strconv.FormatInt(generation, 10))
	}
	var out rawBody
	err := c.do(ctx, "GET", "/v1/raw?"+v.Encode(), nil, nil, &out)
	return out, err
}

// RawAt returns the transcript bytes of the message an address names.
func (c HTTP) RawAt(ctx context.Context, address string) ([]byte, error) {
	var out rawBody
	err := c.do(ctx, "GET", "/v1/raw?"+url.Values{"address": {address}}.Encode(), nil, nil, &out)
	return out, err
}

// rawBody receives a non-JSON body.
type rawBody []byte
