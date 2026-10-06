package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestPolicyPlacementsHTTPRejectsMalformedBeforeServer(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid", `{`},
		{"unknown device", `{"device_id":"someone-else"}`},
		{"unknown nested", `{"placements":[{"cwd":"/src","prompt":"private"}]}`},
		{"second object", `{} {}`},
		{"trailing junk", `{} x`},
		{"oversize", `{"agent":"` + strings.Repeat("a", syncproto.MaxPolicyPlacementsBytes) + `"}`},
		{"oversize trailing whitespace", `{}` + strings.Repeat(" ", syncproto.MaxPolicyPlacementsBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, syncproto.PathPolicyPlacements, strings.NewReader(tc.body))
			req.Header.Set(syncproto.HeaderVersion, "1")
			w := httptest.NewRecorder()
			// No pool: malformed transport must never reach the server method.
			new(Server).ServeSync(w, req, "authenticated-device")
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSyncCapabilitiesVersion(t *testing.T) {
	for _, version := range []string{"", "2", "1"} {
		req := httptest.NewRequest(http.MethodGet, syncproto.PathCapabilities, nil)
		req.Header.Set(syncproto.HeaderVersion, version)
		w := httptest.NewRecorder()
		new(Server).ServeSync(w, req, "authenticated-device")
		if version != "1" {
			if w.Code != http.StatusBadRequest {
				t.Fatalf("version %q: status %d", version, w.Code)
			}
			continue
		}
		var got syncproto.CapabilitiesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || got.Version != 1 || got.PolicyPlacementsVersion != syncproto.PolicyPlacementsVersion || got.MaxConcurrentFlushes != 1 {
			t.Fatalf("status %d, capabilities %+v", w.Code, got)
		}
	}
}
