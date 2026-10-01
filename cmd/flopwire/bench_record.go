package main

// Acceptance records: `flopwire bench acceptance --json PATH` writes one
// machine-readable record of a run, and `flopwire bench compare OLD NEW`
// diffs two records. Each release commits its record under docs/perf/
// (see docs/perf/README.md); scripts/acceptance.sh compares a new run with
// the most recent committed one.

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// Hard limits of the acceptance table (docs/release-checklist.md §5).
const (
	indexWallLimitS   = 300
	sweepCPULimitMs   = 1000
	freshP95LimitMs   = 2000
	queryWarmLimitMs  = 200
	regressionDefault = 0.20
)

// accRecord is one acceptance run as committed under docs/perf/.
type accRecord struct {
	Flopwire   buildRecord   `json:"flopwire"`
	RecordedAt time.Time     `json:"recorded_at"`
	Machine    machineRecord `json:"machine"`
	Corpus     corpusRecord  `json:"corpus"`
	Metrics    []accMetric   `json:"metrics"`
	Checks     []accCheck    `json:"checks"`
}

type buildRecord struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Dirty   bool   `json:"dirty"`
}

type machineRecord struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUModel string `json:"cpu_model"`
	Cores    int    `json:"cores"`
	RAMBytes int64  `json:"ram_bytes"`
}

type corpusRecord struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// accMetric is one measured number. Every metric is lower-is-better.
type accMetric struct {
	Name   string  `json:"name"`
	Value  float64 `json:"value"`
	Unit   string  `json:"unit"`
	Limit  float64 `json:"limit"` // the value must be below it
	Result string  `json:"result"`
}

// accCheck is a pass/fail check without a number to compare.
type accCheck struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Result string `json:"result"`
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func newMetric(name string, value float64, unit string, limit float64) accMetric {
	return accMetric{Name: name, Value: value, Unit: unit, Limit: limit, Result: passFail(value < limit)}
}

// Set with -ldflags -X by scripts/acceptance.sh. Go's own VCS stamp
// (debug.ReadBuildInfo) is the fallback; it is missing in some builds.
var (
	buildCommit string
	buildDirty  string // "true" when the tree had uncommitted changes
)

// buildRecordOf returns this binary's version and VCS revision.
func buildRecordOf() buildRecord {
	b := buildRecord{Version: version}
	if buildCommit != "" {
		b.Commit, b.Dirty = buildCommit, buildDirty == "true"
		return b
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				b.Commit = s.Value
			case "vcs.modified":
				b.Dirty = s.Value == "true"
			}
		}
	}
	return b
}

// newAccRecord turns the merged part results into a record. Parts that did
// not run contribute no metrics.
func newAccRecord(r *accResults, oracles []oracleReport, build buildRecord, machine machineRecord, at time.Time) *accRecord {
	rec := &accRecord{Flopwire: build, RecordedAt: at.UTC(), Machine: machine, Metrics: []accMetric{}, Checks: []accCheck{}}
	if x := r.Index; x != nil {
		rec.Corpus = corpusRecord{Files: x.CorpusFiles, Bytes: x.CorpusBytes}
		rec.Metrics = append(rec.Metrics,
			newMetric("index.wall", x.WallS, "s", indexWallLimitS),
			newMetric("index.peak_rss", x.PeakRSSMB, "MB", peakRSSTarget),
			newMetric("idle.rss", x.IdleRSSMB, "MB", idleRSSTarget))
		if len(x.SweepCPUMs) > 0 {
			rec.Metrics = append(rec.Metrics, newMetric("sweep.cpu_max", slices.Max(x.SweepCPUMs), "ms", sweepCPULimitMs))
		}
	}
	if x := r.Fresh; x != nil {
		rec.Metrics = append(rec.Metrics, newMetric("fresh.p95", x.P95, "ms", freshP95LimitMs))
	}
	for _, o := range oracles {
		rec.Checks = append(rec.Checks, accCheck{
			Name:   "oracle." + o.Agent,
			Detail: fmt.Sprintf("%d/%d conversations match, %d files, %d parse errors", o.Matched, o.Convs, o.Files, o.ParseErrors),
			Result: passFail(o.ParseErrors == 0),
		})
	}
	if x := r.Queries; x != nil {
		for _, q := range x.Results {
			if len(q.Ms) > 1 { // a command that failed on its first run has no warm time
				rec.Metrics = append(rec.Metrics, newMetric("query."+q.Name+".warm", q.WarmMs, "ms", queryWarmLimitMs))
			}
			detail := fmt.Sprintf("%d hits, reads %d/%d", q.Hits, q.ReadsOK, q.Reads)
			// Reasons, not Problems: Problems carry stderr, which can name
			// home paths and projects, and the record is committed.
			switch {
			case len(q.Reasons) > 0:
				detail += "; " + strings.Join(q.Reasons, "; ")
			case len(q.Problems) > 0:
				detail += fmt.Sprintf("; %d problems (see the terminal output)", len(q.Problems))
			}
			rec.Checks = append(rec.Checks, accCheck{Name: "query." + q.Name, Detail: detail, Result: passFail(q.OK)})
		}
	}
	return rec
}

func writeAccRecord(path string, rec *accRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readAccRecord(path string) (*accRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec accRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if rec.RecordedAt.IsZero() {
		return nil, fmt.Errorf("%s: not an acceptance record (no recorded_at)", path)
	}
	return &rec, nil
}

// recordFileName is the docs/perf/ name of a record: <version>-<date>.json.
func recordFileName(rec *accRecord) string {
	v := rec.Flopwire.Version
	if v == "" {
		v = "dev"
	}
	v = strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' || r == os.PathSeparator {
			return '_'
		}
		return r
	}, v)
	return v + "-" + rec.RecordedAt.UTC().Format("2006-01-02") + ".json"
}

// latestRecord returns the record in dir with the newest recorded_at,
// skipping the file exclude (the new record, when it already sits there).
func latestRecord(dir, exclude string) (string, *accRecord, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return "", nil, err
	}
	excl, _ := filepath.Abs(exclude)
	var bestPath string
	var best *accRecord
	for _, p := range paths {
		if a, _ := filepath.Abs(p); a == excl {
			continue
		}
		rec, err := readAccRecord(p)
		if err != nil {
			return "", nil, err
		}
		if best == nil || rec.RecordedAt.After(best.RecordedAt) {
			bestPath, best = p, rec
		}
	}
	if best == nil {
		return "", nil, fmt.Errorf("no acceptance record in %s", dir)
	}
	return bestPath, best, nil
}

// --- comparison ---

type metricDiff struct {
	Name     string
	Unit     string
	Old, New float64
	HasOld   bool
	HasNew   bool
	Change   float64 // (new-old)/old; +Inf when old is 0 and new is not
	Limit    float64
	Result   string // the new run's PASS/FAIL ("" when the metric is gone)
	Regress  bool
}

type accComparison struct {
	Metrics      []metricDiff
	Checks       []accCheck // failing checks of the new run
	MachineDiffs []string
	CorpusNote   string
	Regressions  int
	Failures     int
}

// compareRecords diffs cur against old. A metric regresses when it grew
// by more than threshold (0.2 = 20%) and by more than minRegression.
// Every metric is lower-is-better.
func compareRecords(old, cur *accRecord, threshold float64) *accComparison {
	c := &accComparison{}
	oldBy := map[string]accMetric{}
	for _, m := range old.Metrics {
		oldBy[m.Name] = m
	}
	seen := map[string]bool{}
	for _, m := range cur.Metrics {
		seen[m.Name] = true
		d := metricDiff{Name: m.Name, Unit: m.Unit, New: m.Value, HasNew: true, Limit: m.Limit, Result: m.Result}
		if o, ok := oldBy[m.Name]; ok {
			d.Old, d.HasOld = o.Value, true
			switch {
			case o.Value > 0:
				d.Change = (m.Value - o.Value) / o.Value
			case m.Value > 0:
				d.Change = math.Inf(1)
			}
			d.Regress = d.Change > threshold && m.Value-o.Value > minRegression(m)
		}
		if d.Regress {
			c.Regressions++
		}
		if m.Result != "PASS" {
			c.Failures++
		}
		c.Metrics = append(c.Metrics, d)
	}
	for _, m := range old.Metrics {
		if !seen[m.Name] {
			c.Metrics = append(c.Metrics, metricDiff{Name: m.Name, Unit: m.Unit, Old: m.Value, HasOld: true, Limit: m.Limit})
		}
	}
	for _, ch := range cur.Checks {
		if ch.Result != "PASS" {
			c.Checks = append(c.Checks, ch)
			c.Failures++
		}
	}
	om, nm := old.Machine, cur.Machine
	for _, f := range []struct{ name, old, new string }{
		{"os", om.OS, nm.OS},
		{"arch", om.Arch, nm.Arch},
		{"cpu_model", om.CPUModel, nm.CPUModel},
		{"cores", strconv.Itoa(om.Cores), strconv.Itoa(nm.Cores)},
		{"ram", fmtGB(om.RAMBytes), fmtGB(nm.RAMBytes)},
	} {
		if f.old != f.new {
			c.MachineDiffs = append(c.MachineDiffs, fmt.Sprintf("%s: %s -> %s", f.name, f.old, f.new))
		}
	}
	if o, n := old.Corpus.Bytes, cur.Corpus.Bytes; o > 0 && n > 0 {
		if ch := float64(n-o) / float64(o); math.Abs(ch) > threshold {
			c.CorpusNote = fmt.Sprintf("corpus size changed %+.0f%% (%s -> %s); index time and RSS scale with it", 100*ch, fmtGB(o), fmtGB(n))
		}
	}
	return c
}

// minRegression is the absolute growth below which a metric never counts
// as regressed, whatever its relative change: run-to-run noise of small
// numbers (0 -> 5ms sweep CPU, 40 -> 49ms query) is not a regression.
func minRegression(m accMetric) float64 {
	switch {
	case m.Name == "sweep.cpu_max":
		return 100 // ms of CPU time
	case m.Unit == "ms":
		return 20
	case m.Unit == "s":
		return 10
	case m.Unit == "MB":
		return 16
	}
	return 0
}

func fmtGB(b int64) string { return fmt.Sprintf("%.1fGB", float64(b)/1e9) }

func (c *accComparison) print(w io.Writer, oldName, newName string, threshold float64) {
	fmt.Fprintf(w, "acceptance compare: %s -> %s (regression threshold +%.0f%%)\n", oldName, newName, 100*threshold)
	if len(c.MachineDiffs) > 0 {
		fmt.Fprintln(w, "WARNING: the machines differ, so the timings are not comparable:")
		for _, d := range c.MachineDiffs {
			fmt.Fprintln(w, "  "+d)
		}
	}
	if c.CorpusNote != "" {
		fmt.Fprintln(w, "WARNING: "+c.CorpusNote)
	}
	rows := [][]string{{"metric", "old", "new", "change", "limit", "result", "flag"}}
	num := func(v float64, unit string) string {
		return strconv.FormatFloat(v, 'f', 1, 64) + unit
	}
	for _, d := range c.Metrics {
		ov, nv, change, flag := "-", "-", "-", ""
		if d.HasOld {
			ov = num(d.Old, d.Unit)
		}
		if d.HasNew {
			nv = num(d.New, d.Unit)
		}
		switch {
		case d.HasOld && d.HasNew && math.IsInf(d.Change, 1):
			change = "+inf"
		case d.HasOld && d.HasNew:
			change = fmt.Sprintf("%+.0f%%", 100*d.Change)
		case d.HasNew:
			flag = "new"
		default:
			flag = "gone"
		}
		if d.Regress {
			flag = "REGRESSED"
		}
		rows = append(rows, []string{d.Name, ov, nv, change, "< " + num(d.Limit, d.Unit), d.Result, flag})
	}
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, s := range r {
			widths[i] = max(widths[i], len(s))
		}
	}
	for _, r := range rows {
		var b strings.Builder
		for i, s := range r {
			if i > 0 {
				b.WriteString("  ")
			}
			fmt.Fprintf(&b, "%-*s", widths[i], s)
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	for _, ch := range c.Checks {
		fmt.Fprintf(w, "FAIL check %s: %s\n", ch.Name, ch.Detail)
	}
	fmt.Fprintf(w, "%d regressed by more than %.0f%%, %d failing (metrics and checks)\n", c.Regressions, 100*threshold, c.Failures)
}

// benchCompare is `flopwire bench compare [--strict] OLD NEW`. OLD may be
// a directory: its record with the newest recorded_at is used. Without
// --strict it always exits 0; the table is the result.
func benchCompare(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("bench compare", flag.ContinueOnError)
	strict := flags.Bool("strict", false, "exit nonzero on any regression past the threshold or any FAIL")
	threshold := flags.Float64("threshold", regressionDefault, "relative growth that counts as a regression")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: flopwire bench compare [--strict] [--threshold 0.2] OLD|DIR NEW")
	}
	oldPath, newPath := flags.Arg(0), flags.Arg(1)
	newRec, err := readAccRecord(newPath)
	if err != nil {
		return err
	}
	var oldRec *accRecord
	if fi, err := os.Stat(oldPath); err == nil && fi.IsDir() {
		oldPath, oldRec, err = latestRecord(oldPath, newPath)
		if err != nil {
			return err
		}
	} else if oldRec, err = readAccRecord(oldPath); err != nil {
		return err
	}
	c := compareRecords(oldRec, newRec, *threshold)
	c.print(stdout, oldPath, newPath, *threshold)
	if *strict && (c.Regressions > 0 || c.Failures > 0) {
		return fmt.Errorf("strict: %d regressions, %d failures", c.Regressions, c.Failures)
	}
	return nil
}

// --- machine and corpus ---

func machineInfo() machineRecord {
	m := machineRecord{OS: runtime.GOOS, Arch: runtime.GOARCH, Cores: runtime.NumCPU()}
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			m.CPUModel = strings.TrimSpace(string(out))
		}
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			m.RAMBytes, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		}
	case "linux":
		m.CPUModel = procField("/proc/cpuinfo", "model name")
		if kb := strings.Fields(procField("/proc/meminfo", "MemTotal")); len(kb) > 0 {
			n, _ := strconv.ParseInt(kb[0], 10, 64)
			m.RAMBytes = n * 1024
		}
	}
	return m
}

// procField returns the value of the first "key: value" line of a /proc file.
func procField(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// corpusSize counts the transcripts the index part reads: every .jsonl
// under the Claude projects root (sessions and subagents), every Codex
// rollout, and the Devin store with its WAL. It reads metadata only.
func corpusSize(claudeRoot, codexHome, devinDB string) corpusRecord {
	var c corpusRecord
	add := func(path string) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			c.Files++
			c.Bytes += fi.Size()
		}
	}
	_ = filepath.WalkDir(claudeRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			add(p)
		}
		return nil
	})
	if srcs, err := codex.Discover(codexHome); err == nil {
		for _, s := range srcs {
			add(s.Path)
		}
	}
	add(devinDB)
	if fi, err := os.Stat(devinDB + "-wal"); err == nil {
		c.Bytes += fi.Size()
	}
	return c
}
