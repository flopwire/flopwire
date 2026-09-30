package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tlsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	return srv, Fingerprint(srv.Certificate().Raw)
}

func TestPinnedClientAcceptsMatchingCertificate(t *testing.T) {
	srv, fp := tlsServer(t)
	var out struct{ OK bool }
	if err := (HTTP{Server: srv.URL, Client: NewHTTPClient(fp)}).JSON(t.Context(), "GET", "/", nil, &out); err != nil || !out.OK {
		t.Fatalf("pinned request: ok=%v err=%v", out.OK, err)
	}
}

func TestPinnedClientRefusesMismatchedCertificate(t *testing.T) {
	srv, _ := tlsServer(t)
	other, _ := tlsServer(t) // a different self-signed certificate
	wrong := Fingerprint(other.Certificate().Raw)
	if wrong == Fingerprint(srv.Certificate().Raw) {
		// httptest reuses one certificate; forge a different pin instead.
		wrong = FingerprintPrefix + strings.Repeat("ab", 32)
	}
	var hits atomic.Int32
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	err := (HTTP{Server: srv.URL, Token: "secret", Client: NewHTTPClient(wrong)}).JSON(t.Context(), "GET", "/", nil, nil)
	var pe *PinError
	if !errors.As(err, &pe) || pe.Got != Fingerprint(srv.Certificate().Raw) {
		t.Fatalf("mismatched pin: err=%v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("request reached the handler %d times despite the pin mismatch", n)
	}
}

func TestUnpinnedClientRefusesSelfSignedCertificate(t *testing.T) {
	srv, _ := tlsServer(t)
	err := (HTTP{Server: srv.URL}).JSON(t.Context(), "GET", "/", nil, nil)
	if err == nil || !strings.Contains(TrustHint(err).Error(), "--fingerprint") {
		t.Fatalf("self-signed without a pin: err=%v", err)
	}
}

func TestPlainHTTPOnlyToLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer srv.Close()
	if err := (HTTP{Server: srv.URL}).JSON(t.Context(), "GET", "/", nil, nil); err != nil {
		t.Fatalf("plain HTTP to loopback: %v", err)
	}
	for _, u := range []string{"http://100.82.11.20:8080/v1/x", "http://flopwire.example/v1/x", "http://[2001:db8::1]/"} {
		req, _ := http.NewRequest("GET", u, nil)
		if _, err := DefaultHTTPClient().Do(req); err == nil || !strings.Contains(err.Error(), "refusing plain HTTP") {
			t.Fatalf("plain HTTP to %s: err=%v", u, err)
		}
	}
	// A pinned server is TLS, so even loopback http is refused.
	if err := (HTTP{Server: srv.URL, Client: NewHTTPClient(FingerprintPrefix + strings.Repeat("00", 32))}).JSON(t.Context(), "GET", "/", nil, nil); err == nil {
		t.Fatal("pinned client spoke plain HTTP")
	}
}

func TestParseFingerprintForms(t *testing.T) {
	want := FingerprintPrefix + strings.Repeat("ab", 32)
	for _, in := range []string{want, strings.ToUpper(want), strings.Repeat("AB", 32),
		strings.TrimSuffix(strings.Repeat("AB:", 32), ":"), "sha256 Fingerprint=" + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")} {
		got, err := ParseFingerprint(in)
		if err != nil || got != want {
			t.Fatalf("ParseFingerprint(%q)=%q,%v", in, got, err)
		}
	}
	for _, bad := range []string{"sha256:abc", "md5:" + strings.Repeat("ab", 16), strings.Repeat("zz", 32)} {
		if _, err := ParseFingerprint(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestInviteRoundTripCarriesFingerprint(t *testing.T) {
	fp := FingerprintPrefix + strings.Repeat("0f", 32)
	in := Invite{Server: "https://flopwire.example:8080", Code: "c0de+/=&x", Fingerprint: fp}
	got, err := ParseInvite(in.String())
	if err != nil || got != in {
		t.Fatalf("round trip %q: %+v %v", in.String(), got, err)
	}
	if u, _ := url.Parse(in.String()); u.RawQuery != "" || u.Fragment == "" {
		t.Fatalf("code and pin must ride in the fragment: %q", in.String())
	}
	noPin := Invite{Server: "https://flopwire.example", Code: "abc"}
	if got, err := ParseInvite(noPin.String()); err != nil || got != noPin {
		t.Fatalf("unpinned invite: %+v %v", got, err)
	}
	for _, bad := range []string{"https://flopwire.example", "https://flopwire.example#pin=" + fp,
		"http://flopwire.example#code=abc", "https://flopwire.example#code=abc&pin=sha256:12"} {
		if _, err := ParseInvite(bad); err == nil {
			t.Fatalf("accepted invite %q", bad)
		}
	}
}

func TestRequestTimesOutWaitingForHeaders(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	hc := newHTTPClient("", 150*time.Millisecond, MinRate)
	start := time.Now()
	err := (HTTP{Server: srv.URL, Client: hc}).JSON(context.Background(), "GET", "/", nil, nil)
	if !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("stalled server: err=%v", err)
	}
	var ue *url.Error
	if !errors.As(err, &ue) || !ue.Timeout() {
		t.Fatalf("want a *url.Error timeout, got %T %v", err, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("deadline took %v", d)
	}
}

func TestResponseBodyStallTimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	res, err := newHTTPClient("", 150*time.Millisecond, MinRate).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, err = io.ReadAll(res.Body)
	var ue *url.Error
	if !errors.As(err, &ue) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("stalled body: %T %v", err, err)
	}
}

func TestDeadlineScalesWithPayload(t *testing.T) {
	g := &guard{budget: time.Second, rate: 1000}
	if g.allowance(0) != time.Second || g.allowance(-1) != time.Second {
		t.Fatal("unknown or empty body gets the base budget")
	}
	if got := g.allowance(10_000); got != 11*time.Second {
		t.Fatalf("10kB at 1kB/s: %v", got)
	}
	// A slow but moving upload within its allowance succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"n":`+strconv.FormatInt(n, 10)+`}`)
	}))
	defer srv.Close()
	body := bytes.Repeat([]byte("x"), 1<<20)
	var out struct{ N int64 }
	if err := (HTTP{Server: srv.URL, Client: newHTTPClient("", 50*time.Millisecond, 1<<20)}).JSON(t.Context(), "POST", "/", string(body), &out); err != nil || out.N == 0 {
		t.Fatalf("upload: %v %d", err, out.N)
	}
	if DefaultHTTPClient().Transport.(*guard).base.(*http.Transport).ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Fatal("default client has no ResponseHeaderTimeout")
	}
}
