package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A local credential update must not leave an MCP call waiting indefinitely.
const credentialReloadTimeout = 5 * time.Second

// RetrievalAPI follows saved credentials for read-only requests in long-running
// clients. Environment credentials remain fixed and never fall back to disk.
func (c Config) RetrievalAPI() HTTP {
	api := c.API(c.Token)
	api.Client = &http.Client{Transport: &retrievalTransport{
		initial: c, load: LoadFile, lock: WithConfigLock,
		pin: c.TLSFingerprint, client: api.Client,
	}}
	return api
}

type retrievalTransport struct {
	initial Config
	load    func() (Config, error)
	lock    func(context.Context, func() error) error
	mu      sync.Mutex
	pin     string
	client  *http.Client
}

func (t *retrievalTransport) saved() (Config, error) {
	if t.initial.FromEnv {
		return t.initial, nil
	}
	c, err := t.load()
	if err != nil {
		return Config{}, errors.New("Flopwire credential configuration is unavailable; run flopwire login")
	}
	want, err := NormalizeServer(t.initial.Server)
	if err != nil {
		return Config{}, err
	}
	got, err := NormalizeServer(c.Server)
	if err != nil || got != want || c.DeviceID != t.initial.DeviceID {
		return Config{}, errors.New("Flopwire server or device changed; restart the Flopwire MCP server")
	}
	return c, nil
}

func (t *retrievalTransport) send(req *http.Request, c Config) (*http.Response, error) {
	t.mu.Lock()
	if c.TLSFingerprint != t.pin {
		t.client.CloseIdleConnections()
		t.client, t.pin = c.HTTPClient(), c.TLSFingerprint
	}
	hc := t.client
	t.mu.Unlock()
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := hc.Transport.RoundTrip(r)
	if resp != nil {
		// HTTP.do must sanitize errors against the credential actually sent,
		// including the final credential when a rotation causes a retry.
		resp.Request = r
	}
	return resp, err
}

func (t *retrievalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.initial.Server)
	if err != nil || !strings.EqualFold(req.URL.Host, u.Host) || req.URL.Scheme != u.Scheme {
		return nil, errors.New("Flopwire retrieval refuses to send credentials to another server")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, errors.New("Flopwire retrieval transport only supports read-only requests")
	}
	c, err := t.saved()
	if err != nil {
		return nil, err
	}
	resp, err := t.send(req, c)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || t.initial.FromEnv {
		return resp, err
	}
	// Rotation commits on the server before saving the new token. Wait for
	// its config lock before checking whether this request raced that save.
	var next Config
	lockCtx, cancel := context.WithTimeout(req.Context(), credentialReloadTimeout)
	err = t.lock(lockCtx, func() error {
		var loadErr error
		next, loadErr = t.saved()
		return loadErr
	})
	cancel()
	if err != nil {
		resp.Body.Close()
		if errors.Is(err, context.DeadlineExceeded) && req.Context().Err() == nil {
			return nil, fmt.Errorf("Flopwire credential update is still in progress; retry the tool call: %w", err)
		}
		return nil, err
	}
	if next.Token == c.Token {
		return resp, nil
	}
	resp.Body.Close()
	return t.send(req, next)
}
