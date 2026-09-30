package client

// Transport security and timeouts for every device-to-server call: the
// admin CLI, `--server` retrieval, the raw fallback and the device sync
// client all get their *http.Client from NewHTTPClient.
//
// A server with a self-signed certificate is trusted by pinning: the SHA-256
// of its leaf certificate travels in the invite, is saved in the config at
// claim or login, and is checked on every TLS handshake. Without a pin the
// normal Web PKI verification applies (a server with an ACME certificate, or
// a TLS-terminating proxy with a CA-signed one). Plain HTTP is refused except
// to a loopback host.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// FingerprintPrefix starts every pin string.
const FingerprintPrefix = "sha256:"

// Fingerprint returns the pin of a DER certificate: "sha256:" and the
// lowercase hex SHA-256 of the bytes.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return FingerprintPrefix + hex.EncodeToString(sum[:])
}

// ParseFingerprint normalizes a pin. It accepts the "sha256:" prefix or
// none, any case, and the colon-separated form `openssl x509 -fingerprint
// -sha256` prints. The empty string stays empty (no pin).
func ParseFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	lower := strings.ToLower(s)
	lower = strings.TrimPrefix(lower, FingerprintPrefix)
	lower = strings.TrimPrefix(lower, "sha256 fingerprint=")
	lower = strings.ReplaceAll(lower, ":", "")
	raw, err := hex.DecodeString(lower)
	if err != nil || len(raw) != sha256.Size {
		return "", errors.New("fingerprint must be a SHA-256 certificate fingerprint (sha256:<64 hex digits>)")
	}
	return FingerprintPrefix + lower, nil
}

// IsLoopbackHost reports whether host (no port) names this machine only.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Timeouts. ResponseHeaderTimeout bounds how long the server may think; it
// sits above the server's 60s write timeout. Each request also gets a
// deadline of RequestBase plus its body size at MinRate, and the response
// body gets the same again once headers arrive, so a stalled upload or
// download fails instead of hanging the agent or a CLI call.
const (
	ResponseHeaderTimeout = 75 * time.Second
	RequestBase           = 90 * time.Second
	MinRate               = 64 << 10 // bytes per second
)

// NewHTTPClient returns the client for one server. fingerprint is a pin
// from ParseFingerprint, or "" for Web PKI verification.
func NewHTTPClient(fingerprint string) *http.Client {
	return newHTTPClient(fingerprint, RequestBase, MinRate)
}

func newHTTPClient(fingerprint string, base time.Duration, rate int64) *http.Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if fingerprint != "" {
		want := []byte(fingerprint)
		// Chain and hostname checks are replaced by the pin: the server's
		// certificate is self-signed and names whatever host it chose.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return &PinError{}
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
				return &PinError{Got: got}
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: &guard{base: tr, pinned: fingerprint != "", budget: base, rate: rate}}
}

// PinError is a TLS handshake whose certificate does not match the pin.
type PinError struct{ Got string }

// Permanent: retrying cannot fix a mismatched pin (syncproto.Permanent).
func (e *PinError) Permanent() bool { return true }

func (e *PinError) Error() string {
	if e.Got == "" {
		return "server presented no certificate to check against the pinned fingerprint"
	}
	return "server certificate " + e.Got + " does not match the pinned fingerprint; refusing to connect (a changed server certificate needs a new invite or login --fingerprint)"
}

var (
	defaultOnce   sync.Once
	defaultClient *http.Client
)

// DefaultHTTPClient is the unpinned client, shared.
func DefaultHTTPClient() *http.Client {
	defaultOnce.Do(func() { defaultClient = NewHTTPClient("") })
	return defaultClient
}

// HTTPClient returns the client for this config's server and pin.
func (c Config) HTTPClient() *http.Client {
	if c.TLSFingerprint == "" {
		return DefaultHTTPClient()
	}
	return NewHTTPClient(c.TLSFingerprint)
}

// API returns the API client for this config's server with token.
func (c Config) API(token string) HTTP {
	return HTTP{Server: c.Server, Token: token, Client: c.HTTPClient()}
}

// guard enforces the scheme rules and the per-request deadline.
type guard struct {
	base   http.RoundTripper
	pinned bool
	budget time.Duration
	rate   int64
}

// errTimeout is the cancellation cause of a request that ran out of time.
// It is a net-style timeout wrapping os.ErrDeadlineExceeded, so callers see
// a timeout (retryable), not a cancel.
var errTimeout error = timeoutError{}

type timeoutError struct{}

func (timeoutError) Error() string   { return "flopwire: request deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func (timeoutError) Unwrap() error   { return os.ErrDeadlineExceeded }

func (g *guard) allowance(n int64) time.Duration {
	if n <= 0 {
		return g.budget
	}
	return g.budget + time.Duration(n/g.rate)*time.Second
}

func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkScheme(req.URL, g.pinned); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(g.allowance(req.ContentLength), func() { cancel(errTimeout) })
	res, err := g.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	// Headers are in: the body gets a fresh allowance for its size.
	timer.Reset(g.allowance(res.ContentLength))
	res.Body = &guardedBody{ReadCloser: res.Body, ctx: ctx, url: req.URL.String(), stop: func() { timer.Stop(); cancel(nil) }}
	return res, nil
}

// guardedBody releases the request's timer on Close and reports a read cut
// off by the deadline as a *url.Error timeout.
type guardedBody struct {
	io.ReadCloser
	ctx  context.Context
	url  string
	stop func()
	once sync.Once
}

func (b *guardedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && errors.Is(context.Cause(b.ctx), errTimeout) {
		err = &url.Error{Op: "read", URL: b.url, Err: errTimeout}
	}
	return n, err
}

func (b *guardedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.stop)
	return err
}

// checkScheme refuses plain HTTP to anything but loopback, and plain HTTP
// at all once a pin is configured (a pin means the server speaks TLS).
func checkScheme(u *url.URL, pinned bool) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if pinned {
			return errors.New("the server is pinned by TLS fingerprint but its URL is http://; use https://")
		}
		if IsLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("refusing plain HTTP to %s: flopwire speaks TLS except on loopback; use https://", u.Host)
	default:
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

// Invite is what a member needs to claim an invitation: the server, the
// one-time code and, for a self-signed server, the certificate pin.
type Invite struct {
	Server      string
	Code        string
	Fingerprint string
}

// String renders the invite as the server URL with the code and pin in the
// fragment: https://host:8080#code=...&pin=sha256:... A fragment is never
// sent in a request, so pasting the string into a browser leaks nothing.
func (i Invite) String() string {
	v := url.Values{"code": {i.Code}}
	if i.Fingerprint != "" {
		v.Set("pin", i.Fingerprint)
	}
	return i.Server + "#" + v.Encode()
}

// ParseInvite reads the String form.
func ParseInvite(s string) (Invite, error) {
	s = strings.TrimSpace(s)
	server, frag, ok := strings.Cut(s, "#")
	if !ok {
		return Invite{}, errors.New("invite must look like https://host:port#code=...&pin=...")
	}
	v, err := url.ParseQuery(frag)
	if err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	inv := Invite{Code: v.Get("code")}
	if inv.Code == "" {
		return Invite{}, errors.New("invite has no code")
	}
	if inv.Server, err = NormalizeServer(server); err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	if inv.Fingerprint, err = ParseFingerprint(v.Get("pin")); err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	if inv.Fingerprint != "" && !strings.HasPrefix(inv.Server, "https://") {
		return Invite{}, errors.New("invite pins a certificate but its server is not https://")
	}
	return inv, nil
}

// TrustHint turns an unknown-authority TLS error into advice.
func TrustHint(err error) error {
	var ua x509.UnknownAuthorityError
	var ve *tls.CertificateVerificationError
	if errors.As(err, &ua) || errors.As(err, &ve) {
		return fmt.Errorf("%w\nthe server's certificate did not verify against the system trust store; if it is a self-signed flopwire server, pass --fingerprint (from `flopwire fingerprint` on the server host) or claim with the --invite string", err)
	}
	return err
}
