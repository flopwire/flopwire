package agent

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The server applies the admin rules (internal/ingest) with the matching
// the agent uses: on the same session, placed only by what its transcript
// recorded (no git repository, no symlinks), both reach the same verdict.
func TestServerRuleParity(t *testing.T) {
	const home = "/nonexistent-flopwire/home/me"
	rules := []string{
		"deny ~/clients/acme",
		"local /nonexistent-flopwire/work/private/**",
		"deny repo:github.com/acme/*",
		"secret-proj",
		"allow /nonexistent-flopwire/work/open",
		"local:/nonexistent-flopwire/work/*-scratch",
	}
	claudePath := func(folder string) string { return home + "/.claude/projects/" + folder + "/s.jsonl" }
	codexPath := home + "/.codex/sessions/2026/09/15/rollout-x.jsonl"
	cases := []struct {
		name        string
		agent       transcript.Agent
		path        string
		pl          pathpolicy.Placement
		how         string
		unplaceable string
	}{
		{"home rule", transcript.AgentClaude, claudePath("-x"), pathpolicy.Placement{Cwd: home + "/Clients/ACME/web"}, localindex.PlacedByCwd, ""},
		{"home rule other dir", transcript.AgentClaude, claudePath("-x"), pathpolicy.Placement{Cwd: home + "/clients/acme2"}, localindex.PlacedByCwd, ""},
		// CLAUDE_CONFIG_DIR outside the home: only the reported home works.
		{"home rule, harness elsewhere", transcript.AgentClaude, "/nonexistent-flopwire/data/claude/projects/-x/s.jsonl", pathpolicy.Placement{Cwd: home + "/clients/acme"}, localindex.PlacedByCwd, ""},
		{"local glob", transcript.AgentClaude, claudePath("-x"), pathpolicy.Placement{Cwd: "/nonexistent-flopwire/work/private/a/b"}, localindex.PlacedByCwd, ""},
		{"any depth", transcript.AgentCodex, codexPath, pathpolicy.Placement{Cwd: "/nonexistent-flopwire/x/secret-proj/src"}, localindex.PlacedByCwd, ""},
		{"segment glob", transcript.AgentCodex, codexPath, pathpolicy.Placement{Cwd: "/nonexistent-flopwire/work/my-scratch"}, localindex.PlacedByCwd, ""},
		{"allowed", transcript.AgentCodex, codexPath, pathpolicy.Placement{Cwd: "/nonexistent-flopwire/work/open"}, localindex.PlacedByCwd, ""},
		{"repo by remote", transcript.AgentCodex, codexPath, pathpolicy.Placement{Cwd: "/nonexistent-flopwire/work/app", Remote: "github.com/acme/app"}, localindex.PlacedByCwd, ""},
		{"repo remote only", transcript.AgentCodex, codexPath, pathpolicy.Placement{Remote: "github.com/acme/app"}, localindex.PlacedByRemote, ""},
		{"other remote", transcript.AgentCodex, codexPath, pathpolicy.Placement{Remote: "github.com/other/app"}, localindex.PlacedByRemote, "exclude"},
		{"folder", transcript.AgentClaude, claudePath("-nonexistent-flopwire-x-secret-proj"), pathpolicy.Placement{Cwd: "/nonexistent-flopwire/x/secret-proj"}, localindex.PlacedByFolder, ""},
		{"folder allowed", transcript.AgentClaude, claudePath("-nonexistent-flopwire-x-app"), pathpolicy.Placement{Cwd: "/nonexistent-flopwire/x/app"}, localindex.PlacedByFolder, "exclude"},
		{"unplaceable upload", transcript.AgentCodex, codexPath, pathpolicy.Placement{}, localindex.PlacedByNone, "upload"},
		{"unplaceable local", transcript.AgentCodex, codexPath, pathpolicy.Placement{}, localindex.PlacedByNone, "local"},
		{"unplaceable exclude", transcript.AgentCodex, codexPath, pathpolicy.Placement{}, localindex.PlacedByNone, "exclude"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The user's own setting is the loosest, so the admin floor decides.
			a := &Agent{cfg: Config{Home: home, Unplaceable: "upload"}, adminRaw: AdminPolicy{Rules: rules, Unplaceable: c.unplaceable},
				log: slog.New(slog.NewTextHandler(io.Discard, nil)), phys: map[string]string{}}
			agentD := a.decide(a.buildPolicy(), placed{pl: c.pl, how: c.how, checked: 1})
			cwd, remote := c.pl.Cwd, c.pl.Remote
			if c.how == localindex.PlacedByFolder {
				cwd = "" // the transcript named none; the folder placed it
			}
			// The device reports its home; a device that has not yet is
			// inferred from the path, which agrees while the harness
			// directory is under the home.
			homes := []string{home}
			if strings.HasPrefix(c.path, home+"/") {
				homes = append(homes, "")
			}
			for _, reported := range homes {
				serverD := ingest.AdminDecision(rules, c.unplaceable, reported, string(c.agent), c.path, cwd, remote)
				if agentD.Mode != serverD.Mode {
					t.Fatalf("home %q: agent %s (%s), server %s (%s)", reported, agentD.Mode, agentD.Reason(), serverD.Mode, serverD.Reason())
				}
				if agentD.Mode != pathpolicy.Allow && !agentD.Unplaceable && agentD.Rule.ExpandHome(home) != serverD.Rule.ExpandHome(home) {
					t.Errorf("home %q: agent rule %q, server rule %q", reported, agentD.Rule, serverD.Rule)
				}
			}
		})
	}
}
