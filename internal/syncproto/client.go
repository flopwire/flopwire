package syncproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func marshalJSON(v any) ([]byte, error)   { return json.Marshal(v) }
func unmarshalJSON(b []byte, v any) error { return json.Unmarshal(b, v) }

// Transport is the device's view of the server.
type Transport interface {
	Has(ctx context.Context, hashes []Hash) (missing []Hash, err error)
	Flush(ctx context.Context, req *FlushRequest) (*FlushResponse, error)
}

// Client speaks the protocol over HTTP with the device credential
// (the same bearer token internal/client uses).
type Client struct {
	Server string // base URL, e.g. https://flopwire.example:8443
	Token  string
	HTTP   *http.Client // required: the configured client (TLS pin)
}

// ErrNoHTTPClient is a Client without HTTP. There is no default: the
// configured client is what enforces the server's TLS pin.
var ErrNoHTTPClient = errors.New("syncproto: Client.HTTP is required")

// HTTPError is a non-2xx answer. Retryable reports whether the device
// should back off and retry (5xx, 408, 429) rather than treat the request
// as rejected.
type HTTPError struct {
	Status int
	Body   ErrorResponse
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("syncproto: server answered %d %s: %s", e.Status, e.Body.Code, e.Body.Message)
}

func (e *HTTPError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests
}

// Permanent reports whether err will not go away by retrying: an error in
// its chain says so with a Permanent() bool method (a TLS pin mismatch,
// which needs the user to re-pin the server). Sync stops until restart.
func Permanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

// Retryable reports whether err is transient: a network failure (the
// server is asleep or unreachable) or a retryable HTTP status. Protocol
// rejections (4xx) and local errors are not.
func Retryable(err error) bool {
	if Permanent(err) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Retryable()
	}
	var ue *url.Error
	return errors.As(err, &ue) && !errors.Is(err, context.Canceled)
}

func (c *Client) Has(ctx context.Context, hashes []Hash) ([]Hash, error) {
	raw, err := json.Marshal(HasRequest{Version: Version, Hashes: hashes})
	if err != nil {
		return nil, err
	}
	var out HasResponse
	if err := c.do(ctx, PathHas, "application/json", bytes.NewReader(raw), -1, &out); err != nil {
		return nil, err
	}
	return out.Missing, nil
}

func (c *Client) Flush(ctx context.Context, req *FlushRequest) (*FlushResponse, error) {
	hdr, err := json.Marshal(&req.Header)
	if err != nil {
		return nil, err
	}
	size := int64(8+len(hdr)) + req.Header.PayloadSize()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(EncodeFlush(pw, req))
	}()
	var out FlushResponse
	err = c.do(ctx, PathFlush, FlushContentType, pr, size, &out)
	// Stop the encoder and wait for it: the caller owns req.Payload again
	// once Flush returns.
	pr.CloseWithError(io.ErrClosedPipe)
	<-done
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, path, ctype string, body io.Reader, size int64, out any) error {
	if c.HTTP == nil {
		// The caller's client carries the TLS pin; a default one would
		// bypass it.
		return ErrNoHTTPClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Server, "/")+path, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set(HeaderVersion, strconv.Itoa(Version))
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		he := &HTTPError{Status: res.StatusCode}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(raw, &he.Body) != nil {
			he.Body.Message = string(raw)
		}
		return he
	}
	return json.NewDecoder(res.Body).Decode(out)
}
