package cowork

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const nativeID = "11111111-1111-1111-1111-111111111111"

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func fixture(t *testing.T, root, account, workspace, id string, md map[string]any) (string, string) {
	t.Helper()
	base := filepath.Join(root, account, workspace)
	if md != nil {
		b, err := json.Marshal(md)
		if err != nil {
			t.Fatal(err)
		}
		put(t, filepath.Join(base, id+".json"), string(b))
	}
	project := filepath.Join(base, id, ".claude", "projects", "project")
	transcript := filepath.Join(project, nativeID+".jsonl")
	put(t, transcript, "{\"type\":\"user\",\"message\":{\"content\":\"synthetic\"}}\n")
	return project, transcript
}
func goodMetadata(id string) map[string]any {
	return map[string]any{"sessionId": id, "cliSessionId": nativeID, "userSelectedFolders": []string{"/host/deleted/repo"}}
}
func discover(t *testing.T, root string) Result {
	t.Helper()
	r, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func includes(paths []string, p string) bool {
	for _, v := range paths {
		if v == p {
			return true
		}
	}
	return false
}

func TestDiscoveryPreservesNativeEvidenceAndPrivateMetadataExcluded(t *testing.T) {
	root := filepath.Join(t.TempDir(), "container")
	md := goodMetadata("app-session")
	md["title"] = "private title"
	md["prompt"] = "private prompt"
	project, transcript := fixture(t, root, "account", "workspace", "app-session", md)
	put(t, filepath.Join(root, "account", "workspace", "app-session", "audit.jsonl"), "private audit")
	put(t, filepath.Join(project, nativeID, "subagents", "agent-abc.jsonl"), "synthetic")
	put(t, filepath.Join(project, nativeID, "tool-results", "result.txt"), "synthetic")
	r := discover(t, root)
	if len(r.Sessions) != 1 {
		t.Fatalf("sessions/links: %+v", r)
	}
	s := r.Sessions[0].Session
	if s.SessionID != nativeID || s.Transcript != transcript || len(s.Subagents) != 1 || len(s.Companions) != 1 {
		t.Fatalf("native evidence changed: %+v", s)
	}
	l := r.Sessions[0].Link
	if l.SessionID != "app-session" || l.CLISessionID != nativeID || !l.Mapping.Known() {
		t.Fatalf("link: %+v", l)
	}
	if !reflect.DeepEqual(l.Mapping.HostPaths(), []string{"/host/deleted/repo"}) {
		t.Fatal(l.Mapping.HostPaths())
	}
	paths := l.Mapping.HostPaths()
	paths[0] = "/mutated"
	if l.Mapping.HostPaths()[0] != "/host/deleted/repo" {
		t.Fatal("mutable mapping")
	}
	for _, entry := range r.Sessions {
		s := entry.Session
		for _, c := range s.Companions {
			if strings.Contains(c.Path, "audit") || strings.HasSuffix(c.Path, "app-session.json") {
				t.Fatal("private app metadata collected")
			}
		}
	}
}

func TestLateContainerAndMetadataOnlySession(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "missing", "container")
	r := discover(t, root)
	if r.Unavailable != 1 || !includes(r.WatchDirs, parent) {
		t.Fatalf("missing container: %+v", r)
	}
	meta := filepath.Join(root, "account", "workspace", "later.json")
	put(t, meta, `{"sessionId":"later","cliSessionId":"`+nativeID+`"}`)
	r = discover(t, root)
	if len(r.Sessions) != 0 || r.MetadataOnly != 1 {
		t.Fatalf("metadata-only: %+v", r)
	}
	for _, p := range []string{root, filepath.Join(root, "account"), filepath.Dir(meta)} {
		if !includes(r.WatchDirs, p) {
			t.Fatalf("missing ancestor %s", p)
		}
	}
	project, _ := fixture(t, root, "account", "workspace", "later", goodMetadata("later"))
	r = discover(t, root)
	if len(r.Sessions) != 1 || !includes(r.WatchDirs, project) {
		t.Fatalf("late discovery: %+v", r)
	}
	if again := discover(t, root); !reflect.DeepEqual(r, again) {
		t.Fatal("restart discovery differs")
	}
}

func TestMappingConservative(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"empty", func(m map[string]any) { delete(m, "userSelectedFolders") }, "no verified"},
		{"vm", func(m map[string]any) { m["userApprovedFileAccessPaths"] = []string{"/sessions/x/repo"} }, "non-host"},
		{"relative", func(m map[string]any) { m["userApprovedFileAccessPaths"] = []string{"relative"} }, "non-host"},
		{"mount", func(m map[string]any) { m["fileDeleteApprovedMounts"] = []string{"repo"} }, "deletion mount"},
		{"identity", func(m map[string]any) { m["sessionId"] = "different" }, "identity"},
		{"native identity", func(m map[string]any) { m["cliSessionId"] = "different" }, "cliSessionId"},
		{"malformed array", func(m map[string]any) { m["userSelectedFolders"] = "/host/repo" }, "malformed"},
		{"null array", func(m map[string]any) { m["userApprovedFileAccessPaths"] = nil }, "malformed"},
		{"null element", func(m map[string]any) { m["userApprovedFileAccessPaths"] = []any{nil} }, "malformed"},
		{"delimiter", func(m map[string]any) { m["userApprovedFileAccessPaths"] = []string{"/host/repo\nother"} }, "non-host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			md := goodMetadata("app")
			tc.mutate(md)
			fixture(t, root, "a", "w", "app", md)
			r := discover(t, root)
			if len(r.Sessions) != 1 {
				t.Fatal(r)
			}
			m := r.Sessions[0].Link.Mapping
			if m.Known() || !strings.Contains(m.Reason(), tc.reason) {
				t.Fatalf("mapping: %+v", m)
			}
			if tc.name == "vm" || tc.name == "mount" {
				if !reflect.DeepEqual(m.HostPaths(), []string{"/host/deleted/repo"}) {
					t.Fatal("lost host deny evidence")
				}
			}
		})
	}
}

func TestAbsentAndBoundedMetadata(t *testing.T) {
	for _, name := range []string{"missing", "oversized", "trailing", "malformed"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			fixture(t, root, "a", "w", "app", nil)
			path := filepath.Join(root, "a", "w", "app.json")
			switch name {
			case "oversized":
				put(t, path, strings.Repeat(" ", maxMetadataBytes+1))
			case "trailing":
				b, _ := json.Marshal(goodMetadata("app"))
				put(t, path, string(b)+" {}")
			case "malformed":
				put(t, path, "{")
			}
			r := discover(t, root)
			if len(r.Sessions) != 1 || r.Sessions[0].Link.Mapping.Known() {
				t.Fatal(r)
			}
		})
	}
}

func TestMalformedFieldsPreserveIndependentHostDenyEvidence(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"approved null", func(m map[string]any) { m["userApprovedFileAccessPaths"] = nil }},
		{"approved wrong type", func(m map[string]any) { m["userApprovedFileAccessPaths"] = 42 }},
		{"approved bad element", func(m map[string]any) { m["userApprovedFileAccessPaths"] = []any{42} }},
		{"deletion null", func(m map[string]any) { m["fileDeleteApprovedMounts"] = nil }},
		{"deletion wrong type", func(m map[string]any) { m["fileDeleteApprovedMounts"] = true }},
		{"identity absent", func(m map[string]any) { delete(m, "sessionId") }},
		{"identity mismatch", func(m map[string]any) { m["sessionId"] = "wrong" }},
		{"identity wrong type", func(m map[string]any) { m["sessionId"] = 42 }},
		{"cli identity wrong type", func(m map[string]any) { m["cliSessionId"] = 42 }},
		{"selected mixed elements", func(m map[string]any) { m["userSelectedFolders"] = []any{42, "/host/secret", nil} }},
		{"selected malformed approved valid", func(m map[string]any) {
			m["userSelectedFolders"] = nil
			m["userApprovedFileAccessPaths"] = []string{"/host/secret"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			md := goodMetadata("app")
			md["userSelectedFolders"] = []string{"/host/secret"}
			tc.mutate(md)
			fixture(t, root, "a", "w", "app", md)
			r := discover(t, root)
			if len(r.Sessions) != 1 {
				t.Fatal(r)
			}
			m := r.Sessions[0].Link.Mapping
			if m.Known() || !reflect.DeepEqual(m.HostPaths(), []string{"/host/secret"}) {
				t.Fatalf("lost deny evidence or trusted malformed mapping: %+v", m)
			}
		})
	}
}

func TestMetadataOnlyIdentityLinks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{"valid", func(m map[string]any) {}, true},
		{"malformed optional", func(m map[string]any) { m["userApprovedFileAccessPaths"] = nil }, true},
		{"mismatched app", func(m map[string]any) { m["sessionId"] = "other" }, false},
		{"absent app", func(m map[string]any) { delete(m, "sessionId") }, false},
		{"malformed app", func(m map[string]any) { m["sessionId"] = 42 }, false},
		{"absent CLI", func(m map[string]any) { delete(m, "cliSessionId") }, false},
		{"invalid CLI", func(m map[string]any) { m["cliSessionId"] = "arbitrary" }, false},
		{"noncanonical CLI", func(m map[string]any) { m["cliSessionId"] = "urn:uuid:" + nativeID }, false},
		{"nil CLI", func(m map[string]any) { m["cliSessionId"] = "00000000-0000-0000-0000-000000000000" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			md := goodMetadata("app")
			tc.mutate(md)
			b, err := json.Marshal(md)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "a", "w", "app.json")
			put(t, path, string(b))
			r := discover(t, root)
			if len(r.Sessions) != 0 {
				t.Fatal("metadata-only changed native pairing")
			}
			if !tc.want {
				if len(r.IdentityLinks) != 0 {
					t.Fatal("unverified identity linked")
				}
				return
			}
			if len(r.IdentityLinks) != 1 {
				t.Fatal("missing metadata-only identity link")
			}
			l := r.IdentityLinks[0]
			if l.NativeSessionID != nativeID || l.CLISessionID != nativeID || l.SessionID != "app" || l.MetadataPath != path || l.ProjectDir != "" || !reflect.DeepEqual(l.Mapping.HostPaths(), []string{"/host/deleted/repo"}) {
				t.Fatalf("link: %+v", l)
			}
			if tc.name == "malformed optional" && l.Mapping.Known() {
				t.Fatal("malformed mapping trusted")
			}
		})
	}
}

func TestMultipleAccountsFixedDepth(t *testing.T) {
	root := t.TempDir()
	fixture(t, root, "a1", "w1", "s1", goodMetadata("s1"))
	fixture(t, root, "a2", "w2", "s2", goodMetadata("s2"))
	// A similar layout buried deeper is not a supported container session.
	fixture(t, filepath.Join(root, "a3", "w3", "extra"), "buried", "deeper", "s3", goodMetadata("s3"))
	r := discover(t, root)
	if len(r.Sessions) != 2 {
		t.Fatalf("fixed-depth discovery: %d", len(r.Sessions))
	}
}

func TestSymlinkBoundaries(t *testing.T) {
	for _, level := range []string{"root", "account", "workspace", "session", "claude", "projects", "project", "transcript", "metadata", "companion", "subagent-meta"} {
		t.Run(level, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "root")
			project, transcript := fixture(t, root, "a", "w", "app", goodMetadata("app"))
			outside := t.TempDir()
			var victim string
			switch level {
			case "root":
				victim = root
			case "account":
				victim = filepath.Join(root, "a")
			case "workspace":
				victim = filepath.Join(root, "a", "w")
			case "session":
				victim = filepath.Join(root, "a", "w", "app")
			case "claude":
				victim = filepath.Join(root, "a", "w", "app", ".claude")
			case "projects":
				victim = filepath.Dir(project)
			case "project":
				victim = project
			case "transcript":
				victim = transcript
			case "metadata":
				victim = filepath.Join(root, "a", "w", "app.json")
			case "companion":
				victim = filepath.Join(project, nativeID, "tool-results", "secret.txt")
				put(t, victim, "synthetic")
			case "subagent-meta":
				put(t, filepath.Join(project, nativeID, "subagents", "agent-abc.jsonl"), "synthetic")
				victim = filepath.Join(project, nativeID, "subagents", "agent-abc.meta.json")
				put(t, victim, "{}")
			}
			st, err := os.Lstat(victim)
			if err != nil {
				t.Fatal(err)
			}
			target := outside
			if !st.IsDir() {
				target = filepath.Join(outside, "secret")
				put(t, target, `{"sessionId":"app","cliSessionId":"`+nativeID+`","userSelectedFolders":["/host/repo"]}`)
			}
			if err = os.RemoveAll(victim); err != nil {
				t.Fatal(err)
			}
			if err = os.Symlink(target, victim); err != nil {
				t.Fatal(err)
			}
			r := discover(t, root)
			switch level {
			case "metadata":
				if len(r.Sessions) != 1 || r.Sessions[0].Link.Mapping.Known() {
					t.Fatal("symlink metadata trusted")
				}
			case "companion":
				if len(r.Sessions) != 1 || len(r.Sessions[0].Session.Companions) != 0 {
					t.Fatal("symlink companion collected")
				}
			case "subagent-meta":
				if len(r.Sessions) != 1 || len(r.Sessions[0].Session.Subagents) != 1 || r.Sessions[0].Session.Subagents[0].MetaPath != "" {
					t.Fatal("symlink subagent metadata collected")
				}
			default:
				if len(r.Sessions) != 0 {
					t.Fatalf("symlink %s accepted", level)
				}
			}
		})
	}
}
