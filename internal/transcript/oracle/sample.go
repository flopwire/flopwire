package oracle

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sample parity over real transcripts (A4, spec §11.2). A script copies a
// sample of the device's transcripts into a scratch home and runs fad-dump
// on it (scripts/oracle-sample.sh); each parser package's
// TestOracleCorpusSample diffs its output against FAD's with the same
// divergence rules as the fixture tests and records a Report. Reports stay
// in the scratch directory: their examples quote real transcript text.

// SampleDirEnv names the scratch directory holding home/ and expected/.
const SampleDirEnv = "FLOPWIRE_ORACLE_SAMPLE"

// Report is one agent's sample result.
type Report struct {
	Agent         string              `json:"agent"`
	Files         int                 `json:"files"`
	Conversations int                 `json:"conversations"`
	Matched       int                 `json:"matched"`
	ParseErrors   int                 `json:"parse_errors"`
	Categories    map[string]int      `json:"categories"`
	Examples      map[string][]string `json:"examples"`
	// Normalized counts rows removed from both sides by a documented
	// normalization before comparing (e.g. harness-injected texts).
	Normalized int `json:"normalized_rows,omitempty"`
}

// NewReport starts a report for agent.
func NewReport(agent string) *Report {
	return &Report{Agent: agent, Categories: map[string]int{}, Examples: map[string][]string{}}
}

// Add records one conversation's comparison.
func (r *Report) Add(name string, c Comparison) {
	r.Conversations++
	if len(c.Problems) == 0 {
		r.Matched++
		return
	}
	cat := Classify(c)
	r.Categories[cat]++
	if len(r.Examples[cat]) < 3 {
		r.Examples[cat] = append(r.Examples[cat], name+": "+Excerpt(c))
	}
}

// Missing records a FAD conversation our parser did not emit.
func (r *Report) Missing(name, id string) {
	r.Conversations++
	r.Categories["conversation not emitted by us"]++
	if len(r.Examples["conversation not emitted by us"]) < 3 {
		r.Examples["conversation not emitted by us"] = append(r.Examples["conversation not emitted by us"], name+": "+id)
	}
}

// Fail records a file the parser or the comparison could not handle.
func (r *Report) Fail(name string, err error) {
	r.ParseErrors++
	r.Categories["parse error"]++
	r.Examples["parse error"] = append(r.Examples["parse error"], fmt.Sprintf("%s: %v", name, err))
}

// Write saves the report as <dir>/report-<agent>.json.
func (r *Report) Write(dir string) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report-"+r.Agent+".json"), data, 0o644)
}

// Summary is a one-line result with categories by count.
func (r *Report) Summary() string {
	type kv struct {
		k string
		v int
	}
	var cats []kv
	for k, v := range r.Categories {
		cats = append(cats, kv{k, v})
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].v > cats[j].v || cats[i].v == cats[j].v && cats[i].k < cats[j].k })
	var parts []string
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s: %d", c.k, c.v))
	}
	return fmt.Sprintf("%s: %d/%d conversations match the oracle (%d files, %d parse errors, %d rows normalized away); divergences: %s",
		r.Agent, r.Matched, r.Conversations, r.Files, r.ParseErrors, r.Normalized, strings.Join(parts, ", "))
}

// Classify names the kind of a comparison's first problem.
func Classify(c Comparison) string {
	switch {
	case c.SessionID:
		return "session id"
	case c.Cwd:
		return "cwd vs FAD workspace"
	case c.Row < 0 && len(c.Problems) > 0 && strings.HasPrefix(c.Problems[0], "tool calls"):
		return "tool call count"
	case c.Row < 0:
		return "other"
	}
	a, b := c.FAD, c.Ours
	for _, w := range injected {
		if strings.HasPrefix(b.Content, w.prefix) && !strings.HasPrefix(a.Content, w.prefix) {
			return w.name + ": a user row for us, not a prompt for the oracle"
		}
		if strings.HasPrefix(a.Content, w.prefix) && !strings.HasPrefix(b.Content, w.prefix) {
			return w.name + ": a prompt for the oracle, not a user row for us"
		}
	}
	switch {
	case a == Row{}:
		return "extra rows on our side (" + b.Role + ")"
	case b == Row{}:
		return "rows only the oracle has (" + a.Role + ")"
	case a.Role == "tool" && a.Content == "" && b.Role != "tool":
		return "empty tool output: a FAD row, none of ours"
	case a.Role != b.Role:
		return "role: FAD " + a.Role + ", ours " + b.Role
	case a.Content == b.Content && a.CreatedAt-b.CreatedAt <= 1000 && b.CreatedAt-a.CreatedAt <= 1000:
		return "timestamp within 1s (" + a.Role + ")"
	case a.Content == b.Content:
		return "timestamp (" + a.Role + ")"
	case strings.HasPrefix(a.Content, `{"output":`) && !strings.HasPrefix(b.Content, `{"output":`):
		return "FAD keeps the exec output JSON envelope (" + a.Role + ")"
	case strings.HasPrefix(a.Content, b.Content) || strings.HasPrefix(b.Content, a.Content):
		return "content: one side is a prefix of the other (" + a.Role + ")"
	default:
		return "content (" + a.Role + ")"
	}
}

// injected are harness-written texts that arrive as user records; the
// parsers and oracles disagree on whether they are prompts.
var injected = []struct{ prefix, name string }{
	{"<task-notification>", "Claude background task notification"},
	{"<command-name>", "Claude slash command wrapper"},
	{"<local-command-stdout>", "Claude local command output"},
	{"<environment_context>", "Codex environment context"},
	{"<user_instructions>", "Codex user instructions"},
	{"# AGENTS.md instructions", "Codex AGENTS.md instructions"},
	{"<recommended_plugins>", "Codex recommended plugins"},
	{"# Context from my IDE setup", "Codex IDE context"},
	{"<turn_aborted>", "Codex turn aborted marker"},
	{"<subagent_notification>", "Codex subagent notification"},
	{"[Request interrupted by user", "Claude interruption marker"},
	{"Message Type: NEW_TASK", "Codex multi-agent task envelope"},
}

// Excerpt shows the first differing rows around where their text parts.
func Excerpt(c Comparison) string {
	if c.Row < 0 {
		return strings.Join(c.Problems, "; ")
	}
	a, b := c.FAD.Content, c.Ours.Content
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	cut := func(s string) string {
		from, to := max(0, p-80), min(len(s), p+160)
		return fmt.Sprintf("%q", s[from:to])
	}
	return fmt.Sprintf("row %d of FAD %d / ours %d; roles %s/%s; ts %d/%d; lens %d/%d; differ at byte %d\n  FAD:  %s\n  ours: %s",
		c.Row, c.NFAD, c.NOur, c.FAD.Role, c.Ours.Role, c.FAD.CreatedAt, c.Ours.CreatedAt, len(a), len(b), p, cut(a), cut(b))
}
