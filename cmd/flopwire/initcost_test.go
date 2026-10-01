package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// depInitBytesCap is the looser bound for a third-party or standard library
// package. It catches a dependency that starts building large tables at
// init after an upgrade. The race detector makes some dependency inits
// allocate more and unevenly (prometheus: 64KB, 0.7-1.5MB under -race), so
// this cap and allInitBytesTotalCap are checked only without -race.
const depInitBytesCap = 512 << 10

// initBytesKnown are the packages whose init is allowed to exceed their
// cap, each with its own ceiling. Raise one only with a reason.
var initBytesKnown = map[string]int{
	"github.com/flopwire/flopwire/internal/redact": 2 << 20,    // compiled redaction rules, used by every ingest and read
	"github.com/flopwire/flopwire/internal/agent":  1280 << 10, // package-level regexps and tables
	"modernc.org/libc/honnef.co/go/netdb":          4 << 20,    // services/protocols tables of the pure-Go SQLite libc; 3.5MB today
}

// initBytesTotalCap bounds the sum over all flopwire packages.
const initBytesTotalCap = 7 << 19 // 3.5MiB; 2.9MB today

// allInitBytesTotalCap bounds the sum over every package in the binary.
const allInitBytesTotalCap = 9 << 20 // 9MiB; 7.2MB today

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
	total, all := 0, 0
	for pkg, n := range pkgs {
		all += n
		limit := depInitBytesCap
		if strings.HasPrefix(pkg, "github.com/flopwire/flopwire/") {
			total += n
			limit = initBytesCap
		} else if raceEnabled {
			continue
		}
		if known, ok := initBytesKnown[pkg]; ok {
			limit = known
		}
		if n > limit {
			t.Errorf("init of %s allocates %d bytes, limit %d: build large tables on first use (sync.OnceValue), not at package init", pkg, n, limit)
		}
	}
	if total > initBytesTotalCap {
		t.Errorf("flopwire package inits allocate %d bytes in total, limit %d", total, initBytesTotalCap)
	}
	if !raceEnabled && all > allInitBytesTotalCap {
		t.Errorf("package inits allocate %d bytes in total, limit %d", all, allInitBytesTotalCap)
	}
}

// A package that runs a program at init costs every command a fork and
// exec. github.com/rs/xid v1.6.0 ran ioreg at init on darwin, about 20ms.
// Wall time is too noisy to gate, so this test reruns the package inits and
// `flopwire version` with PATH holding a logging shim for every program
// name on the real PATH. Any program found by PATH lookup leaves a line in
// the log. Programs run by absolute path are not caught.
func TestInitAndVersionRunNoProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shims are shell scripts")
	}
	dir := t.TempDir()
	shims := filepath.Join(dir, "bin")
	if err := os.Mkdir(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"${0##*/}\" >> \"$FLOPWIRE_EXEC_LOG\"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := append(filepath.SplitList(os.Getenv("PATH")), "/bin", "/sbin", "/usr/bin", "/usr/sbin")
	n := 0
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			link := filepath.Join(shims, e.Name())
			if _, err := os.Lstat(link); err == nil {
				continue
			}
			if os.Symlink(shim, link) == nil {
				n++
			}
		}
	}
	if n == 0 {
		t.Fatal("no programs found on PATH to shim")
	}
	log := filepath.Join(dir, "exec.log")
	cmd := exec.Command(os.Args[0], "-test.run=^TestVersionHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "PATH="+shims, "FLOPWIRE_EXEC_LOG="+log, "FLOPWIRE_VERSION_HELPER=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rerun test binary: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: TestVersionHelper")) {
		t.Fatalf("version helper did not run:\n%s", out)
	}
	ran, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(ran) > 0 {
		t.Errorf("package init or `flopwire version` ran programs found on PATH:\n%s", ran)
	}
}

// TestVersionHelper is the child process of TestInitAndVersionRunNoProgram.
func TestVersionHelper(t *testing.T) {
	if os.Getenv("FLOPWIRE_VERSION_HELPER") != "1" {
		t.Skip("helper process for TestInitAndVersionRunNoProgram")
	}
	if err := run(context.Background(), []string{"version"}); err != nil {
		t.Fatal(err)
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
