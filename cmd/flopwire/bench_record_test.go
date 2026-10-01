package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var testMachine = machineRecord{OS: "darwin", Arch: "arm64", CPUModel: "Apple M3 Max", Cores: 16, RAMBytes: 64 << 30}

func sampleResults() *accResults {
	return &accResults{
		Index: &indexResult{WallS: 450, PeakRSSMB: 410, IdleRSSMB: 80, SweepCPUMs: []float64{120, 340, 90}, CorpusFiles: 1200, CorpusBytes: 9e9},
		Fresh: &freshResult{P95: 1500},
		Queries: &queriesResult{Results: []queryResult{
			{Name: "error", Ms: []float64{400, 294, 290, 300}, WarmMs: 294, Hits: 20, OK: true, Reads: 5, ReadsOK: 5},
			{Name: "backoff", Ms: []float64{60, 40, 41, 39}, WarmMs: 40, Hits: 3, OK: false, Problems: []string{"missing session abc"}},
		}},
	}
}

func TestAccRecordJSONShape(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.FixedZone("x", 3600))
	rec := newAccRecord(sampleResults(), []oracleReport{{Agent: "claude", Files: 10, Convs: 8, Matched: 7}},
		buildRecord{Version: "v0.4.0", Commit: "abc123", Dirty: true}, testMachine, at)
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	keys := func(m any) []string {
		var ks []string
		for k := range m.(map[string]any) {
			ks = append(ks, k)
		}
		slices.Sort(ks)
		return ks
	}
	for path, want := range map[string][]string{
		"":         {"checks", "corpus", "flopwire", "machine", "metrics", "recorded_at"},
		"flopwire": {"commit", "dirty", "version"},
		"machine":  {"arch", "cores", "cpu_model", "os", "ram_bytes"},
		"corpus":   {"bytes", "files"},
	} {
		obj := any(got)
		if path != "" {
			obj = got[path]
		}
		if ks := keys(obj); !slices.Equal(ks, want) {
			t.Errorf("%q keys = %v, want %v", path, ks, want)
		}
	}
	if got["recorded_at"] != "2026-09-30T11:00:00Z" {
		t.Errorf("recorded_at = %v, want UTC", got["recorded_at"])
	}
	for _, m := range got["metrics"].([]any) {
		if ks := keys(m); !slices.Equal(ks, []string{"limit", "name", "result", "unit", "value"}) {
			t.Errorf("metric keys = %v", ks)
		}
	}
	for _, c := range got["checks"].([]any) {
		if ks := keys(c); !slices.Equal(ks, []string{"detail", "name", "result"}) {
			t.Errorf("check keys = %v", ks)
		}
	}

	want := []accMetric{
		{"index.wall", 450, "s", 300, "FAIL"},
		{"index.peak_rss", 410, "MB", 600, "PASS"},
		{"idle.rss", 80, "MB", 120, "PASS"},
		{"sweep.cpu_max", 340, "ms", 1000, "PASS"},
		{"fresh.p95", 1500, "ms", 2000, "PASS"},
		{"query.error.warm", 294, "ms", 200, "FAIL"},
		{"query.backoff.warm", 40, "ms", 200, "PASS"},
	}
	if !slices.Equal(rec.Metrics, want) {
		t.Errorf("metrics =\n%+v\nwant\n%+v", rec.Metrics, want)
	}
	if rec.Corpus != (corpusRecord{Files: 1200, Bytes: 9e9}) {
		t.Errorf("corpus = %+v", rec.Corpus)
	}
	results := map[string]string{}
	for _, c := range rec.Checks {
		results[c.Name] = c.Result
	}
	if want := map[string]string{"oracle.claude": "PASS", "query.error": "PASS", "query.backoff": "FAIL"}; !mapsEqual(results, want) {
		t.Errorf("checks = %v, want %v", results, want)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A run of only some parts records only their metrics, and empty lists
// stay arrays in the JSON.
func TestAccRecordPartialRun(t *testing.T) {
	rec := newAccRecord(&accResults{Fresh: &freshResult{P95: 2500}}, nil, buildRecord{}, testMachine, time.Now())
	if len(rec.Metrics) != 1 || rec.Metrics[0].Name != "fresh.p95" || rec.Metrics[0].Result != "FAIL" {
		t.Fatalf("metrics = %+v", rec.Metrics)
	}
	data, _ := json.Marshal(newAccRecord(&accResults{}, nil, buildRecord{}, testMachine, time.Now()))
	if !bytes.Contains(data, []byte(`"metrics":[]`)) || !bytes.Contains(data, []byte(`"checks":[]`)) {
		t.Errorf("empty record = %s", data)
	}
}

func mkRec(at time.Time, m machineRecord, corpusBytes int64, metrics ...accMetric) *accRecord {
	return &accRecord{RecordedAt: at, Machine: m, Corpus: corpusRecord{Files: 1, Bytes: corpusBytes}, Metrics: metrics, Checks: []accCheck{}}
}

func TestCompareRecords(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := mkRec(t0, testMachine, 1e9,
		newMetric("index.wall", 100, "s", 300),
		newMetric("idle.rss", 100, "MB", 120),
		newMetric("fresh.p95", 1000, "ms", 2000),
		newMetric("sweep.cpu_max", 0, "ms", 1000),
		newMetric("query.gone.warm", 50, "ms", 200))
	cur := mkRec(t0.Add(time.Hour), testMachine, 1.1e9,
		newMetric("index.wall", 121, "s", 300),   // +21%: regressed
		newMetric("idle.rss", 119, "MB", 120),    // +19%: within threshold
		newMetric("fresh.p95", 2100, "ms", 2000), // regressed and over the limit
		newMetric("sweep.cpu_max", 150, "ms", 1000),
		newMetric("query.new.warm", 10, "ms", 200))
	cur.Checks = []accCheck{{Name: "query.x", Detail: "missing", Result: "FAIL"}, {Name: "query.y", Result: "PASS"}}
	c := compareRecords(old, cur, 0.2)

	by := map[string]metricDiff{}
	for _, d := range c.Metrics {
		by[d.Name] = d
	}
	for name, regress := range map[string]bool{"index.wall": true, "idle.rss": false, "fresh.p95": true, "sweep.cpu_max": true, "query.new.warm": false, "query.gone.warm": false} {
		if by[name].Regress != regress {
			t.Errorf("%s: regress = %v, want %v (%+v)", name, by[name].Regress, regress, by[name])
		}
	}
	if d := by["index.wall"]; math.Abs(d.Change-0.21) > 1e-9 || d.Old != 100 || d.New != 121 {
		t.Errorf("index.wall diff = %+v", d)
	}
	if !math.IsInf(by["sweep.cpu_max"].Change, 1) {
		t.Errorf("0 -> 150 change = %v, want +Inf", by["sweep.cpu_max"].Change)
	}
	if d := by["query.new.warm"]; d.HasOld || !d.HasNew {
		t.Errorf("new metric = %+v", d)
	}
	if d := by["query.gone.warm"]; !d.HasOld || d.HasNew || d.Result != "" {
		t.Errorf("gone metric = %+v", d)
	}
	if c.Regressions != 3 {
		t.Errorf("regressions = %d, want 3", c.Regressions)
	}
	if c.Failures != 2 { // fresh.p95 over its limit and the failing check
		t.Errorf("failures = %d, want 2", c.Failures)
	}
	if len(c.MachineDiffs) != 0 || c.CorpusNote != "" {
		t.Errorf("same machine, corpus +10%%: diffs %v, note %q", c.MachineDiffs, c.CorpusNote)
	}

	var out bytes.Buffer
	c.print(&out, "old.json", "new.json", 0.2)
	s := out.String()
	for _, want := range []string{"index.wall", "+21%", "REGRESSED", "+inf", "new", "gone", "FAIL check query.x: missing", "3 regressed by more than 20%, 2 failing"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "WARNING") {
		t.Errorf("unexpected warning:\n%s", s)
	}
}

func TestCompareRecordsWarnsOnMachineAndCorpus(t *testing.T) {
	other := testMachine
	other.CPUModel, other.RAMBytes = "Intel Xeon", 32<<30
	c := compareRecords(mkRec(time.Now(), testMachine, 1e9), mkRec(time.Now(), other, 2e9), 0.2)
	if len(c.MachineDiffs) != 2 || !strings.HasPrefix(c.MachineDiffs[0], "cpu_model: Apple M3 Max -> Intel Xeon") || !strings.HasPrefix(c.MachineDiffs[1], "ram: ") {
		t.Errorf("machine diffs = %v", c.MachineDiffs)
	}
	if !strings.Contains(c.CorpusNote, "+100%") {
		t.Errorf("corpus note = %q", c.CorpusNote)
	}
	var out bytes.Buffer
	c.print(&out, "a", "b", 0.2)
	if !strings.Contains(out.String(), "WARNING: the machines differ") || !strings.Contains(out.String(), "WARNING: corpus size changed") {
		t.Errorf("output:\n%s", out.String())
	}
}

func writeRec(t *testing.T, path string, r *accRecord) {
	t.Helper()
	if err := writeAccRecord(path, r); err != nil {
		t.Fatal(err)
	}
}

func TestBenchCompareExitAndDirectory(t *testing.T) {
	dir := t.TempDir()
	perf := filepath.Join(dir, "perf")
	if err := os.Mkdir(perf, 0o755); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// The newest record by recorded_at, not by file name, is the baseline.
	writeRec(t, filepath.Join(perf, "v0.9.0-2026-09-01.json"), mkRec(t0.Add(48*time.Hour), testMachine, 1e9, newMetric("index.wall", 100, "s", 300)))
	writeRec(t, filepath.Join(perf, "v0.10.0-2026-08-01.json"), mkRec(t0, testMachine, 1e9, newMetric("index.wall", 200, "s", 300)))
	cur := filepath.Join(perf, "new.json") // the new record may already sit in the directory
	writeRec(t, cur, mkRec(t0.Add(72*time.Hour), testMachine, 1e9, newMetric("index.wall", 130, "s", 300)))

	var out bytes.Buffer
	if err := benchCompare([]string{perf, cur}, &out); err != nil {
		t.Fatalf("non-strict compare failed: %v", err)
	}
	if !strings.Contains(out.String(), "v0.9.0-2026-09-01.json") || !strings.Contains(out.String(), "REGRESSED") {
		t.Errorf("output:\n%s", out.String())
	}
	if err := benchCompare([]string{"--strict", perf, cur}, &out); err == nil {
		t.Error("strict compare with a regression succeeded")
	}
	// A looser threshold clears the regression; strict then passes.
	if err := benchCompare([]string{"--strict", "--threshold", "0.5", perf, cur}, &out); err != nil {
		t.Errorf("strict compare at 50%%: %v", err)
	}
	// A hard-limit FAIL alone fails strict mode.
	fail := filepath.Join(dir, "fail.json")
	writeRec(t, fail, mkRec(t0.Add(96*time.Hour), testMachine, 1e9, newMetric("index.wall", 310, "s", 300)))
	if err := benchCompare([]string{"--threshold", "10", fail, fail}, &out); err != nil {
		t.Errorf("non-strict compare with a FAIL: %v", err)
	}
	if err := benchCompare([]string{"--strict", "--threshold", "10", fail, fail}, &out); err == nil {
		t.Error("strict compare with a FAIL succeeded")
	}

	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte(`{"x":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := benchCompare([]string{dir, cur}, &out); err == nil || !strings.Contains(err.Error(), "not an acceptance record") {
		t.Errorf("directory with a non-record: %v", err)
	}
	empty := t.TempDir()
	if err := benchCompare([]string{empty, cur}, &out); err == nil || !strings.Contains(err.Error(), "no acceptance record") {
		t.Errorf("empty directory: %v", err)
	}
}

func TestRecordFileName(t *testing.T) {
	r := &accRecord{Flopwire: buildRecord{Version: "v0.4.0"}, RecordedAt: time.Date(2026, 9, 30, 23, 0, 0, 0, time.FixedZone("x", -5*3600))}
	if got := recordFileName(r); got != "v0.4.0-2026-10-01.json" {
		t.Errorf("name = %q", got)
	}
	r.Flopwire.Version = ""
	if got := recordFileName(r); got != "dev-2026-10-01.json" {
		t.Errorf("name = %q", got)
	}
	r.Flopwire.Version = "v1/x y"
	if got := recordFileName(r); got != "v1_x_y-2026-10-01.json" {
		t.Errorf("name = %q", got)
	}
}

func TestCorpusSize(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("claude/projects/-a/s1.jsonl", 100)
	write("claude/projects/-a/s1/subagents/agent-1.jsonl", 10)
	write("claude/projects/-a/notes.md", 1000)
	write("codex/sessions/2026/09/30/rollout-2026-09-30T00-00-00-00000000-0000-4000-8000-000000000000.jsonl", 200)
	write("codex/archived_sessions/rollout-x.jsonl", 20)
	write("codex/history.jsonl", 1000)
	write("devin/sessions.db", 300)
	write("devin/sessions.db-wal", 30)
	got := corpusSize(filepath.Join(dir, "claude/projects"), filepath.Join(dir, "codex"), filepath.Join(dir, "devin/sessions.db"))
	if want := (corpusRecord{Files: 5, Bytes: 660}); got != want {
		t.Errorf("corpus = %+v, want %+v", got, want)
	}
	if got := corpusSize(filepath.Join(dir, "none"), filepath.Join(dir, "none"), filepath.Join(dir, "none.db")); got != (corpusRecord{}) {
		t.Errorf("missing corpus = %+v", got)
	}
}

func TestMachineInfo(t *testing.T) {
	m := machineInfo()
	if m.OS == "" || m.Arch == "" || m.Cores < 1 {
		t.Errorf("machine = %+v", m)
	}
}

// A query whose command failed before any warm run has no warm latency.
// Recording its zero as a PASS metric would show a -100% "improvement"
// now and a +inf REGRESSED once the query works again.
func TestAccRecordSkipsUnmeasuredWarmLatency(t *testing.T) {
	r := &accResults{Queries: &queriesResult{Results: []queryResult{
		{Name: "broken", Ms: []float64{12}, Problems: []string{"exit status 1"}},
		{Name: "fine", Ms: []float64{30, 20, 21, 22}, WarmMs: 21, OK: true},
	}}}
	rec := newAccRecord(r, nil, buildRecord{}, testMachine, time.Now())
	var names []string
	for _, m := range rec.Metrics {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"query.fine.warm"}) {
		t.Errorf("metrics = %v, want only query.fine.warm", names)
	}
	if len(rec.Checks) != 2 || rec.Checks[0].Result != "FAIL" {
		t.Errorf("checks = %+v", rec.Checks)
	}
}

// The committed record must not carry subprocess stderr: it can hold
// absolute paths with the user name and project names.
func TestAccRecordKeepsStderrOutOfChecks(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	stderr := "localindex: " + filepath.Join(home, ".claude", "projects", "-Users-someone-Code-secret", "s.jsonl") + ": permission denied"
	var qr queryResult
	qr.Name = "q"
	qr.problem("query command: exit status 1", stderr)
	qr.problem("read: exit status 2", "open /var/folders/x/index.db: no such file")
	qr.problem("missing session abc", "")
	if !strings.Contains(strings.Join(qr.Problems, "\n"), stderr) {
		t.Errorf("terminal problems lost the stderr: %v", qr.Problems)
	}
	rec := newAccRecord(&accResults{Queries: &queriesResult{Results: []queryResult{qr}}}, nil, buildRecord{}, testMachine, time.Now())
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{home, "secret", "/var/folders", "permission denied", "no such file"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("record contains %q: %s", leak, data)
		}
	}
	if d := rec.Checks[0].Detail; d != "0 hits, reads 0/0; query command: exit status 1; read: exit status 2; missing session abc" {
		t.Errorf("detail = %q", d)
	}

	// A record built from problems alone (no reasons) still leaks nothing.
	old := queryResult{Name: "old", Problems: []string{"exit status 1: " + stderr}}
	rec = newAccRecord(&accResults{Queries: &queriesResult{Results: []queryResult{old}}}, nil, buildRecord{}, testMachine, time.Now())
	if strings.Contains(rec.Checks[0].Detail, home) {
		t.Errorf("detail from problems only = %q", rec.Checks[0].Detail)
	}
}

// Small absolute changes are noise, whatever their relative size.
func TestCompareRecordsMinimumChange(t *testing.T) {
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	old := mkRec(t0, testMachine, 1e9,
		newMetric("sweep.cpu_max", 0, "ms", 1000),
		newMetric("query.a.warm", 40, "ms", 200),
		newMetric("query.b.warm", 40, "ms", 200),
		newMetric("fresh.p95", 100, "ms", 2000),
		newMetric("idle.rss", 40, "MB", 120),
		newMetric("index.peak_rss", 40, "MB", 600),
		newMetric("index.wall", 20, "s", 300))
	cur := mkRec(t0.Add(time.Hour), testMachine, 1e9,
		newMetric("sweep.cpu_max", 5, "ms", 1000),  // 0 -> 5ms CPU: noise
		newMetric("query.a.warm", 49, "ms", 200),   // +9ms: noise
		newMetric("query.b.warm", 70, "ms", 200),   // +30ms: regressed
		newMetric("fresh.p95", 115, "ms", 2000),    // +15ms: noise
		newMetric("idle.rss", 50, "MB", 120),       // +10MB: noise
		newMetric("index.peak_rss", 60, "MB", 600), // +20MB: regressed
		newMetric("index.wall", 28, "s", 300))      // +8s: noise
	c := compareRecords(old, cur, 0.2)
	want := map[string]bool{"sweep.cpu_max": false, "query.a.warm": false, "query.b.warm": true, "fresh.p95": false, "idle.rss": false, "index.peak_rss": true, "index.wall": false}
	for _, d := range c.Metrics {
		if d.Regress != want[d.Name] {
			t.Errorf("%s: regress = %v, want %v (%+v)", d.Name, d.Regress, want[d.Name], d)
		}
	}
	if c.Regressions != 2 {
		t.Errorf("regressions = %d, want 2", c.Regressions)
	}
}
