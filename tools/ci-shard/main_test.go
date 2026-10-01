package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestAssignBalancesLongestFirst(t *testing.T) {
	dur := map[string]float64{"a": 10, "b": 8, "c": 6, "d": 5, "e": 4}
	plan := assign([]string{"e", "d", "c", "b", "a", "new"}, dur, 1, 2)
	// a->0 (10), b->1 (8), c->1 (14), d->0 (15), e->1 (18), new->0 (16).
	want := [][]string{{"a", "d", "new"}, {"b", "c", "e"}}
	for i, s := range plan {
		if !slices.Equal(s.Packages, want[i]) {
			t.Fatalf("shard %d = %v, want %v", i, s.Packages, want[i])
		}
	}
	if plan[0].Load != 16 || plan[1].Load != 18 {
		t.Fatalf("loads = %v, %v", plan[0].Load, plan[1].Load)
	}
}

func TestAssignPartitionsAndIsDeterministic(t *testing.T) {
	var pkgs []string
	dur := map[string]float64{}
	for i := range 40 {
		p := fmt.Sprintf("m/p%02d", i)
		pkgs = append(pkgs, p)
		if i%3 != 0 {
			dur[p] = float64(i % 7) // many ties
		}
	}
	first := assign(pkgs, dur, 3, 4)
	reversed := slices.Clone(pkgs)
	slices.Reverse(reversed)
	again := assign(reversed, dur, 3, 4)
	seen := map[string]int{}
	for i := range first {
		if !slices.Equal(first[i].Packages, again[i].Packages) {
			t.Fatalf("shard %d depends on input order: %v vs %v", i, first[i].Packages, again[i].Packages)
		}
		for _, p := range first[i].Packages {
			seen[p]++
		}
	}
	for _, p := range pkgs {
		if seen[p] != 1 {
			t.Fatalf("%s assigned %d times", p, seen[p])
		}
	}
}

func TestAssignMoreShardsThanPackages(t *testing.T) {
	plan := assign([]string{"a"}, nil, 1, 3)
	if len(plan) != 3 || !slices.Equal(plan[0].Packages, []string{"a"}) || plan[1].Packages != nil {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestScanEventsCountsPackageResults(t *testing.T) {
	in := `{"Action":"start","Package":"m/a"}
{"Action":"run","Package":"m/a","Test":"TestX"}
{"Action":"pass","Package":"m/a","Test":"TestX","Elapsed":1}
{"Action":"pass","Package":"m/a","Elapsed":2.5}
# m/c
c.go:1: syntax error
{"Action":"skip","Package":"m/b","Elapsed":0}
{"Action":"fail","Package":"m/c","Elapsed":0.1}
`
	elapsed, runs := map[string]float64{}, map[string]int{}
	if err := scanEvents(strings.NewReader(in), elapsed, runs); err != nil {
		t.Fatal(err)
	}
	if elapsed["m/a"] != 2.5 || runs["m/a"] != 1 || runs["m/b"] != 1 || runs["m/c"] != 1 || len(runs) != 3 {
		t.Fatalf("elapsed=%v runs=%v", elapsed, runs)
	}
}

func TestCheckCoverage(t *testing.T) {
	pkgs := []string{"m/a", "m/b", "m/c"}
	if err := checkCoverage(pkgs, map[string]int{"m/a": 1, "m/b": 1, "m/c": 1}); err != nil {
		t.Fatal(err)
	}
	err := checkCoverage(pkgs, map[string]int{"m/a": 2, "m/c": 1, "m/x": 1})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"not run: m/b", "ran 2 times: m/a", "not in go list ./...: m/x"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestDurationsRoundTrip(t *testing.T) {
	out, err := encodeDurations(map[string]float64{"m/x/b": 1.26, "m/a": 30, "other/z": 5}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), "{\n  \"a\": 30,\n  \"x/b\": 1.3\n}\n"; got != want {
		t.Fatalf("encode = %q, want %q", got, want)
	}
	dur, err := decodeDurations(out, "m")
	if err != nil {
		t.Fatal(err)
	}
	if dur["m/a"] != 30 || dur["m/x/b"] != 1.3 || len(dur) != 2 {
		t.Fatalf("decode = %v", dur)
	}
}

// The committed duration file must parse and name only real packages, so a
// rename does not silently drop a heavy package to the default weight.
func TestCommittedDurationsNameRealPackages(t *testing.T) {
	mod, pkgs, err := goList()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readRepoFile("scripts/ci-test-durations.json")
	if err != nil {
		t.Fatal(err)
	}
	dur, err := decodeDurations(raw, mod)
	if err != nil {
		t.Fatal(err)
	}
	for p := range dur {
		if !slices.Contains(pkgs, p) {
			t.Errorf("duration file names %s, which go list ./... does not", strings.TrimPrefix(p, mod+"/"))
		}
	}
}
