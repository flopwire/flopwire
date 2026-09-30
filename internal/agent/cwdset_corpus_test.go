package agent

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// dirsSink keeps the directories each parsed session named.
type dirsSink struct {
	first map[string]string
	other map[string]map[string]bool
}

func (s *dirsSink) Conversation(c *transcript.Conversation) error {
	if c.Cwd != "" && s.first[c.SessionID] == "" {
		s.first[c.SessionID] = c.Cwd
	}
	for _, d := range c.OtherCwds {
		if s.other[c.SessionID] == nil {
			s.other[c.SessionID] = map[string]bool{}
		}
		s.other[c.SessionID][d] = true
	}
	return nil
}

func (s *dirsSink) Message(*transcript.Message) error { return nil }

// TestCorpusCwdSets parses every real Claude and Codex transcript
// (read-only) and reports how many sessions name more than one directory,
// and how many would get a stricter verdict under sample rules when every
// directory counts rather than the first.
//
//	FLOPWIRE_CORPUS=1 go test -run TestCorpusCwdSets -v ./internal/agent/
func TestCorpusCwdSets(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	home, _ := os.UserHomeDir()
	type src struct {
		s transcript.Source
		p transcript.Parser
	}
	var srcs []src
	sessions, err := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	if err != nil {
		t.Fatal(err)
	}
	cp, xp := &claude.Parser{}, &codex.Parser{}
	for _, s := range sessions {
		for _, x := range s.Sources() {
			srcs = append(srcs, src{x, cp})
		}
	}
	cs, err := codex.Discover(codex.Home())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range cs {
		srcs = append(srcs, src{x, xp})
	}
	t0 := time.Now()
	per := map[transcript.Agent]*dirsSink{}
	var bytes int64
	for _, x := range srcs {
		sk := per[x.s.Agent]
		if sk == nil {
			sk = &dirsSink{first: map[string]string{}, other: map[string]map[string]bool{}}
			per[x.s.Agent] = sk
		}
		f, err := os.Open(x.s.Path)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil {
			s := x.s
			_, err = x.p.Parse(context.Background(), transcript.Input{Source: &s, R: f, Size: fi.Size()}, transcript.Cursor{}, sk)
			bytes += fi.Size()
		}
		f.Close()
		if err != nil {
			t.Logf("%s: %v", x.s.Path, err)
		}
	}
	t.Logf("parsed %d files, %.1f GB in %s", len(srcs), float64(bytes)/1e9, time.Since(t0).Round(time.Second))

	// The most common later directory is one sample rule; two generic ones
	// (a scratch area, the harness's own directory) are the others.
	count := map[string]int{}
	for _, sk := range per {
		for _, ds := range sk.other {
			for d := range ds {
				count[d]++
			}
		}
	}
	var top string
	for d, n := range count {
		if n > count[top] || n == count[top] && d < top {
			top = d
		}
	}
	samples := []string{"deny /tmp", "local:~/.claude", "local:~/Code/*/notes"}
	if top != "" {
		samples = append(samples, "deny "+top)
	}
	for _, rule := range samples {
		rules, err := pathpolicy.ParseRules([]string{rule})
		if err != nil {
			t.Fatal(err)
		}
		for i := range rules {
			rules[i] = rules[i].ExpandHome(home)
		}
		pol := pathpolicy.Policy{User: rules, Unplaceable: pathpolicy.Allow}
		for _, agent := range []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex} {
			sk := per[agent]
			if sk == nil {
				continue
			}
			multi, changed := 0, 0
			var examples []string
			for id, ds := range sk.other {
				if len(ds) == 0 {
					continue
				}
				multi++
				was := pol.Decide(pathpolicy.Placement{Cwd: sk.first[id]}).Mode
				now := was
				for d := range ds {
					now = max(now, pol.Decide(pathpolicy.Placement{Cwd: d}).Mode)
				}
				if now > was {
					changed++
					if len(examples) < 3 {
						examples = append(examples, id)
					}
				}
			}
			sort.Strings(examples)
			t.Logf("rule %-28q %-6s sessions=%d with>1cwd=%d stricter=%d e.g. %s", rule, agent, len(sk.first), multi, changed, strings.Join(examples, ","))
		}
	}
}
