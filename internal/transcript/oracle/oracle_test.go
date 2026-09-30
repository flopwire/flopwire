package oracle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

const expectedDir = "../../../testdata/oracle/expected"
const homeDir = "../../../testdata/oracle/home"

func TestExpectedFilesLoadAndPointAtFixtures(t *testing.T) {
	files, err := Load(expectedDir)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, f := range files {
		counts[f.Connector]++
		if len(f.Conversations) == 0 {
			t.Errorf("%s: no conversations", f.Name)
		}
		p := RealPath(homeDir, f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: source %s missing", f.Name, p)
		}
		for _, c := range f.Conversations {
			if c.SourcePath != f.SourcePath || strings.HasPrefix(c.SourcePath, "/") {
				t.Errorf("%s: conversation source %q not relative to home", f.Name, c.SourcePath)
			}
		}
	}
	for _, agent := range []string{"claude", "codex", "devin"} {
		if counts[agent] < 2 {
			t.Errorf("want >= 2 expected files for %s, got %d (run scripts/regen-oracle.sh)", agent, counts[agent])
		}
	}
}

// childMessages hand-builds what a Codex parser should emit for the
// subagent rollout fixture, to exercise Diff without a real parser.
func childMessages() (*transcript.Conversation, []*transcript.Message) {
	ts := func(ms int64) time.Time { return time.UnixMilli(ms) }
	conv := &transcript.Conversation{Agent: transcript.AgentCodex, SessionID: "019a0000-0000-7000-8000-00000000c0df", Cwd: "/tmp/oracle-alpha"}
	msgs := []*transcript.Message{
		{Kind: transcript.KindUser, Text: "review the router change", TS: ts(1790157604100)},
		{Kind: transcript.KindThinking, Text: "not in FAD", TS: ts(1790157604200)},
		{Kind: transcript.KindAssistant, Text: "Looks right;  chi.NewRouter\nreturns *Mux.", TS: ts(1790157604900)},
	}
	return conv, msgs
}

func childExpected(t *testing.T) (File, Conversation) {
	t.Helper()
	files, err := Load(expectedDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ForConnector(files, "codex") {
		for _, c := range f.Conversations {
			if c.Matches("019a0000-0000-7000-8000-00000000c0df") {
				return f, c
			}
		}
	}
	t.Fatal("codex child rollout not in expected files")
	return File{}, Conversation{}
}

func TestDiffParity(t *testing.T) {
	f, exp := childExpected(t)
	conv, msgs := childMessages()
	Assert(t, f, exp, conv, msgs, Rules{})
}

func TestDiffReportsMismatches(t *testing.T) {
	_, exp := childExpected(t)
	conv, msgs := childMessages()
	msgs[2].Text = "something else"
	conv.Cwd = "/elsewhere"
	got := Diff(exp, conv, msgs, Rules{})
	if len(got) != 2 || !strings.Contains(got[0], "cwd") || !strings.Contains(got[1], "row 1 differs") {
		t.Fatalf("problems = %q", got)
	}

	// A documented divergence drops the row on our side.
	msgs = msgs[:2]
	rules := Rules{Divergences: []Divergence{{
		Name:    "fixture: FAD keeps the final assistant reply we dropped",
		DropFAD: func(m Message) bool { return m.Role == "assistant" },
	}}}
	conv.Cwd = "/tmp/oracle-alpha"
	if got := Diff(exp, conv, msgs, rules); len(got) != 0 {
		t.Fatalf("divergence not applied: %q", got)
	}
}

func TestRealPathWalksUpVirtualPaths(t *testing.T) {
	got := RealPath(homeDir, File{SourcePath: ".codex/sessions/2026/09/23/missing/virtual"})
	if filepath.Base(got) != "23" {
		t.Fatalf("RealPath = %s", got)
	}
}
