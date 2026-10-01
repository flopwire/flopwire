package main

// `flopwire bench corpus` writes the synthetic corpus of internal/synthcorpus;
// `flopwire bench ab` runs the acceptance parts against two binaries,
// in ABBA blocks (A,B,B,A,A,B) on one machine and one corpus, and compares their
// medians. The nightly workflow (.github/workflows/perf-nightly.yml) runs
// both on a hosted runner; see docs/perf/README.md.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/synthcorpus"
)

// parseSize reads a byte count: a number with an optional KB/MB/GB
// (decimal) or KiB/MiB/GiB suffix.
func parseSize(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1}}
	t := strings.TrimSpace(s)
	mult := 1.0
	for _, u := range units {
		if rest, ok := strings.CutSuffix(t, u.suffix); ok {
			t, mult = strings.TrimSpace(rest), u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return int64(v * mult), nil
}

func benchCorpus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("bench corpus", flag.ContinueOnError)
	out := fs.String("out", "", "directory to write the corpus into (a fake home; must be empty)")
	seed := fs.Uint64("seed", 1, "generator seed")
	size := fs.String("size", "1.5GB", "target transcript bytes")
	bigMin := fs.String("big-min", "21MB", "size above which a file counts as big")
	verify := fs.Bool("verify", false, "parse every file with the real parsers and check the planted needles (after generating, or of an existing --out)")
	printVersion := fs.Bool("version", false, "print the generator version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *printVersion {
		fmt.Fprintln(stdout, synthcorpus.Version)
		return nil
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	var m *synthcorpus.Manifest
	if data, err := os.ReadFile(synthcorpus.ManifestPath(*out)); err == nil {
		// An existing corpus (a CI cache hit): verify it, never regenerate.
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		if m.Version != synthcorpus.Version {
			return fmt.Errorf("%s holds generator version %d, this binary writes %d", *out, m.Version, synthcorpus.Version)
		}
		fmt.Fprintf(stdout, "bench corpus: %s already holds a corpus (seed %d)\n", *out, m.Seed)
	} else {
		bytes, err := parseSize(*size)
		if err != nil {
			return err
		}
		big, err := parseSize(*bigMin)
		if err != nil {
			return err
		}
		t0 := time.Now()
		if m, err = synthcorpus.Generate(*out, synthcorpus.Config{Seed: *seed, Bytes: bytes, BigMin: big}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "bench corpus: generated in %s\n", time.Since(t0).Round(time.Second))
	}
	fmt.Fprintf(stdout, "bench corpus: %d files, %.2fGB (claude %d + %d subagents %.2fGB, codex %d %.2fGB, devin %d sessions %.1fMB), %d files over %dMB holding %.2fGB, largest %.1fMB\n",
		m.Files, float64(m.Bytes)/1e9, m.ClaudeFiles, m.Subagents, float64(m.ClaudeBytes)/1e9, m.CodexFiles, float64(m.CodexBytes)/1e9,
		m.DevinSess, float64(m.DevinBytes)/1e6, m.BigFiles, m.BigMin>>20, float64(m.BigBytes)/1e9, float64(m.LargestFile)/1e6)
	fmt.Fprintf(stdout, "bench corpus: %d lines, p50 %dB, p99 %dB, max %dB, %.0f%% of bytes in lines over 100KB, %d tool-results files\n",
		m.Lines, m.LineP50, m.LineP99, m.LineMax, 100*m.HugeShare, m.Companions)
	if !*verify {
		return nil
	}
	t0 := time.Now()
	rep, err := synthcorpus.Verify(ctx, *out, m.NeedleList())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "bench corpus: parsed %d sources, %d messages in %s\n", rep.Sources, rep.Messages, time.Since(t0).Round(time.Second))
	return rep.Check(m)
}

// --- A/B ---

type abRun struct {
	Side   string     `json:"side"`
	N      int        `json:"n"`
	Record *accRecord `json:"record"`
	Rows   int64      `json:"index_rows"`
	IndexM float64    `json:"index_mb"`
	WallS  float64    `json:"wall_s"`
}

type abMetric struct {
	Name    string    `json:"name"`
	Unit    string    `json:"unit"`
	A       float64   `json:"a"`
	B       float64   `json:"b"`
	Change  float64   `json:"change"` // (b-a)/a; +Inf encoded as a large number
	ASpread float64   `json:"a_spread"`
	BSpread float64   `json:"b_spread"`
	AValues []float64 `json:"a_values"`
	BValues []float64 `json:"b_values"`
	Limit   float64   `json:"limit"`
	// MedianRule: B's median grew past the threshold and the minimum
	// change. Regress additionally needs every B run above every A run.
	MedianRule bool `json:"median_rule"`
	Regress    bool `json:"regressed"`
	OnlyInA    bool `json:"only_in_a,omitempty"`
	OnlyInB    bool `json:"only_in_b,omitempty"`
}

type abCheck struct {
	Name    string `json:"name"`
	A       string `json:"a"`
	B       string `json:"b"`
	ADetail string `json:"a_detail"`
	BDetail string `json:"b_detail"`
	Regress bool   `json:"regressed"` // fails in B, passes in A
	Changed bool   `json:"changed"`   // hit counts differ
}

// Verdicts of `bench ab`. BASELINE_FAILED means binary A could not run
// under B's harness (an old baseline lacking a flag the harness now
// passes): no comparison was made, and it is not a regression of B.
const (
	verdictClean          = "CLEAN"
	verdictRegressed      = "REGRESSED"
	verdictBaselineFailed = "BASELINE_FAILED"
)

type abSummary struct {
	Verdict     string        `json:"verdict"`         // CLEAN, REGRESSED or BASELINE_FAILED
	Error       string        `json:"error,omitempty"` // why A failed, with BASELINE_FAILED
	Threshold   float64       `json:"threshold"`
	Runs        int           `json:"runs"`
	A           buildRecord   `json:"a"`
	B           buildRecord   `json:"b"`
	Machine     machineRecord `json:"machine"`
	Corpus      corpusRecord  `json:"corpus"`
	Metrics     []abMetric    `json:"metrics"`
	Checks      []abCheck     `json:"checks"`
	Regressions int           `json:"regressions"`
	ARows       []int64       `json:"a_index_rows"`
	BRows       []int64       `json:"b_index_rows"`
	AIndexMB    float64       `json:"a_index_mb"`
	BIndexMB    float64       `json:"b_index_mb"`
	WallS       float64       `json:"wall_s"`
}

func benchAB(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("bench ab", flag.ContinueOnError)
	binA := fs.String("a", "", "baseline flopwire binary")
	binB := fs.String("b", "", "candidate flopwire binary")
	labelA := fs.String("a-commit", "", "commit of the baseline, for the records")
	labelB := fs.String("b-commit", "", "commit of the candidate, for the records")
	scratch := fs.String("scratch", "", "scratch directory (required)")
	out := fs.String("out", "", "directory for the per-run records and the comparison (required)")
	runs := fs.Int("runs", 3, "runs per binary, in ABBA blocks: A,B,B,A,A,B for 3")
	warm := fs.Bool("warm", true, "read the whole corpus once before the first run, so neither binary meets a cold file cache")
	queries := fs.String("queries", "", "query set (default <home>/queries.yaml)")
	parts := fs.String("only", "index,fresh,queries", "parts to run each time")
	idleAfter := fs.Duration("idle-after", 60*time.Second, "how long the agent idles before its memory is read")
	threshold := fs.Float64("threshold", regressionDefault, "relative growth of a median that counts as a regression")
	strict := fs.Bool("strict", false, "exit nonzero when the verdict is REGRESSED")
	hf := addHarnessFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *binA == "" || *binB == "" || *scratch == "" || *out == "" || *runs < 1 {
		return errors.New("usage: flopwire bench ab --a BIN --b BIN --home DIR --scratch DIR --out DIR [--runs 3]")
	}
	home, claudeDir, codexHome, devinDB := hf.resolve()
	if *queries == "" {
		*queries = synthcorpus.QueriesPath(home)
	}
	for _, d := range []string{*scratch, *out} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	sides := []struct {
		name, bin string
		build     buildRecord
	}{
		{"A", *binA, buildRecord{Version: "baseline", Commit: *labelA}},
		{"B", *binB, buildRecord{Version: "candidate", Commit: *labelB}},
	}
	machine := machineInfo()
	t0 := time.Now()
	var all []abRun
	if *warm {
		wt := time.Now()
		// The harness roots, not home: home is $HOME when --home is unset.
		n, err := warmTree(claudeDir, codexHome, devinDB)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "bench ab: read %.2fGB of the corpus into the file cache in %s\n", float64(n)/1e9, time.Since(wt).Round(time.Second))
	}
	count := map[string]int{}
	for k, si := range abOrder(*runs) {
		sd := sides[si]
		count[sd.name]++
		i := count[sd.name]
		{
			dir := filepath.Join(*scratch, fmt.Sprintf("%s-%d", sd.name, i))
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			b := &bench{exe: sd.bin, scratch: dir, claude: claudeDir, codex: codexHome, devin: devinDB, home: home}
			res := &accResults{}
			rt := time.Now()
			fmt.Fprintf(os.Stderr, "bench ab: run %d/%d, %s %d (%s)\n", k+1, 2**runs, sd.name, i, sd.bin)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			for _, part := range strings.Split(*parts, ",") {
				if err := b.runPart(ctx, res, part, *queries, "", *idleAfter); err != nil {
					err = fmt.Errorf("%s run %d: %w", sd.name, i, err)
					if sd.name != "A" || ctx.Err() != nil {
						return err
					}
					// The baseline cannot run under this harness. Report
					// that, distinctly from a regression of B.
					_ = os.RemoveAll(dir)
					return writeAB(*out, stdout, baselineFailed(sides[0].build, sides[1].build, machine, err), *strict)
				}
			}
			run := abRun{Side: sd.name, N: i, Record: newAccRecord(res, nil, sd.build, machine, time.Now()), WallS: time.Since(rt).Seconds()}
			if res.Index != nil {
				run.Rows, run.IndexM = res.Index.Rows, res.Index.IndexMB
			}
			if err := writeAccRecord(filepath.Join(*out, fmt.Sprintf("%s-%d.json", sd.name, i)), run.Record); err != nil {
				return err
			}
			all = append(all, run)
			fmt.Fprintf(os.Stderr, "bench ab: run %d %s took %s\n", i, sd.name, time.Since(rt).Round(time.Second))
			// Indexes are large; keep the disk of a hosted runner free.
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	sum := summarizeAB(all, *threshold)
	sum.WallS = time.Since(t0).Seconds()
	medA, medB := medianRecord(runsOf(all, "A")), medianRecord(runsOf(all, "B"))
	for name, rec := range map[string]*accRecord{"A.json": medA, "B.json": medB} {
		if err := writeAccRecord(filepath.Join(*out, name), rec); err != nil {
			return err
		}
	}
	return writeAB(*out, stdout, sum, *strict)
}

// writeAB writes ab.json and ab.md and prints the markdown.
func writeAB(out string, stdout io.Writer, sum *abSummary, strict bool) error {
	data, err := json.MarshalIndent(sum, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "ab.json"), append(data, '\n'), 0o644); err != nil {
		return err
	}
	md := sum.markdown()
	if err := os.WriteFile(filepath.Join(out, "ab.md"), []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Fprint(stdout, md)
	if strict && sum.Verdict != verdictClean {
		return fmt.Errorf("strict: verdict %s", sum.Verdict)
	}
	return nil
}

func baselineFailed(a, b buildRecord, machine machineRecord, err error) *abSummary {
	return &abSummary{Verdict: verdictBaselineFailed, Error: err.Error(), A: a, B: b, Machine: machine}
}

// abOrder is the run order of k runs per binary, as indexes into
// {A, B}: ABBA blocks (A,B,B,A,A,B for k=3), so neither binary always runs
// first after the other and drift over the job (cache warmth, thermal or
// neighbour load) does not favour one side.
func abOrder(k int) []int {
	out := make([]int, 0, 2*k)
	for i := range k {
		if i%2 == 0 {
			out = append(out, 0, 1)
		} else {
			out = append(out, 1, 0)
		}
	}
	return out
}

// warmTree reads every regular file under the roots once, so the first
// measured run does not pay for a cold page cache that later runs skip. A
// missing root is skipped (a corpus need not hold every harness); symlinks
// and other non-regular files are not followed. It returns the bytes read.
func warmTree(roots ...string) (int64, error) {
	var n int64
	buf := make([]byte, 1<<20)
	for _, root := range roots {
		if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			c, err := io.CopyBuffer(io.Discard, f, buf)
			n += c
			return err
		})
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func runsOf(all []abRun, side string) []*accRecord {
	var out []*accRecord
	for _, r := range all {
		if r.Side == side {
			out = append(out, r.Record)
		}
	}
	return out
}

// medianRecord folds runs into one record: each metric is the median of
// its values, and a check fails if it failed in any run.
func medianRecord(recs []*accRecord) *accRecord {
	if len(recs) == 0 {
		return nil
	}
	out := *recs[len(recs)-1]
	out.Metrics, out.Checks = nil, nil
	vals := map[string][]float64{}
	var order []string
	byName := map[string]accMetric{}
	checks := map[string]accCheck{}
	var corder []string
	for _, r := range recs {
		for _, m := range r.Metrics {
			if _, ok := vals[m.Name]; !ok {
				order = append(order, m.Name)
				byName[m.Name] = m
			}
			vals[m.Name] = append(vals[m.Name], m.Value)
		}
		for _, c := range r.Checks {
			prev, ok := checks[c.Name]
			if !ok {
				corder = append(corder, c.Name)
			}
			if !ok || (prev.Result == "PASS" && c.Result != "PASS") {
				checks[c.Name] = c
			}
		}
	}
	for _, n := range order {
		m := byName[n]
		out.Metrics = append(out.Metrics, newMetric(n, median(vals[n]), m.Unit, m.Limit))
	}
	for _, n := range corder {
		out.Checks = append(out.Checks, checks[n])
	}
	return &out
}

// spread is (max-min)/median of a metric's runs: the noise of one binary.
func spread(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	m := median(v)
	if m == 0 {
		return 0
	}
	return (slices.Max(v) - slices.Min(v)) / m
}

func checkHits(detail string) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(detail, "%d hits", &n); err != nil {
		return 0, false
	}
	return n, true
}

// summarizeAB compares the medians of B against A with the `bench
// compare` rule (grew by more than threshold and by more than the
// metric's minimum change) and, against runner noise, requires every B
// run to be above every A run. Checks compare directly: a check that fails
// in B but passes in A is a regression; differing hit counts are flagged
// as changed.
func summarizeAB(all []abRun, threshold float64) *abSummary {
	recA, recB := runsOf(all, "A"), runsOf(all, "B")
	medA, medB := medianRecord(recA), medianRecord(recB)
	s := &abSummary{Threshold: threshold, Runs: len(recA), A: medA.Flopwire, B: medB.Flopwire, Machine: medB.Machine, Corpus: medB.Corpus}
	values := func(recs []*accRecord, name string) []float64 {
		var v []float64
		for _, r := range recs {
			for _, m := range r.Metrics {
				if m.Name == name {
					v = append(v, m.Value)
				}
			}
		}
		return v
	}
	c := compareRecords(medA, medB, threshold)
	for _, d := range c.Metrics {
		m := abMetric{Name: d.Name, Unit: d.Unit, A: d.Old, B: d.New, Limit: d.Limit, MedianRule: d.Regress,
			AValues: values(recA, d.Name), BValues: values(recB, d.Name), OnlyInA: !d.HasNew, OnlyInB: !d.HasOld}
		// Hosted runners are noisy: a median shift alone is not enough
		// when the runs overlap.
		m.Regress = d.Regress && len(m.AValues) > 0 && len(m.BValues) > 0 && slices.Min(m.BValues) > slices.Max(m.AValues)
		m.ASpread, m.BSpread = spread(m.AValues), spread(m.BValues)
		m.Change = d.Change
		if math.IsInf(m.Change, 0) || math.IsNaN(m.Change) {
			m.Change = 1e9
		}
		s.Metrics = append(s.Metrics, m)
		if m.Regress {
			s.Regressions++
		}
	}
	checksA := map[string]accCheck{}
	for _, ch := range medA.Checks {
		checksA[ch.Name] = ch
	}
	for _, cb := range medB.Checks {
		ca, ok := checksA[cb.Name]
		x := abCheck{Name: cb.Name, B: cb.Result, BDetail: cb.Detail, A: "-"}
		if ok {
			x.A, x.ADetail = ca.Result, ca.Detail
			ha, oka := checkHits(ca.Detail)
			hb, okb := checkHits(cb.Detail)
			x.Changed = oka && okb && ha != hb
		}
		x.Regress = cb.Result != "PASS" && (!ok || ca.Result == "PASS")
		if x.Regress {
			s.Regressions++
		}
		s.Checks = append(s.Checks, x)
	}
	for _, r := range all {
		if r.Side == "A" {
			s.ARows = append(s.ARows, r.Rows)
			s.AIndexMB = r.IndexM
		} else {
			s.BRows = append(s.BRows, r.Rows)
			s.BIndexMB = r.IndexM
		}
	}
	s.Verdict = verdictClean
	if s.Regressions > 0 {
		s.Verdict = verdictRegressed
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "?"
	}
	return sha
}

// markdown renders the summary for a GitHub step summary or issue.
func (s *abSummary) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Perf A/B: %s\n\n", s.Verdict)
	if s.Verdict == verdictBaselineFailed {
		fmt.Fprintf(&b, "The baseline (A) `%s` failed under the harness of the candidate (B) `%s`, so nothing was compared. This is not a regression of B. "+
			"Most often the baseline lacks a command or flag the harness now passes; move the pinned baseline (docs/perf/README.md#nightly-a-b) to a commit that has it.\n\n",
			short(s.A.Commit), short(s.B.Commit))
		fence := "```"
		for strings.Contains(s.Error, fence) {
			fence += "`"
		}
		fmt.Fprintf(&b, "%s\n%s\n%s\n", fence, tail(s.Error, 3000), fence)
		return b.String()
	}
	fmt.Fprintf(&b, "Baseline (A) `%s`, candidate (B) `%s`. %d runs each, in ABBA blocks (A,B,B,A,...) after warming the file cache, on one runner (%s, %d cores, %.0fGB RAM). Corpus: %d files, %.2fGB. Took %s.\n\n",
		short(s.A.Commit), short(s.B.Commit), s.Runs, s.Machine.CPUModel, s.Machine.Cores, float64(s.Machine.RAMBytes)/(1<<30),
		s.Corpus.Files, float64(s.Corpus.Bytes)/1e9, (time.Duration(s.WallS) * time.Second).String())
	fmt.Fprintf(&b, "A metric regresses when B's median grew by more than %.0f%% and by more than its minimum change (docs/perf/README.md), and every B run is above every A run. Spread is (max-min)/median of one binary's runs: the noise.\n\n", 100*s.Threshold)
	b.WriteString("| metric | A median | B median | change | A spread | B spread | limit | flag |\n|---|---|---|---|---|---|---|---|\n")
	num := func(v float64, unit string) string { return strconv.FormatFloat(v, 'f', 1, 64) + unit }
	for _, m := range s.Metrics {
		a, bv, change, flag := num(m.A, m.Unit), num(m.B, m.Unit), fmt.Sprintf("%+.0f%%", 100*m.Change), ""
		switch {
		case m.OnlyInA:
			bv, change, flag = "-", "-", "gone"
		case m.OnlyInB:
			a, change, flag = "-", "-", "new"
		case m.Change >= 1e9:
			change = "+inf"
		}
		switch {
		case m.Regress:
			flag = "**REGRESSED**"
		case m.MedianRule:
			flag = "noise (runs overlap)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %.0f%% | %.0f%% | < %s | %s |\n", m.Name, a, bv, change, 100*m.ASpread, 100*m.BSpread, num(m.Limit, m.Unit), flag)
	}
	var notes []string
	for _, c := range s.Checks {
		switch {
		case c.Regress:
			notes = append(notes, fmt.Sprintf("- **REGRESSED** check `%s`: A %s (%s), B %s (%s)", c.Name, c.A, c.ADetail, c.B, c.BDetail))
		case c.B != "PASS":
			notes = append(notes, fmt.Sprintf("- check `%s` fails in both: %s", c.Name, c.BDetail))
		case c.Changed:
			notes = append(notes, fmt.Sprintf("- check `%s` hit count changed: A %s, B %s", c.Name, c.ADetail, c.BDetail))
		}
	}
	pass := 0
	for _, c := range s.Checks {
		if c.B == "PASS" {
			pass++
		}
	}
	fmt.Fprintf(&b, "\nChecks: %d/%d pass in B.\n", pass, len(s.Checks))
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	fmt.Fprintf(&b, "\nIndex rows A %v, B %v; index size A %.0fMB, B %.0fMB.\n", s.ARows, s.BRows, s.AIndexMB, s.BIndexMB)
	return b.String()
}
