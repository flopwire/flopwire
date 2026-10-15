package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
)

// Cross-authority cache reads must neither purge retained rows under a foreign
// deny nor collect under a foreign allow while the selected server is offline.
func TestPolicyAuthorityTransitionPreservesRowsUntilOwnFetch(t *testing.T) {
	for _, denyA := range []bool{false, true} {
		name := "A-allows-B-denies"
		if denyA {
			name = "A-denies-B-allows"
		}
		t.Run(name, func(t *testing.T) {
			f, _, _ := rulesFixture(t, "-", "allow /tmp/**")
			f.once()
			before := f.count(`SELECT count(*) FROM conversations`)
			policyServer := func(deny bool) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer test-device" {
						http.Error(w, "unauthorized", 401)
						return
					}
					if deny {
						w.Write([]byte(`{"path_rules":["/tmp/oracle-alpha"]}`))
					} else {
						w.Write([]byte(`{"path_rules":[]}`))
					}
				}))
			}
			a, b := policyServer(denyA), policyServer(!denyA)
			defer a.Close()
			defer b.Close()
			f.cfg.AdminRulesCache = filepath.Join(t.TempDir(), "policy.json")
			f.cfg.PolicyAuthority = &PolicyAuthority{Server: a.URL, DeviceID: "device"}
			f.cfg.AdminRules = FetchAdminRules(a.URL, "test-device", a.Client())
			f.restart()
			f.once()
			afterA := f.count(`SELECT count(*) FROM conversations`)
			if denyA && afterA >= before {
				t.Fatal("A deny did not purge actual indexed rows")
			}
			if !denyA && afterA != before {
				t.Fatal("A allow changed retained rows")
			}
			f.cfg.PolicyAuthority = &PolicyAuthority{Server: b.URL, DeviceID: "device"}
			f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{}, errors.New("B offline") }
			f.rec = newRecorder()
			f.cfg.Sync = f.rec
			f.restart()
			if err := f.a.Once(ctx); !errors.Is(err, ErrPolicyUnknown) {
				t.Fatalf("foreign cache accepted offline: %v", err)
			}
			if got := f.count(`SELECT count(*) FROM conversations`); got != afterA {
				t.Fatalf("offline B changed rows %d -> %d", afterA, got)
			}
			if got := f.notified(func(devicesync.SourceSpec) bool { return true }); len(got) != 0 {
				t.Fatalf("offline B queued uploads: %v", got)
			}
			f.a.cfg.AdminRules = FetchAdminRules(b.URL, "test-device", b.Client())
			f.a.refreshPolicy(ctx, true)
			f.once()
			alpha := f.count(convsCwd, "/tmp/oracle-alpha")
			if denyA && alpha == 0 {
				t.Fatal("B allow failed to recollect A-denied rows")
			}
			if !denyA && alpha != 0 {
				t.Fatal("B deny failed to purge A-allowed rows")
			}
			// Returning offline to A must recover A's cache, not B's latest answer.
			f.cfg.PolicyAuthority = &PolicyAuthority{Server: a.URL, DeviceID: "device"}
			f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) { return AdminPolicy{}, errors.New("A offline") }
			f.restart()
			f.once()
			alpha = f.count(convsCwd, "/tmp/oracle-alpha")
			if denyA && alpha != 0 || !denyA && alpha == 0 {
				t.Fatalf("A offline recovery used wrong policy: alpha=%d", alpha)
			}
		})
	}
}
