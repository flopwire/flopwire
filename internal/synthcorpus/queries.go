package synthcorpus

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Query is one entry of the generated query set, in the format of
// testdata/acceptance/queries.yaml (read by `flopwire bench acceptance`).
type Query struct {
	Name     string `yaml:"name"`
	Verb     string `yaml:"verb"`
	Query    string `yaml:"query"`
	Regex    bool   `yaml:"regex,omitempty"`
	Repo     string `yaml:"repo,omitempty"`
	Agent    string `yaml:"agent,omitempty"`
	Since    string `yaml:"since,omitempty"`
	Until    string `yaml:"until,omitempty"`
	Limit    int    `yaml:"limit,omitempty"`
	Expect   Expect `yaml:"expect"`
	Evidence string `yaml:"evidence"`

	needles []string // needle keys whose plants this query counts
	planned int      // plants scheduled
}

// Expect is what a query must return.
type Expect struct {
	MinHits  int      `yaml:"min_hits"`
	MaxHits  int      `yaml:"max_hits,omitempty"`
	Sessions []string `yaml:"sessions"`
}

// until bounds every query: the corpus window ends 2026-09-28.
const until = "2026-09-29T00:00:00Z"

// Needles. Each holds a q, x, z or j (see techWords), so the vocabulary
// never produces one by chance.
const (
	needlePhrase   = "quokka saffron"
	needlePath     = "internal/zircon/jaxflux.go"
	needleRepo     = "zephyrine"
	needleSince    = "xenolith"
	needleAgent    = "QUAZAR_LIMIT"
	needleRegexPre = "ZQ-"
	needleDevin    = "jacquardloom"
	needleSub      = "quillfeather"
	needleBig      = "xebec-bigfile-marker"
	needleTool     = "jinxed-tool-sentinel"
)

type pick struct {
	f *fileSpec
	d *devinSession
}

func (k pick) sessionID() string {
	if k.d != nil {
		return k.d.id
	}
	if k.f.isSub() {
		return "agent-" + k.f.agentID
	}
	return k.f.id
}

// plantQueries schedules the needles of every query into chosen files and
// returns the query set.
func (p *plan) plantQueries() []*Query {
	r := newRNG(p.cfg.Seed, 1)
	var claude, codex, subs []*fileSpec
	for _, f := range p.files {
		switch {
		case f.agent == agentCodex && f.size >= 16<<10:
			codex = append(codex, f)
		case f.isSub() && f.size >= 8<<10:
			subs = append(subs, f)
		case !f.isSub() && f.agent == agentClaude && f.size >= 16<<10:
			claude = append(claude, f)
		}
	}
	used := map[*fileSpec]bool{}
	// The largest file is reserved for the big-file query.
	var big *fileSpec
	for _, f := range p.files {
		if !f.isSub() && f.size >= p.cfg.BigMin && (big == nil || f.size > big.size) {
			big = f
		}
	}
	if big != nil {
		used[big] = true
	}
	// The repo filter uses the repo with the most eligible sessions.
	perRepo := map[string]int{}
	for _, f := range append(append([]*fileSpec(nil), claude...), codex...) {
		perRepo[f.cwd]++
	}
	repoCwd := cwdOf(repos[0])
	for _, name := range repos {
		if perRepo[cwdOf(name)] > perRepo[repoCwd] {
			repoCwd = cwdOf(name)
		}
	}
	take := func(from []*fileSpec, n int, ok func(*fileSpec) bool) []*fileSpec {
		var out []*fileSpec
		for _, i := range r.Perm(len(from)) {
			f := from[i]
			if len(out) == n {
				break
			}
			if used[f] || (ok != nil && !ok(f)) {
				continue
			}
			used[f] = true
			out = append(out, f)
		}
		return out
	}
	var qs []*Query
	add := func(q *Query, needle string, per int, tool bool, files ...*fileSpec) {
		for _, f := range files {
			for range per {
				f.plants = append(f.plants, plant{text: needle, needle: needle, tool: tool})
				q.planned++
			}
		}
		if !slices.Contains(q.needles, needle) {
			q.needles = append(q.needles, needle)
		}
	}
	sessions := func(fs []*fileSpec) []string {
		var out []string
		for _, f := range fs {
			out = append(out, pick{f: f}.sessionID())
		}
		return out
	}

	// A Codex session id quoted in a Claude session.
	if c, x := take(claude, 1, nil), take(codex, 1, nil); len(c) == 1 && len(x) == 1 {
		q := &Query{Name: "session-uuid", Verb: "find", Query: x[0].id, Limit: 50}
		add(q, x[0].id, 3, false, c...)
		q.Expect = Expect{MinHits: 3, Sessions: sessions(c)}
		qs = append(qs, q)
	}
	// Claude tool output.
	if c := take(claude, 3, nil); len(c) > 0 {
		q := &Query{Name: "tool-output", Verb: "find", Query: needleTool, Limit: 50}
		add(q, needleTool, 2, true, c...)
		q.Expect = Expect{MinHits: q.planned, Sessions: sessions(c)}
		qs = append(qs, q)
	}
	// A rare phrase across agents (ranked search).
	if fs := append(take(claude, 2, nil), take(codex, 3, nil)...); len(fs) > 0 {
		q := &Query{Name: "rare-phrase", Verb: "search", Query: needlePhrase, Limit: 100}
		add(q, needlePhrase, 2, false, fs...)
		q.Expect = Expect{MinHits: q.planned}
		qs = append(qs, q)
	}
	// An exact path in three sessions.
	if fs := append(take(claude, 2, nil), take(codex, 1, nil)...); len(fs) > 0 {
		q := &Query{Name: "exact-path", Verb: "find", Query: needlePath, Limit: 50}
		add(q, needlePath, 3, false, fs...)
		q.Expect = Expect{MinHits: q.planned, Sessions: sessions(fs)}
		qs = append(qs, q)
	}
	// A repo filter: the needle is in the repo and outside it.
	inRepo := func(f *fileSpec) bool { return f.cwd == repoCwd }
	notRepo := func(f *fileSpec) bool { return f.cwd != repoCwd }
	if in := append(take(claude, 2, inRepo), take(codex, 2, inRepo)...); len(in) > 0 {
		q := &Query{Name: "repo-filter", Verb: "search", Query: needleRepo, Repo: repoCwd, Limit: 100}
		add(q, needleRepo, 3, false, in...)
		inside := q.planned
		out := append(take(claude, 2, notRepo), take(codex, 2, notRepo)...)
		add(q, needleRepo, 3, false, out...)
		q.Expect = Expect{MinHits: inside, MaxHits: inside, Sessions: sessions(in)}
		q.Evidence = fmt.Sprintf("%d messages in %s, %d outside it", inside, repoCwd, q.planned-inside)
		qs = append(qs, q)
	}
	// A since filter: half the plants are before the cut, half after.
	cut := time.Unix(windowStartUnix, 0).UTC().AddDate(0, 0, windowDays/2)
	after := func(f *fileSpec) bool { return !f.start.Before(cut.Add(time.Hour)) }
	before := func(f *fileSpec) bool { return f.start.Add(7 * time.Hour).Before(cut) }
	if a, b := append(take(claude, 2, after), take(codex, 2, after)...), append(take(claude, 2, before), take(codex, 2, before)...); len(a) > 0 {
		q := &Query{Name: "since-filter", Verb: "search", Query: needleSince, Since: cut.Format(time.RFC3339), Limit: 100}
		add(q, needleSince, 2, false, a...)
		n := q.planned
		add(q, needleSince, 2, false, b...)
		q.Expect = Expect{MinHits: n, MaxHits: n, Sessions: sessions(a)}
		q.Evidence = fmt.Sprintf("%d messages after %s, %d before", n, q.Since, q.planned-n)
		qs = append(qs, q)
	}
	// An agent filter.
	if x, c := take(codex, 3, nil), take(claude, 2, nil); len(x) > 0 {
		q := &Query{Name: "agent-filter", Verb: "find", Query: needleAgent, Agent: "codex", Limit: 50}
		add(q, needleAgent, 2, false, x...)
		n := q.planned
		add(q, needleAgent, 2, false, c...)
		q.Expect = Expect{MinHits: n, MaxHits: n, Sessions: sessions(x)}
		q.Evidence = fmt.Sprintf("%d messages in codex, %d in claude", n, q.planned-n)
		qs = append(qs, q)
	}
	// A regex over distinct codes.
	if fs := append(take(claude, 2, nil), take(codex, 3, nil)...); len(fs) > 0 {
		q := &Query{Name: "regex-code", Verb: "find", Query: needleRegexPre + "[0-9]{4}", Regex: true, Limit: 50}
		for i, f := range fs {
			for k := range 2 {
				add(q, fmt.Sprintf("%s%04d", needleRegexPre, 1000+37*i+k), 1, false, f)
			}
		}
		q.Expect = Expect{MinHits: q.planned}
		qs = append(qs, q)
	}
	// A subagent transcript.
	if s := take(subs, 3, nil); len(s) > 0 {
		q := &Query{Name: "subagent", Verb: "find", Query: needleSub, Limit: 50}
		add(q, needleSub, 1, false, s...)
		q.Expect = Expect{MinHits: q.planned, Sessions: sessions(s)}
		qs = append(qs, q)
	}
	// Late in the largest file.
	if big != nil {
		q := &Query{Name: "big-file", Verb: "find", Query: needleBig, Limit: 50}
		big.plants = append(big.plants, plant{text: needleBig, needle: needleBig, at: 0.9}, plant{text: needleBig, needle: needleBig, at: 0.95})
		q.needles, q.planned = []string{needleBig}, 2
		q.Expect = Expect{MinHits: 2, Sessions: []string{big.id}}
		qs = append(qs, q)
	}
	// Devin.
	if len(p.devin) >= 2 {
		q := &Query{Name: "devin", Verb: "find", Query: needleDevin, Limit: 50}
		var ids []string
		for _, d := range p.devin[:2] {
			d.plants = append(d.plants, needleDevin, needleDevin)
			ids = append(ids, d.id)
		}
		q.needles, q.planned = []string{needleDevin}, 4
		q.Expect = Expect{MinHits: 4, Sessions: ids}
		qs = append(qs, q)
	}
	// A very common term: a latency check for ranked search.
	qs = append(qs, &Query{Name: "common-term", Verb: "search", Query: "retry", Limit: 100, Expect: Expect{MinHits: 100},
		Evidence: "a tech word of the generator's vocabulary; every session holds it"})

	// Spread each file's plants over the file.
	for _, f := range p.files {
		k := 0
		for i := range f.plants {
			if f.plants[i].at == 0 {
				k++
			}
		}
		j := 0
		for i := range f.plants {
			if f.plants[i].at == 0 {
				j++
				f.plants[i].at = float64(j) / float64(k+1)
			}
		}
		slices.SortStableFunc(f.plants, func(a, b plant) int {
			switch {
			case a.at < b.at:
				return -1
			case a.at > b.at:
				return 1
			}
			return 0
		})
	}
	for _, q := range qs {
		q.Until = until
		if q.Evidence == "" && q.planned > 0 {
			q.Evidence = fmt.Sprintf("planted in %d messages", q.planned)
		}
	}
	return qs
}

func writeQueries(path string, qs []*Query) error {
	var b strings.Builder
	b.WriteString("# Generated by internal/synthcorpus: needles planted in known messages.\n")
	data, err := yaml.Marshal(qs)
	if err != nil {
		return err
	}
	b.Write(data)
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
