package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/flopwire/flopwire/internal/synthcorpus"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"1.5GB": 1_500_000_000, "200MB": 200_000_000, "8MiB": 8 << 20, "21MB": 21_000_000, "4096": 4096, "1 GiB": 1 << 30} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "GB", "-1MB", "abc"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded", in)
		}
	}
}

// abRecord is one run's record with the given index wall, query latency
// and check result.
func abRecord(wall, query float64, check string, hits int) *accRecord {
	return &accRecord{
		RecordedAt: time.Now(),
		Machine:    testMachine,
		Metrics: []accMetric{
			newMetric("index.wall", wall, "s", indexWallLimitS),
			newMetric("query.x.warm", query, "ms", queryWarmLimitMs),
		},
		Checks: []accCheck{{Name: "query.x", Detail: strconv.Itoa(hits) + " hits, reads 5/5", Result: check}},
	}
}

func abRuns(a, b []*accRecord) []abRun {
	var out []abRun
	for i := range a {
		out = append(out, abRun{Side: "A", N: i + 1, Record: a[i]}, abRun{Side: "B", N: i + 1, Record: b[i]})
	}
	return out
}

func metricOf(s *abSummary, name string) abMetric {
	for _, m := range s.Metrics {
		if m.Name == name {
			return m
		}
	}
	return abMetric{}
}

func TestSummarizeABRegression(t *testing.T) {
	a := []*accRecord{abRecord(100, 50, "PASS", 12), abRecord(104, 52, "PASS", 12), abRecord(98, 51, "PASS", 12)}
	b := []*accRecord{abRecord(140, 51, "PASS", 12), abRecord(138, 53, "PASS", 12), abRecord(150, 50, "PASS", 12)}
	s := summarizeAB(abRuns(a, b), 0.2)
	if s.Verdict != "REGRESSED" || s.Regressions != 1 {
		t.Fatalf("verdict %s, %d regressions; want REGRESSED, 1", s.Verdict, s.Regressions)
	}
	w := metricOf(s, "index.wall")
	if !w.Regress || w.A != 100 || w.B != 140 {
		t.Errorf("index.wall = %+v; want medians 100 -> 140, regressed", w)
	}
	if q := metricOf(s, "query.x.warm"); q.Regress {
		t.Errorf("query.x.warm regressed: %+v", q)
	}
	if w.ASpread < 0.059 || w.ASpread > 0.061 {
		t.Errorf("A spread = %v, want (104-98)/100", w.ASpread)
	}
	if !strings.Contains(s.markdown(), "**REGRESSED**") {
		t.Error("markdown does not flag the regression")
	}
}

// A median shift whose runs overlap is noise, not a regression.
func TestSummarizeABOverlapIsNoise(t *testing.T) {
	a := []*accRecord{abRecord(100, 50, "PASS", 12), abRecord(150, 50, "PASS", 12), abRecord(100, 50, "PASS", 12)}
	b := []*accRecord{abRecord(130, 50, "PASS", 12), abRecord(130, 50, "PASS", 12), abRecord(90, 50, "PASS", 12)}
	s := summarizeAB(abRuns(a, b), 0.2)
	w := metricOf(s, "index.wall")
	if !w.MedianRule || w.Regress || s.Verdict != "CLEAN" {
		t.Fatalf("index.wall = %+v, verdict %s; want the median rule hit, no regression, CLEAN", w, s.Verdict)
	}
	if !strings.Contains(s.markdown(), "noise (runs overlap)") {
		t.Error("markdown does not mark the overlap")
	}
}

// A small absolute change is below the minimum and never regresses.
func TestSummarizeABMinimumChange(t *testing.T) {
	a := []*accRecord{abRecord(10, 40, "PASS", 12), abRecord(10, 40, "PASS", 12)}
	b := []*accRecord{abRecord(15, 55, "PASS", 12), abRecord(15, 55, "PASS", 12)}
	s := summarizeAB(abRuns(a, b), 0.2)
	if s.Verdict != "CLEAN" {
		t.Fatalf("verdict %s for +5s wall and +15ms query; want CLEAN (minimums 10s, 20ms)", s.Verdict)
	}
}

func TestSummarizeABChecks(t *testing.T) {
	// A check failing in B only is a regression, even in one run.
	a := []*accRecord{abRecord(100, 50, "PASS", 12), abRecord(100, 50, "PASS", 12)}
	b := []*accRecord{abRecord(100, 50, "PASS", 12), abRecord(100, 50, "FAIL", 11)}
	s := summarizeAB(abRuns(a, b), 0.2)
	if s.Verdict != "REGRESSED" || len(s.Checks) != 1 || !s.Checks[0].Regress || !s.Checks[0].Changed {
		t.Fatalf("verdict %s, checks %+v; want REGRESSED with the check regressed and changed", s.Verdict, s.Checks)
	}
	// Failing in both is not a regression of B.
	a[1] = abRecord(100, 50, "FAIL", 11)
	s = summarizeAB(abRuns(a, b), 0.2)
	if s.Verdict != "CLEAN" || s.Checks[0].Regress {
		t.Fatalf("verdict %s, checks %+v; want CLEAN when A fails the same check", s.Verdict, s.Checks)
	}
}

func TestMedianRecord(t *testing.T) {
	m := medianRecord([]*accRecord{abRecord(300, 10, "PASS", 12), abRecord(100, 30, "PASS", 12), abRecord(200, 20, "PASS", 12)})
	if m.Metrics[0].Value != 200 || m.Metrics[0].Result != "PASS" || m.Metrics[1].Value != 20 {
		t.Fatalf("metrics %+v; want medians 200 and 20", m.Metrics)
	}
	m = medianRecord([]*accRecord{abRecord(400, 10, "PASS", 12), abRecord(400, 10, "FAIL", 12)})
	if m.Metrics[0].Result != "FAIL" || m.Checks[0].Result != "FAIL" {
		t.Fatalf("got %+v; want the 400s wall to FAIL its 300s limit and the check to fail", m)
	}
}

// TestBenchCorpusVerify generates a small corpus through the CLI, verifies
// it, and reads its query set with the bench's own query type.
func TestBenchCorpusVerify(t *testing.T) {
	out := filepath.Join(t.TempDir(), "home")
	var buf bytes.Buffer
	if err := benchCorpus(context.Background(), []string{"--out", out, "--size", "40MiB", "--big-min", "3MiB", "--verify"}, &buf); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "parsed") {
		t.Fatalf("no verify line:\n%s", buf.String())
	}
	// A second call on the same directory verifies, never regenerates.
	buf.Reset()
	if err := benchCorpus(context.Background(), []string{"--out", out, "--seed", "9", "--verify"}, &buf); err != nil || !strings.Contains(buf.String(), "already holds") {
		t.Fatalf("second call: %v\n%s", err, buf.String())
	}
	data, err := os.ReadFile(synthcorpus.QueriesPath(out))
	if err != nil {
		t.Fatal(err)
	}
	var specs []querySpec
	if err := yaml.Unmarshal(data, &specs); err != nil {
		t.Fatal(err)
	}
	filters := 0
	for _, q := range specs {
		if q.Expect.MaxHits > 0 {
			filters++
		}
	}
	if len(specs) < 10 || filters < 3 {
		t.Fatalf("%d queries, %d with max_hits; want >= 10 and >= 3", len(specs), filters)
	}
}

func TestHarnessFlagsHome(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	h := addHarnessFlags(fs)
	if err := fs.Parse([]string{"--home", "/synth", "--codex-home", "/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	home, cl, cx, dv := h.resolve()
	if home != "/synth" || cl != "/synth/.claude/projects" || cx != "/elsewhere" || dv != "/synth/.local/share/devin/cli/sessions.db" {
		t.Fatalf("resolve = %s %s %s %s", home, cl, cx, dv)
	}
}

// A baseline binary that cannot run under the harness (here: it rejects a
// flag, as an old pinned baseline would) yields BASELINE_FAILED with the
// error, not a failed command and not a regression of B.
func TestBenchABBaselineFailed(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "flopwire-old")
	script := "#!/bin/sh\necho 'flag provided but not defined: -no-sync' >&2\nexit 2\n"
	if err := os.WriteFile(old, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	for _, d := range []string{".claude/projects", ".codex"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "out")
	var buf bytes.Buffer
	err := benchAB(context.Background(), []string{"--a", old, "--a-commit", "aaaa", "--b", old, "--b-commit", "bbbb",
		"--home", home, "--scratch", filepath.Join(dir, "scratch"), "--out", out, "--only", "index", "--idle-after", "1ms"}, &buf)
	if err != nil {
		t.Fatalf("bench ab: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "ab.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s abSummary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Verdict != verdictBaselineFailed || !strings.Contains(s.Error, "-no-sync") {
		t.Fatalf("verdict %s, error %q; want BASELINE_FAILED naming the flag", s.Verdict, s.Error)
	}
	if md := buf.String(); !strings.Contains(md, "not a regression") || !strings.Contains(md, "-no-sync") {
		t.Fatalf("markdown does not explain the failure:\n%s", md)
	}
	err = benchAB(context.Background(), []string{"--a", old, "--b", old, "--home", home, "--scratch", filepath.Join(dir, "scratch"),
		"--out", out, "--only", "index", "--idle-after", "1ms", "--strict"}, &buf)
	if err == nil {
		t.Fatal("--strict passed a BASELINE_FAILED run")
	}
}

// TestABOrderBalanced: runs come in ABBA blocks, so A does not always run
// first (onto a colder cache) and each side leads equally often.
func TestABOrderBalanced(t *testing.T) {
	name := func(o []int) string {
		var b strings.Builder
		for _, i := range o {
			b.WriteByte("AB"[i])
		}
		return b.String()
	}
	for k, want := range map[int]string{1: "AB", 2: "ABBA", 3: "ABBAAB", 4: "ABBAABBA"} {
		if got := name(abOrder(k)); got != want {
			t.Errorf("abOrder(%d) = %s, want %s", k, got, want)
		}
	}
	for k := 1; k <= 8; k++ {
		o := abOrder(k)
		a, aFirst := 0, 0
		for i, s := range o {
			if s == 0 {
				a++
			}
			if i%2 == 0 && s == 0 {
				aFirst++
			}
		}
		if a != k || len(o) != 2*k || aFirst != (k+1)/2 {
			t.Errorf("abOrder(%d) = %v: %d A runs, A leads %d pairs", k, o, a, aFirst)
		}
	}
}

func TestWarmTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, n := range map[string]int{"x": 10, "a/y": 2000, "a/b/z": 3 << 20} {
		if err := os.WriteFile(filepath.Join(dir, p), make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := warmTree(dir); err != nil || n != 10+2000+3<<20 {
		t.Fatalf("warmTree = %d, %v", n, err)
	}
}
