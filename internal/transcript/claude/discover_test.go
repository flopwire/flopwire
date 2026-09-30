package claude

import (
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestProjectsRoot(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := ProjectsRoot(getenv, "/home/me"); got != "/home/me/.claude/projects" {
		t.Fatalf("default root %s", got)
	}
	env["CLAUDE_CONFIG_DIR"] = "/cfg"
	if got := ProjectsRoot(getenv, "/home/me"); got != "/cfg/projects" {
		t.Fatalf("CLAUDE_CONFIG_DIR root %s", got)
	}
}

func TestDiscoverFixture(t *testing.T) {
	sessions, err := Discover(projects)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Session{}
	for _, s := range sessions {
		byID[s.SessionID] = s
	}
	if len(sessions) != 3 {
		t.Fatalf("sessions %d", len(sessions))
	}

	beta := byID[betaID]
	if beta == nil || beta.Transcript != betaMain || beta.Orphaned() || beta.Stub() != nil {
		t.Fatalf("beta %+v", beta)
	}
	if len(beta.Subagents) != 3 {
		t.Fatalf("beta subagents %+v", beta.Subagents)
	}
	wf := beta.Subagents[2]
	if wf.AgentID != "b000000000000001" || wf.RunID != "wf_beta-001" || filepath.Base(wf.MetaPath) != "agent-b000000000000001.meta.json" {
		t.Fatalf("workflow subagent %+v", wf)
	}
	roles := map[string]CompanionRole{}
	for _, c := range beta.Companions {
		rel, _ := filepath.Rel(betaDir, c.Path)
		roles[filepath.ToSlash(rel)] = c.Role
	}
	want := map[string]CompanionRole{
		"subagents/agent-b000000000000002.meta.json":                       CompanionMeta,
		"subagents/agent-b000000000000003.meta.json":                       CompanionMeta,
		"subagents/workflows/wf_beta-001/agent-b000000000000001.meta.json": CompanionMeta,
		"subagents/workflows/wf_beta-001/journal.jsonl":                    CompanionOther,
		"tool-results/bpersist01.txt":                                      CompanionToolResultText,
		"tool-results/shot01.png":                                          CompanionToolResultBinary,
		"workflows/wf_beta-001.json":                                       CompanionOther,
	}
	if len(roles) != len(want) {
		t.Fatalf("companions %v", roles)
	}
	for k, v := range want {
		if roles[k] != v {
			t.Errorf("companion %s role %q, want %q", k, roles[k], v)
		}
	}
	srcs := beta.Sources()
	if len(srcs) != 4 || srcs[0].SessionKey != betaID || srcs[1].SessionKey != "agent-b000000000000002" ||
		srcs[0].Parser != ParserName || srcs[0].StorageKind != transcript.StorageJSONLAppend {
		t.Fatalf("sources %+v", srcs)
	}

	orphan := byID["0b7e2c1a-0000-4000-8000-00000000000f"]
	if orphan == nil || !orphan.Orphaned() || len(orphan.Sources()) != 0 || len(orphan.Companions) != 1 ||
		orphan.Companions[0].Role != CompanionToolResultText || !orphan.Companions[0].Indexed() {
		t.Fatalf("orphan %+v", orphan)
	}
	if st := orphan.Stub(); st == nil || st.SessionID != orphan.SessionID || st.Agent != transcript.AgentClaude {
		t.Fatalf("stub %+v", st)
	}

	if s, err := Discover(filepath.Join(projects, "missing")); err != nil || s != nil {
		t.Fatalf("missing root: %v %v", s, err)
	}
}
