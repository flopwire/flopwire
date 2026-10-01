package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Every flopwire command, `flopwire version` included, runs the package
// initialisers of every package linked into the binary. Work done there is
// a fixed cost on each CLI query. An eager 1MB random table in
// internal/synthcorpus once added about 5ms to every command.
//
// Wall time is too noisy to gate in CI, so this guard reads the heap bytes
// each package's init allocates (GODEBUG=inittrace=1), which depend only
// on the code. It re-runs this test binary, which links every package of
// the flopwire binary, with no test selected.

// initBytesCap bounds the init allocation of one flopwire package.
const initBytesCap = 256 << 10

// initBytesKnown are the packages whose init is allowed to exceed
// initBytesCap, each with its own ceiling. Raise one only with a reason.
var initBytesKnown = map[string]int{
	"github.com/flopwire/flopwire/internal/redact": 2 << 20,    // compiled redaction rules, used by every ingest and read
	"github.com/flopwire/flopwire/internal/agent":  1280 << 10, // package-level regexps and tables
}

// initBytesTotalCap bounds the sum over all flopwire packages.
const initBytesTotalCap = 7 << 19 // 3.5MiB; 2.9MB today

func TestPackageInitCost(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rerun test binary: %v\n%s", err, out)
	}
	pkgs := parseInitTrace(out)
	if len(pkgs) == 0 {
		t.Fatalf("no init trace in output:\n%s", out)
	}
	total := 0
	for pkg, n := range pkgs {
		if !strings.HasPrefix(pkg, "github.com/flopwire/flopwire/") {
			continue
		}
		total += n
		limit, ok := initBytesKnown[pkg]
		if !ok {
			limit = initBytesCap
		}
		if n > limit {
			t.Errorf("init of %s allocates %d bytes, limit %d: build large tables on first use (sync.OnceValue), not at package init", pkg, n, limit)
		}
	}
	if total > initBytesTotalCap {
		t.Errorf("flopwire package inits allocate %d bytes in total, limit %d", total, initBytesTotalCap)
	}
}

// parseInitTrace maps package to init heap bytes from inittrace lines:
//
//	init example.com/pkg @0.5 ms, 0.01 ms clock, 176 bytes, 4 allocs
func parseInitTrace(out []byte) map[string]int {
	pkgs := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[0] != "init" || f[8] != "bytes," {
			continue
		}
		n, err := strconv.Atoi(f[7])
		if err != nil {
			continue
		}
		pkgs[f[1]] += n
	}
	return pkgs
}

func TestParseInitTrace(t *testing.T) {
	got := parseInitTrace([]byte("init a/b @1 ms, 0.1 ms clock, 176 bytes, 4 allocs\nnoise\ninit c @2 ms, 5.2 ms clock, 1138480 bytes, 2828 allocs\n"))
	if got["a/b"] != 176 || got["c"] != 1138480 || len(got) != 2 {
		t.Fatalf("parseInitTrace = %v", got)
	}
}
