package agent

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/perfguard"
)

// Every store this test binary opens goes through the counting driver;
// connections no test claims pass through unwrapped.
func init() { localindex.UseDriver(perfguard.SQLiteDriver) }

// sweepUnits is the fixture size of TestNoChangeSweepScales at n (k=8
// gives 8n). A unit is one Claude session in a project of four; even units
// add a session directory with a subagent transcript, its meta.json and a
// tool result, odd units a Codex rollout in its own day directory. About
// three files and two directories per unit.
const sweepUnits = 24

// sweepCost is what one no-change periodic sweep cost at one fixture size.
type sweepCost struct {
	files    int
	projects string         // the Claude projects root
	sql      perfguard.Cost // statements and pages on the index
	bySQL    string         // the statements, for failures
	fs       perfguard.FSCost
	opened   []string
	listed   map[string]int64
	rewatch  perfguard.FSCost // a further watchNew alone
	relist   map[string]int64
	work     int64 // files the sweep queued and indexed
	cpu      time.Duration
}

// Release budget (docs/release-checklist.md, a): a sweep over ~20k
// unchanged files takes under 1s of CPU. The guard is the shape that
// keeps it there: the sweep stats each tracked file and lists each
// directory a constant number of times, opens no file, and runs no
// query per file (the index sees a constant number of statements). A
// stat or listing per file pair, a parse of an unchanged file, a query
// per file, or watchNew listing every watched directory again fails it.
func TestNoChangeSweepScales(t *testing.T) {
	t.Parallel() // waits out the racy window three times; counts are scoped to its own paths
	costs := map[int]sweepCost{}
	measure := func(t testing.TB, n int) sweepCost {
		if c, ok := costs[n]; ok {
			return c
		}
		c := measureNoChangeSweep(t, n)
		t.Logf("n=%d: %d files, %s, %s, watchNew again %s, indexed %d, cpu %v", n, c.files, c.sql, c.fs, c.rewatch, c.work, c.cpu)
		costs[n] = c
		return c
	}
	perfguard.AssertScaling(t, perfguard.Constant, sweepUnits, 8, func(t testing.TB, n int) perfguard.Cost {
		return measure(t, n).sql
	})
	perfguard.AssertScaling(t, perfguard.Linear, sweepUnits, 8, func(t testing.TB, n int) perfguard.Cost {
		return perfguard.Cost{Statements: -1, FS: measure(t, n).fs}
	})
	for _, n := range []int{1, sweepUnits, 8 * sweepUnits} {
		c := costs[n]
		if c.fs.Opens != 0 || c.work != 0 {
			t.Errorf("n=%d: a sweep over unchanged files opened %d files and indexed %d: %s", n, c.fs.Opens, c.work, strings.Join(c.opened, ", "))
		}
		// A constant number of statements, whatever the corpus: today only
		// the idle memory trim's transaction (which may also prune FTS queue
		// entries the shards applied since the last write).
		if c.sql.Statements > 12 {
			t.Errorf("n=%d: a sweep over unchanged files ran %d statements on the index, want <= 12:\n%s", n, c.sql.Statements, c.bySQL)
		}
		// Per tracked file: the merge stat, plus the discovery's own (a Codex
		// rollout's FileID, a subagent's meta.json) and the watch set's checks
		// of a hot session's directories: about 3 per file at n, 2.2 at 8n.
		// One more stat per file fails at both.
		if c.fs.Stats > int64(3*c.files)+32 {
			t.Errorf("n=%d: %d stats for %d files", n, c.fs.Stats, c.files)
		}
		// watchNew lists only directories it newly watches: none here.
		// watchDirs reads the projects root to rank projects; that is all.
		if c.rewatch.Lists > 1 || c.rewatch.Opens != 0 {
			t.Errorf("n=%d: watchNew with nothing newly watched cost %s: listed %v", n, c.rewatch, slices.Sorted(maps.Keys(c.relist)))
		}
		if dup := listedTwice(c.listed, c.projects); len(dup) > 0 {
			t.Errorf("n=%d: a sweep listed directories more than once: %s", n, strings.Join(dup, ", "))
		}
	}
}

// listedTwice is the directories of listed that appear more than once,
// beyond the projects root (discovery and watchDirs each read it).
func listedTwice(listed map[string]int64, projects string) []string {
	var dup []string
	for d, n := range listed {
		if n > 1 && d != projects {
			dup = append(dup, fmt.Sprintf("%s ×%d", d, n))
		}
	}
	slices.Sort(dup)
	return dup
}

// measureNoChangeSweep builds a fixture of n units, indexes it, lets one
// periodic sweep settle the gates (the racy window, the first watches),
// then measures the next periodic sweep and one more watchNew.
func measureNoChangeSweep(t testing.TB, n int) sweepCost {
	root := t.TempDir()
	files := writeSweepFixture(t, root, n)
	idx := t.TempDir()
	sc := perfguard.CountSQLite(t, idx)
	fc := perfguard.CountFS(t, root)
	store, err := localindex.Open(filepath.Join(idx, "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := Config{ClaudeProjects: filepath.Join(root, "claude", "projects"), CodexHome: filepath.Join(root, "codex"),
		DevinDB: filepath.Join(root, "devin", "cli", "sessions.db"), OpencodeDB: "-", Workers: 3, Sync: newRecorder(), Home: root,
		UserRuleList: []string{"local " + filepath.Join(root, "work", "p0")},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil))}
	a := New(store, cfg)
	if err := a.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if got := a.stats.Indexed.Load() + a.stats.Companions.Load(); got == 0 {
		t.Fatal("the fixture indexed nothing")
	}

	rctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	a.startWorkers(rctx, &wg)
	defer func() {
		cancel()
		a.kick()
		wg.Wait()
		a.bgWG.Wait()
	}()
	w := newWatcher(cfg.Logger)
	defer w.close()
	recovered := make(chan struct{}, 1)
	tick := func() {
		a.periodicSweep(rctx, w, recovered)
		a.WaitIdle()
		a.bgWG.Wait()
	}
	waitRacy()
	tick() // settles racy gates and watches the hot directories

	var c sweepCost
	c.files, c.projects = files, cfg.ClaudeProjects
	work := func() int64 {
		s := &a.stats
		return s.Indexed.Load() + s.Appends.Load() + s.Rewrites.Load() + s.Unchanged.Load() + s.Companions.Load() + s.DevinPolls.Load()
	}
	before, cpu := work(), cpuTime()
	sc.Reset()
	c.sql = perfguard.MeasureSQLite(sc, func() { c.fs = fc.Measure(tick) })
	c.bySQL = sc.String()
	c.cpu = cpuTime() - cpu
	c.work = work() - before
	c.opened, c.listed = fc.Opened(), fc.Listed()
	c.rewatch = fc.Measure(func() { a.watchNew(rctx, w, "") })
	c.relist = fc.Listed()
	return c
}

// writeSweepFixture writes n units under root (see sweepUnits), a Devin
// store, and the working directories the sessions name. It returns the
// number of files the agent tracks.
func writeSweepFixture(t testing.TB, root string, n int) int {
	t.Helper()
	files := 0
	write := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files++
	}
	projects := filepath.Join(root, "claude", "projects")
	for i := range n {
		cwd := filepath.Join(root, "work", fmt.Sprintf("p%d", i/4))
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		sid := fmt.Sprintf("5e550000-0000-4000-8000-%012d", i)
		s := perfguard.ClaudeSession{SessionID: sid, Cwd: cwd}
		proj := filepath.Join(projects, pathpolicy.ClaudeFolderName(cwd))
		write(filepath.Join(proj, sid+".jsonl"), s.Lines(0, 4))
		if i%2 == 0 {
			agentID := fmt.Sprintf("a%016x", i)
			sub := filepath.Join(proj, sid, "subagents", "agent-"+agentID)
			line := fmt.Sprintf(`{"parentUuid":null,"isSidechain":true,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","agentId":%q,"type":"user","message":{"role":"user","content":"subagent task %d"},"uuid":"5e550000-0000-4000-a000-%012d","timestamp":"2026-09-01T00:00:10.000Z"}`+"\n", cwd, sid, agentID, i, i)
			write(sub+".jsonl", []byte(line))
			write(sub+".meta.json", []byte(`{"agentType":"general-purpose","description":"synthetic","spawnDepth":1}`))
			write(filepath.Join(proj, sid, "tool-results", "r1.txt"), []byte("synthetic tool output\n"))
		} else {
			day := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i)
			id := fmt.Sprintf("019a0000-0000-7000-8000-%012x", i)
			p := filepath.Join(root, "codex", "sessions", day.Format("2006/01/02"), "rollout-"+day.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
			ts := day.Format("2006-01-02T15:04:05.000Z")
			write(p, []byte(fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,"originator":"codex-tui","cli_version":"0.154.0","source":"cli","model_provider":"openai"}}`+"\n"+
				`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"synthetic prompt %d"}]}}`+"\n"+
				`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic answer %d"}]}}`+"\n",
				ts, id, ts, cwd, ts, i, ts, i)))
		}
	}
	seed, err := os.ReadFile(devinSeed)
	if err != nil {
		t.Fatal(err)
	}
	dpath := filepath.Join(root, "devin", "cli", "sessions.db")
	if err := os.MkdirAll(filepath.Dir(dpath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dpath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	return files
}

// sweepPathPackages are the packages a sweep runs code of: the agent, the
// discovery and gate code, and the parsers a changed file reaches.
var sweepPathPackages = []string{".", "../transcript", "../transcript/claude", "../transcript/codex", "../transcript/devin"}

// directFSAllowed are direct file system uses off the sweep path, by
// file and expression.
var directFSAllowed = map[string]bool{
	"proc_linux.go os.ReadFile": true, // /proc, for presence
}

// TestNoChangeSweepScales counts file system calls through fsprobe; a
// direct one on the agent's paths would escape it. This fails on any
// reference to a stat, open, listing or walk of package os or
// path/filepath (a call or a function value such as stat := os.Stat),
// os.DirFS, and any Readdir, Readdirnames or ReadDir method (*os.File
// listings), in every non-test file of the packages a sweep runs.
func TestFileSystemCallsGoThroughProbe(t *testing.T) {
	banned := map[string]map[string]bool{
		"os": {"Open": true, "OpenFile": true, "ReadFile": true, "ReadDir": true, "Stat": true, "Lstat": true,
			"DirFS": true, "OpenRoot": true, "OpenInRoot": true},
		"path/filepath": {"Walk": true, "WalkDir": true, "Glob": true, "EvalSymlinks": true},
		"io/fs":         {"ReadDir": true, "ReadFile": true, "Stat": true, "WalkDir": true, "Glob": true, "Sub": true},
	}
	methods := map[string]bool{"Readdir": true, "Readdirnames": true, "ReadDir": true}
	fset := token.NewFileSet()
	checked := 0
	for _, dir := range sweepPathPackages {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			checked++
			imports := map[string]string{} // local name -> import path
			for _, im := range f.Imports {
				path, _ := strconv.Unquote(im.Path.Value)
				local := path[strings.LastIndex(path, "/")+1:]
				if im.Name != nil {
					local = im.Name.Name
				}
				if local == "." && banned[path] != nil {
					// A dot import makes os.Stat a bare Stat, which the
					// selector check below cannot see.
					t.Errorf("%s: dot import of %s: use fsprobe", fset.Position(im.Pos()), path)
				}
				imports[local] = path
			}
			rel := filepath.ToSlash(filepath.Clean(name))
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				var expr string
				if x, ok := sel.X.(*ast.Ident); ok && imports[x.Name] != "" {
					if !banned[imports[x.Name]][sel.Sel.Name] {
						return true
					}
					expr = x.Name + "." + sel.Sel.Name
				} else if methods[sel.Sel.Name] {
					expr = "(…)." + sel.Sel.Name
				} else {
					return true
				}
				if directFSAllowed[rel+" "+expr] {
					return true
				}
				t.Errorf("%s: %s: use fsprobe, not a direct file system call", fset.Position(sel.Pos()), expr)
				return true
			})
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d files", checked)
	}
}
