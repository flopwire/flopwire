package desktopcode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript/claude"
)

const appID = "local_11111111-1111-4111-8111-111111111111"
const cliID = "22222222-2222-4222-8222-222222222222"
const archivedID = "33333333-3333-4333-8333-333333333333"

func put(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func record(t *testing.T, root, id string, fields map[string]any) string {
	t.Helper()
	fields["sessionId"] = id
	if _, exists := fields["cwd"]; !exists {
		fields["cwd"] = "/tmp/synthetic-code-probe"
	}
	b, e := json.Marshal(fields)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(root, "acct", "org", id+".json")
	put(t, p, string(b))
	return p
}
func scoped(root, id string) string {
	return filepath.Join(root, "acct", "org", id, ".claude", "projects")
}
func discover(t *testing.T, root, normal string) Result {
	t.Helper()
	r, e := Discover(root, normal)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestScopedNativeSessionsAndMetadataExclusion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	meta := record(t, root, appID, map[string]any{"cliSessionId": cliID, "initialPrompt": "PRIVATE_APP_METADATA", "stagedTranscriptPath": "/must/not/follow"})
	project := filepath.Join(scoped(root, appID), "-tmp-probe")
	main := filepath.Join(project, cliID+".jsonl")
	put(t, main, "")
	agent := filepath.Join(project, cliID, "subagents", "agent-abc.jsonl")
	put(t, agent, "")
	tool := filepath.Join(project, cliID, "tool-results", "one.txt")
	put(t, tool, "synthetic tool")
	put(t, filepath.Join(root, "acct", "org", appID, "audit.jsonl"), "not native")
	put(t, filepath.Join(root, "acct", "org", appID, "uploads", "secret.txt"), "not companion")
	r := discover(t, root, "")
	if len(r.Sessions) != 1 || len(r.IdentityLinks) != 1 || r.MetadataOnly != 0 {
		t.Fatalf("counts %+v", r)
	}
	s := r.Sessions[0]
	if s.Session.SessionID != cliID || s.Session.Transcript != main || len(s.Session.Subagents) != 1 || s.Session.Subagents[0].Path != agent {
		t.Fatalf("native session %+v", s)
	}
	if len(s.Session.Companions) != 1 || s.Session.Companions[0].Path != tool || s.Session.Companions[0].Role != claude.CompanionToolResultText {
		t.Fatalf("companions %+v", s.Session.Companions)
	}
	if s.Link.MetadataPath != meta || s.Link.ScopedTranscripts != 1 || !reflect.DeepEqual(s.Link.CLISessionIDs, []string{cliID}) {
		t.Fatalf("link %+v", s.Link)
	}
	for _, c := range s.Session.Companions {
		if c.Path == meta {
			t.Fatal("metadata returned as companion")
		}
	}
	if !SafeFile(root, main) || !SafeFile(root, agent) {
		t.Fatal("native files failed containment")
	}
	// Each paired link owns its slice; altering a consumer snapshot must not
	// mutate the metadata-only diagnostic records.
	s.Link.CLISessionIDs[0] = "changed"
	if r.IdentityLinks[0].CLISessionIDs[0] != cliID {
		t.Fatal("aliased linkage snapshot")
	}
}

func TestConfiguredNormalRootAndArchivedLinkage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	normal := filepath.Join(t.TempDir(), "custom", "projects")
	record(t, root, appID, map[string]any{"cliSessionId": cliID, "unarchivedCliSessionId": archivedID})
	put(t, filepath.Join(normal, "-host-repo", archivedID+".jsonl"), "")
	r := discover(t, root, normal)
	if len(r.Sessions) != 0 || r.MetadataOnly != 0 || len(r.IdentityLinks) != 1 || r.IdentityLinks[0].NormalTranscripts != 1 || r.NormalRootState != RootAvailable {
		t.Fatalf("configured root linkage %+v", r)
	}
	r = discover(t, root, filepath.Join(t.TempDir(), "other", "projects"))
	if r.MetadataOnly != 1 || r.IdentityLinks[0].NormalTranscripts != 0 || r.NormalRootState != RootMissing {
		t.Fatalf("ignored configured root %+v", r)
	}
}

func TestDiscoveryAppearingAfterStartup(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "Claude", "claude-code-sessions")
	r := discover(t, root, "")
	if r.Unavailable != 1 || !reflect.DeepEqual(r.WatchDirs, []string{parent}) {
		t.Fatalf("absent %+v", r)
	}
	record(t, root, appID, map[string]any{"cliSessionId": cliID})
	r = discover(t, root, "")
	if r.Unavailable != 0 || r.MetadataOnly != 1 || len(r.IdentityLinks) != 1 {
		t.Fatalf("metadata only %+v", r)
	}
	put(t, filepath.Join(scoped(root, appID), "one", cliID+".jsonl"), "")
	r = discover(t, root, "")
	if len(r.Sessions) != 1 || r.MetadataOnly != 0 {
		t.Fatalf("new scoped root %+v", r)
	}
	if !contains(r.WatchDirs, scoped(root, appID)) {
		t.Fatal("new projects root not watched")
	}
}

func TestBoundedLayoutAndArbitraryStagedPathIgnored(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	put(t, outside, "")
	record(t, root, appID, map[string]any{"cliSessionId": cliID, "stagedTranscriptPath": outside})
	put(t, filepath.Join(root, "acct", "org", "unrelated", "deep", appID, ".claude", "projects", "one", cliID+".jsonl"), "")
	put(t, filepath.Join(root, "acct", "org", "ordinary-chat", "projects", "one", cliID+".jsonl"), "")
	r := discover(t, root, "")
	if len(r.Sessions) != 0 || r.MetadataOnly != 1 {
		t.Fatalf("unexpected recursive or staged discovery %+v", r)
	}
	if SafeFile(root, outside) {
		t.Fatal("outside file accepted")
	}
	r = discover(t, "", "")
	if !reflect.DeepEqual(r, Result{}) {
		t.Fatalf("disabled %+v", r)
	}
}

func TestMalformedMetadataCannotEstablishScopedCodeScope(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	meta := filepath.Join(root, "acct", "org", appID+".json")
	put(t, meta, `{"sessionId":"wrong","cliSessionId":"`+cliID+`"}`)
	put(t, filepath.Join(scoped(root, appID), "one", cliID+".jsonl"), "")
	for _, s := range []string{`{"sessionId":"wrong","cliSessionId":"` + cliID + `"}`, `{`, `{"sessionId":"` + appID + `","cliSessionId":7}`, `{"sessionId":"` + appID + `","cliSessionId":"` + cliID + `"} {}`, strings.Repeat(" ", maxMetadataBytes+1)} {
		put(t, meta, s)
		r := discover(t, root, "")
		if len(r.Sessions) != 0 || len(r.IdentityLinks) != 0 || r.UnreadableMetadata != 1 {
			t.Fatalf("malformed metadata outcome %+v", r)
		}
	}
}

func TestSymlinkBoundaries(t *testing.T) {
	for _, location := range []string{"root", "account", "org", "session", "config", "projects", "project", "main", "metadata", "companion", "subagent"} {
		t.Run(location, func(t *testing.T) {
			temp := t.TempDir()
			root := filepath.Join(temp, "code")
			outside := filepath.Join(temp, "outside")
			meta := record(t, root, appID, map[string]any{"cliSessionId": cliID})
			project := filepath.Join(scoped(root, appID), "one")
			main := filepath.Join(project, cliID+".jsonl")
			put(t, main, "")
			companion := filepath.Join(project, cliID, "tool-results", "one.txt")
			put(t, companion, "")
			subagent := filepath.Join(project, cliID, "subagents", "agent-abc.jsonl")
			put(t, subagent, "")
			paths := map[string]string{"root": root, "account": filepath.Join(root, "acct"), "org": filepath.Join(root, "acct", "org"), "session": filepath.Join(root, "acct", "org", appID), "config": filepath.Join(root, "acct", "org", appID, ".claude"), "projects": scoped(root, appID), "project": project, "main": main, "metadata": meta, "companion": companion, "subagent": subagent}
			path := paths[location]
			st, e := os.Stat(path)
			if e != nil {
				t.Fatal(e)
			}
			target := outside
			if st.IsDir() {
				if e = os.MkdirAll(target, 0700); e != nil {
					t.Fatal(e)
				}
			} else {
				put(t, target, `{"sessionId":"`+appID+`","cliSessionId":"`+cliID+`"}`)
			}
			if e = os.RemoveAll(path); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink(target, path); e != nil {
				t.Fatal(e)
			}
			r := discover(t, root, "")
			if location == "metadata" {
				if r.UnreadableMetadata != 1 {
					t.Fatalf("symlink metadata accepted %+v", r)
				}
			} else if location != "project" && location != "main" && location != "companion" && location != "subagent" && r.Excluded == 0 {
				t.Fatalf("symlink exclusion not reported %+v", r)
			}
			if SafeFile(root, path) {
				t.Fatal("symlink accepted as safe")
			}
			for _, s := range r.Sessions {
				for _, source := range s.Session.Sources() {
					if !SafeFile(root, source.Path) {
						t.Fatalf("unsafe returned source %s", source.Path)
					}
				}
				for _, c := range s.Session.Companions {
					if !SafeFile(root, c.Path) {
						t.Fatal("unsafe returned companion")
					}
				}
			}
		})
	}
}
func contains(v []string, w string) bool {
	for _, s := range v {
		if s == w {
			return true
		}
	}
	return false
}

func TestUnreadableNormalRootDoesNotSuppressScopedDiscovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	record(t, root, appID, map[string]any{"cliSessionId": cliID})
	put(t, filepath.Join(scoped(root, appID), "one", cliID+".jsonl"), "")
	// A regular file is deterministically unlistable, including under root users.
	normal := filepath.Join(t.TempDir(), "not-a-directory")
	put(t, normal, "")
	r := discover(t, root, normal)
	if r.NormalRootState != RootUnreadable || len(r.Sessions) != 1 || r.MetadataOnly != 0 {
		t.Fatalf("scoped collection suppressed %+v", r)
	}
	if e := os.RemoveAll(filepath.Join(root, "acct", "org", appID)); e != nil {
		t.Fatal(e)
	}
	r = discover(t, root, normal)
	if r.MetadataOnly != 0 || len(r.IdentityLinks) != 1 {
		t.Fatalf("claimed absent transcript under unreadable normal root %+v", r)
	}
}

func TestLocalCodeMetadataScope(t *testing.T) {
	cases := []struct {
		name     string
		fields   map[string]any
		accepted bool
	}{
		{"legacy-local", map[string]any{}, true},
		{"explicit-local", map[string]any{"backend": "local", "sessionType": "code"}, true},
		{"runtime-object-local", map[string]any{"backend": map[string]any{"kind": "local"}}, true},
		{"code-origin", map[string]any{"importedFrom": "local-1p-code"}, true},
		{"terminal-adoption", map[string]any{"importedFrom": "terminal-cli"}, true},
		{"sibling-code", map[string]any{"importedFrom": "sibling-1p-code"}, true},
		{"chat-type", map[string]any{"sessionType": "chat"}, false},
		{"chat-origin", map[string]any{"importedFrom": "local-1p-chat", "backend": "local"}, false},
		{"web-chat-origin", map[string]any{"importedFrom": "claude-ai-chat"}, false},
		{"sibling-chat-origin", map[string]any{"importedFrom": "sibling-3p-chat"}, false},
		{"cowork-container-mixup", map[string]any{"sessionType": "cowork"}, false},
		{"cowork-origin", map[string]any{"importedFrom": "local-1p-cowork"}, false},
		{"sibling-cowork-origin", map[string]any{"importedFrom": "sibling-1p-cowork"}, false},
		{"ssh-persisted", map[string]any{"sshConfig": map[string]any{"host": "synthetic.invalid"}}, false},
		{"wsl-persisted", map[string]any{"wslConfig": map[string]any{"distro": "synthetic"}}, false},
		{"remote-backend", map[string]any{"backend": map[string]any{"kind": "ssh"}}, false},
		{"unknown-backend", map[string]any{"backend": "future-backend"}, false},
		{"unknown-origin", map[string]any{"importedFrom": "future-surface"}, false},
		{"missing-host-shape", map[string]any{"cwd": ""}, false},
		{"vm-host-shape", map[string]any{"cwd": "/sessions/synthetic/mnt/repo"}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "code")
			tt.fields["cliSessionId"] = cliID
			record(t, root, appID, tt.fields)
			put(t, filepath.Join(scoped(root, appID), "one", cliID+".jsonl"), "synthetic transcript content is never read in discovery")
			r := discover(t, root, "")
			if tt.accepted {
				if len(r.Sessions) != 1 || len(r.IdentityLinks) != 1 || r.OutOfScope != 0 {
					t.Fatalf("local Code rejected %+v", r)
				}
			} else {
				if len(r.Sessions) != 0 || len(r.IdentityLinks) != 0 || r.OutOfScope != 1 {
					t.Fatalf("non-Code/non-local metadata admitted %+v", r)
				}
				if contains(r.WatchDirs, scoped(root, appID)) {
					t.Fatal("out-of-scope transcript root watched")
				}
			}
		})
	}
}

func TestImportedCoworkAliasesSurviveMissingOriginalContainer(t *testing.T) {
	for _, origin := range []string{"local-1p-cowork", "sibling-1p-cowork", "sibling-3p-cowork", "previous-profile-cowork"} {
		t.Run(origin, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "claude-code-sessions")
			normal := filepath.Join(base, "configured-cli", "projects")
			// No local-agent-mode-sessions original container is created. Code cwd is
			// deliberately untrusted as Cowork folder proof, including absent host cwd.
			meta := record(t, root, appID, map[string]any{"cliSessionId": cliID, "unarchivedCliSessionId": archivedID, "importedFrom": origin, "cwd": ""})
			put(t, filepath.Join(normal, "one", cliID+".jsonl"), "synthetic Cowork copy")
			put(t, filepath.Join(normal, "two", archivedID+".jsonl"), "synthetic archived Cowork copy")
			// Even a scoped candidate must not be collected or inspected as Code.
			put(t, filepath.Join(scoped(root, appID), "one", cliID+".jsonl"), "synthetic scoped Cowork copy")
			r := discover(t, root, normal)
			if len(r.Sessions) != 0 || len(r.IdentityLinks) != 0 || r.OutOfScope != 1 || r.MetadataOnly != 0 {
				t.Fatalf("Cowork promoted to Code %+v", r)
			}
			if len(r.CoworkOriginAliases) != 1 {
				t.Fatalf("lost Cowork provenance %+v", r)
			}
			a := r.CoworkOriginAliases[0]
			if a.SessionID != appID || a.MetadataPath != meta || a.ImportedFrom != origin || a.NormalTranscripts != 2 || !reflect.DeepEqual(a.CLISessionIDs, []string{cliID, archivedID}) {
				t.Fatalf("wrong provenance %+v", a)
			}
			if contains(r.WatchDirs, scoped(root, appID)) {
				t.Fatal("Cowork scoped bodies watched as Code")
			}
		})
	}
}

func TestChatImportsCannotBecomeCoworkPolicyAliases(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	record(t, root, appID, map[string]any{"cliSessionId": cliID, "importedFrom": "local-1p-chat"})
	r := discover(t, root, "")
	if len(r.CoworkOriginAliases) != 0 || r.OutOfScope != 1 {
		t.Fatalf("chat origin misclassified %+v", r)
	}
}

func TestScopedParentsMustMatchMetadataCLIIdentities(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	record(t, root, appID, map[string]any{"cliSessionId": cliID, "unarchivedCliSessionId": archivedID})
	project := filepath.Join(scoped(root, appID), "one")
	unrelated := "44444444-4444-4444-8444-444444444444"
	for _, id := range []string{cliID, archivedID, unrelated} {
		put(t, filepath.Join(project, id+".jsonl"), "")
		put(t, filepath.Join(project, id, "subagents", "agent-"+id[:8]+".jsonl"), "")
		put(t, filepath.Join(project, id, "tool-results", "one.txt"), "")
	}
	r := discover(t, root, "")
	if len(r.Sessions) != 2 || len(r.IdentityLinks) != 1 || r.IdentityLinks[0].ScopedTranscripts != 2 {
		t.Fatalf("unrelated parents gained Code provenance %+v", r)
	}
	got := map[string]bool{}
	for _, session := range r.Sessions {
		got[session.Session.SessionID] = true
		if len(session.Session.Subagents) != 1 || len(session.Session.Companions) != 1 {
			t.Fatalf("matched parent descendants lost %+v", session.Session)
		}
		for _, source := range session.Session.Sources() {
			if strings.Contains(source.Path, unrelated) {
				t.Fatal("unrelated parent or subagent admitted")
			}
		}
	}
	if !got[cliID] || !got[archivedID] || got[unrelated] {
		t.Fatalf("wrong returned parents %+v", got)
	}
}
