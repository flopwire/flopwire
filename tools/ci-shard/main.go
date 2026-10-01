// ci-shard splits the module's Go packages into CI test shards balanced by
// recorded test duration, checks that a set of shard runs covered every
// package exactly once, and rebuilds the duration file from those runs.
//
//	ci-shard -shards 4 -index 0              packages for shard 0, one per line
//	ci-shard -shards 4 -verify go-test*.json  every package ran in exactly one file
//	ci-shard -refresh go-test*.json           new duration file on stdout
//
// Assignment is greedy longest-processing-time: packages sorted by duration
// (longest first, then by path), each placed on the least-loaded shard
// (lowest index on ties). The output depends only on the package list and
// the duration file. A package missing from the file weighs -default
// seconds, so a new package still lands on some shard.
//
// Durations live in scripts/ci-test-durations.json, keyed by package path
// relative to the module, in seconds under `go test -race` on CI. Refresh
// them by hand from a green run (CI uploads each shard's go-test.json):
//
//	gh run download <run-id> -p 'go-test-*' -D /tmp/shards
//	go run ./tools/ci-shard -refresh /tmp/shards/*/go-test.json >scripts/ci-test-durations.json
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	shards := flag.Int("shards", 0, "number of shards")
	index := flag.Int("index", -1, "shard to print (0-based)")
	verify := flag.Bool("verify", false, "check that the go test -json files given as arguments cover every package exactly once")
	refresh := flag.Bool("refresh", false, "print a duration file built from the go test -json files given as arguments")
	durFile := flag.String("durations", "scripts/ci-test-durations.json", "duration file, relative to the module root")
	def := flag.Float64("default", 10, "seconds assumed for a package missing from the duration file")
	flag.Parse()
	if err := run(*shards, *index, *verify, *refresh, *durFile, *def, flag.Args(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ci-shard:", err)
		os.Exit(1)
	}
}

func run(shards, index int, verify, refresh bool, durFile string, def float64, args []string, stdout io.Writer) error {
	mod, pkgs, err := goList()
	if err != nil {
		return err
	}
	switch {
	case refresh:
		elapsed, _, err := readEvents(args)
		if err != nil {
			return err
		}
		out, err := encodeDurations(elapsed, mod)
		if err != nil {
			return err
		}
		_, err = stdout.Write(out)
		return err
	case verify:
		_, runs, err := readEvents(args)
		if err != nil {
			return err
		}
		if err := checkCoverage(pkgs, runs); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "all %d packages ran exactly once\n", len(pkgs))
		return nil
	}
	if shards < 1 || index < 0 || index >= shards {
		return fmt.Errorf("need -shards >= 1 and 0 <= -index < -shards (got %d, %d)", shards, index)
	}
	raw, err := readRepoFile(durFile)
	if err != nil {
		return err
	}
	dur, err := decodeDurations(raw, mod)
	if err != nil {
		return fmt.Errorf("%s: %w", durFile, err)
	}
	plan := assign(pkgs, dur, def, shards)
	for i, s := range plan {
		fmt.Fprintf(os.Stderr, "shard %d: %d packages, ~%.0fs\n", i, len(s.Packages), s.Load)
	}
	for _, p := range plan[index].Packages {
		fmt.Fprintln(stdout, p)
	}
	return nil
}

// Shard is one bucket of the plan.
type Shard struct {
	Packages []string
	Load     float64
}

// assign splits pkgs into n shards by greedy longest-processing-time. dur
// maps full import paths to seconds; packages absent from it weigh def.
// Each shard's package list is sorted.
func assign(pkgs []string, dur map[string]float64, def float64, n int) []Shard {
	weight := func(p string) float64 {
		if d, ok := dur[p]; ok {
			return d
		}
		return def
	}
	order := slices.Clone(pkgs)
	slices.SortFunc(order, func(a, b string) int {
		if wa, wb := weight(a), weight(b); wa != wb {
			if wa > wb {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	plan := make([]Shard, n)
	for _, p := range order {
		best := 0
		for i := range plan {
			if plan[i].Load < plan[best].Load {
				best = i
			}
		}
		plan[best].Packages = append(plan[best].Packages, p)
		plan[best].Load += weight(p)
	}
	for i := range plan {
		slices.Sort(plan[i].Packages)
	}
	return plan
}

// event is the subset of a test2json record this tool reads.
type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
}

// readEvents reads go test -json files and returns, per package, the
// elapsed seconds of its final event and how many final events it had.
// A final event is a package-level pass, fail or skip (skip means no test
// files).
func readEvents(files []string) (map[string]float64, map[string]int, error) {
	if len(files) == 0 {
		return nil, nil, errors.New("no go test -json files given")
	}
	elapsed := map[string]float64{}
	runs := map[string]int{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, nil, err
		}
		err = scanEvents(fh, elapsed, runs)
		fh.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", f, err)
		}
	}
	return elapsed, runs, nil
}

func scanEvents(r io.Reader, elapsed map[string]float64, runs map[string]int) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 || line[0] != '{' {
			continue // go test may print build errors outside JSON
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		if e.Test != "" || e.Package == "" {
			continue
		}
		switch e.Action {
		case "pass", "fail", "skip":
			elapsed[e.Package] = e.Elapsed
			runs[e.Package]++
		}
	}
	return sc.Err()
}

// checkCoverage reports packages that did not run, ran more than once, or
// ran but are not in the package list.
func checkCoverage(pkgs []string, runs map[string]int) error {
	var problems []string
	listed := map[string]bool{}
	for _, p := range pkgs {
		listed[p] = true
		switch n := runs[p]; {
		case n == 0:
			problems = append(problems, "not run: "+p)
		case n > 1:
			problems = append(problems, fmt.Sprintf("ran %d times: %s", n, p))
		}
	}
	for p := range runs {
		if !listed[p] {
			problems = append(problems, "ran but not in go list ./...: "+p)
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return errors.New("shard coverage:\n  " + strings.Join(problems, "\n  "))
	}
	return nil
}

func decodeDurations(raw []byte, mod string) (map[string]float64, error) {
	var short map[string]float64
	if err := json.Unmarshal(raw, &short); err != nil {
		return nil, err
	}
	dur := make(map[string]float64, len(short))
	for k, v := range short {
		dur[mod+"/"+k] = v
	}
	return dur, nil
}

// encodeDurations writes seconds rounded to 0.1, keyed by module-relative
// path, sorted, so a refresh diffs cleanly.
func encodeDurations(elapsed map[string]float64, mod string) ([]byte, error) {
	short := make(map[string]float64, len(elapsed))
	for p, s := range elapsed {
		k, ok := strings.CutPrefix(p, mod+"/")
		if !ok {
			continue
		}
		short[k] = math.Round(s*10) / 10
	}
	out, err := json.MarshalIndent(short, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// moduleRoot is the directory holding go.mod, so the tool and its tests
// see the whole module from any working directory.
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("not inside a Go module")
	}
	return filepath.Dir(gomod), nil
}

func readRepoFile(rel string) ([]byte, error) {
	if filepath.IsAbs(rel) {
		return os.ReadFile(rel)
	}
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(root, rel))
}

// goList returns the module path and every package `go list ./...` names
// from the module root.
func goList() (mod string, pkgs []string, err error) {
	root, err := moduleRoot()
	if err != nil {
		return "", nil, err
	}
	list := func(args ...string) (string, error) {
		cmd := exec.Command("go", args...)
		cmd.Dir = root
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
		return string(out), nil
	}
	m, err := list("list", "-m")
	if err != nil {
		return "", nil, err
	}
	out, err := list("list", "./...")
	if err != nil {
		return "", nil, err
	}
	return strings.TrimSpace(m), strings.Fields(out), nil
}
