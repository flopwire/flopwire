package syncproto

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestUploadConcurrencyExplicitNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		requested, status, want int
		bad                     bool
	}{
		{"two", `{"version":1,"max_concurrent_flushes":2}`, 2, 200, 2, false},
		{"serial", `{"version":1,"max_concurrent_flushes":1}`, 2, 200, 1, false},
		{"legacy404", `old server`, 2, 404, 1, false},
		{"requestone", `{"version":1,"max_concurrent_flushes":1}`, 1, 200, 1, false},
		{"exceedsrequest", `{"version":1,"max_concurrent_flushes":2}`, 1, 200, 0, true},
		{"zero", `{"version":1,"max_concurrent_flushes":0}`, 2, 200, 0, true},
		{"missing", `{"version":1}`, 2, 200, 0, true},
		{"future", `{"version":1,"max_concurrent_flushes":3}`, 2, 200, 0, true},
		{"wrongversion", `{"version":2,"max_concurrent_flushes":2}`, 2, 200, 0, true},
		{"malformed", `bad`, 2, 200, 0, true},
		{"trailing", `{"version":1,"max_concurrent_flushes":2}{}`, 2, 200, 0, true},
		{"oversize", strings.Repeat(" ", 64<<10) + `{}`, 2, 200, 0, true},
		{"unauthorized", `{"code":"credential_invalid"}`, 2, 401, 0, true},
		{"forbidden", `{}`, 2, 403, 0, true},
		{"unavailable", `{}`, 2, 503, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != PathCapabilities || r.URL.Query().Get("max_concurrent_flushes") != strconv.Itoa(tc.requested) || r.Header.Get("Authorization") != "Bearer device" || r.Header.Get(HeaderVersion) != "1" {
					t.Error("missing explicit authenticated capability request")
				}
				if r.ContentLength > 0 {
					t.Error("capability GET has body")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := Client{Server: srv.URL, Token: "device", HTTP: srv.Client()}
			got, err := c.UploadConcurrency(context.Background(), tc.requested)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %d,%v want %d bad=%v", got, err, tc.want, tc.bad)
			}
			if tc.status == 401 {
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != 401 {
					t.Fatal("credential failure lost HTTP identity")
				}
			}
		})
	}
}

func TestUploadConcurrencyFailsClosedBeforeOrDuringTransport(t *testing.T) {
	c := Client{}
	if _, err := c.UploadConcurrency(context.Background(), 2); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatal(err)
	}
	c.HTTP = &http.Client{}
	if _, err := c.UploadConcurrency(context.Background(), 2); err == nil {
		t.Fatal("anonymous negotiation accepted")
	}
	for _, n := range []int{-1, 0, 3} {
		if _, err := c.UploadConcurrency(context.Background(), n); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	c.Token = "device"
	c.Server = "http://127.0.0.1:1"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := c.UploadConcurrency(ctx, 2); got != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel downgraded to serial: %d %v", got, err)
	}
}

type uploadCapabilityPinError struct{}

func (uploadCapabilityPinError) Error() string   { return "synthetic pin mismatch" }
func (uploadCapabilityPinError) Permanent() bool { return true }

type uploadCapabilityPinTransport struct{}

func (uploadCapabilityPinTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, uploadCapabilityPinError{}
}
func TestUploadConcurrencyPreservesPermanentPinFailure(t *testing.T) {
	c := Client{Server: "https://synthetic.invalid", Token: "device", HTTP: &http.Client{Transport: uploadCapabilityPinTransport{}}}
	if got, err := c.UploadConcurrency(context.Background(), 2); got != 0 || !Permanent(err) {
		t.Fatalf("pin failure became capability: %d %v", got, err)
	}
}
