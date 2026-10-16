package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
)

// The filename selects a cache candidate; its embedded owner must still match.
// Copy real saved bytes so the only invalid field is the foreign authority.
func TestForeignAdminCacheOwnerPausesOfflineRestart(t *testing.T) {
	for name, rules := range map[string][]string{
		"foreign allow": {"allow /tmp/**"},
		"foreign deny":  {"deny /tmp/oracle-alpha"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _, _ := rulesFixture(t, "-", "allow /tmp/**")
			f.once()
			beforeConversations := f.count(`SELECT count(*) FROM conversations`)
			beforeMessages := f.count(`SELECT count(*) FROM messages`)
			if beforeConversations == 0 || len(f.find("login test flake", false)) != 1 {
				t.Fatal("fixture has no retained searchable session")
			}
			queued, ok := f.rec.spec(f.path(alphaRel))
			if !ok {
				t.Fatal("fixture did not queue the retained session")
			}

			f.cfg.AdminRulesCache = filepath.Join(t.TempDir(), "policy.json")
			f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://a.example", DeviceID: "device"}
			writer := &Agent{cfg: f.cfg}
			if err := writer.writeAdminCache(AdminPolicy{Rules: rules}); err != nil {
				t.Fatal(err)
			}
			if got := writer.readAdminCache(); got.state != adminCacheValid {
				t.Fatalf("A's saved policy is not valid: %+v", got)
			}
			aPath := writer.adminCachePath()
			original, err := os.ReadFile(aPath)
			if err != nil {
				t.Fatal(err)
			}
			f.cfg.PolicyAuthority = &PolicyAuthority{Server: "https://b.example", DeviceID: "device"}
			writer.cfg = f.cfg
			bPath := writer.adminCachePath()
			if bPath == aPath {
				t.Fatal("authorities selected the same cache filename")
			}
			if err := os.WriteFile(bPath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			f.cfg.AdminRules = func(context.Context) (AdminPolicy, error) {
				return AdminPolicy{}, errors.New("B offline")
			}
			f.rec = newRecorder()
			fr := &filterRecorder{recorder: f.rec}
			f.cfg.Sync = fr
			appendFile(t, f.path(alphaRel), claudeUser("c1000000-0000-4000-8000-0000000000e1", "foreign cache offline pangolin"))
			f.restart()
			fr.mu.Lock()
			filter := fr.filter
			fr.mu.Unlock()
			if filter == nil || filter(queued) {
				t.Fatal("foreign cache allowed an old queued upload before the startup pass")
			}
			if err := f.a.admitUpload(queued); !errors.Is(err, ErrPolicyUnknown) {
				t.Fatalf("queued upload admission = %v", err)
			}
			if err := f.a.Once(ctx); !errors.Is(err, ErrPolicyUnknown) {
				t.Fatalf("foreign cache accepted offline: %v", err)
			}
			if got := f.count(`SELECT count(*) FROM conversations`); got != beforeConversations {
				t.Errorf("retained conversations changed: %d -> %d", beforeConversations, got)
			}
			if got := f.count(`SELECT count(*) FROM messages`); got != beforeMessages {
				t.Errorf("retained messages changed: %d -> %d", beforeMessages, got)
			}
			if len(f.find("login test flake", false)) != 1 {
				t.Error("foreign deny changed retained searchable data")
			}
			if len(f.find("foreign cache offline pangolin", false)) != 0 {
				t.Error("foreign allow collected the appended message")
			}
			if got := f.notified(func(devicesync.SourceSpec) bool { return true }); len(got) != 0 {
				t.Errorf("foreign cache scheduled uploads: %v", got)
			}
			for _, path := range []string{aPath, bPath} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, original) {
					t.Errorf("offline restart changed cache %s: %v", path, err)
				}
			}

			// A valid B-owned cache resumes offline, without a successful fetch.
			if err := writer.writeAdminCache(AdminPolicy{Rules: []string{"allow /tmp/**"}}); err != nil {
				t.Fatal(err)
			}
			f.restart()
			f.once()
			if len(f.find("foreign cache offline pangolin", false)) != 1 {
				t.Fatal("B's own saved policy did not resume collection offline")
			}
			if _, ok := f.rec.spec(f.path(alphaRel)); !ok {
				t.Fatal("B's own saved policy did not resume sync notifications")
			}
		})
	}
}
