package api_test

import (
	"net/http"
	"testing"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/flopwire/flopwire/internal/syncproto"
)

type policySyncRecorder struct {
	device string
	path   string
}

func (s *policySyncRecorder) ServeSync(w http.ResponseWriter, r *http.Request, deviceID string) {
	s.device, s.path = deviceID, r.URL.Path
	w.WriteHeader(http.StatusNoContent)
}

func TestPolicySyncRoutesRequireDeviceCredential(t *testing.T) {
	s := store.NewMemory()
	spy := new(policySyncRecorder)
	hs := newServer(t, s, api.Config{Sync: spy})
	login := seedAdmin(t, s)
	enrolled := postJSON(t, hs.URL+"/v1/devices", map[string]string{"name": "policy-mac", "platform": "darwin"}, bearer(login))
	token := enrolled["token"].(string)
	deviceID := enrolled["device"].(map[string]any)["id"].(string)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, syncproto.PathCapabilities},
		{http.MethodPost, syncproto.PathPolicyPlacements},
	} {
		for _, tc := range []struct {
			token string
			want  int
		}{
			{"", http.StatusUnauthorized},
			{login, http.StatusForbidden},
			{token, http.StatusNoContent},
		} {
			spy.device, spy.path = "", ""
			resp := request(t, route.method, hs.URL+route.path, nil, bearer(tc.token))
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("%s %s: status %d, want %d", route.method, route.path, resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusNoContent {
				if spy.device != deviceID || spy.path != route.path {
					t.Fatalf("forwarded device %q path %q", spy.device, spy.path)
				}
			} else if spy.device != "" {
				t.Fatal("unauthorized request reached sync server")
			}
		}
	}
}
