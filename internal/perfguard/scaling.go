package perfguard

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// Class is how an operation's cost may grow with input size.
type Class int

const (
	// Linear: the size-dependent cost at k·n is at most 1.5·k times that
	// at n (bulk work: reparse, rules upgrade, delete). A quadratic step
	// shows as about k².
	Linear Class = iota
	// Constant: the size-dependent cost at k·n is at most 2 times that at
	// n, or 1 + (k-1)/2 times when k < 3, so a linear term (×k) always
	// fails (incremental work: an append, one retrieval page).
	Constant
)

func (c Class) String() string {
	switch c {
	case Linear:
		return "linear"
	case Constant:
		return "constant"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// Bound is the largest allowed cost ratio between sizes k·n and n.
func (c Class) Bound(k int) float64 {
	if c == Constant {
		return min(2, 1+float64(k-1)/2)
	}
	return 1.5 * float64(k)
}

// Ratio denominators are at least these floors, so a handful of fixed
// rows or statements at the small size cannot fail a guard on its own. A
// real per-item cost at n≈500 is far above them.
const (
	minBase           = 16
	minBaseStatements = 4
)

func ratio(small, large, floor int64) float64 {
	return float64(max(large, 0)) / float64(max(small, floor))
}

// AssertScaling fails t when the operation's rows touched, heap pages
// passed over by sequential scans (TableCost.SeqPages, which sees scans
// over dead tuples that rows miss), statements sent, SQLite pages fetched
// (Cost.SQLitePages) or file system calls (Cost.FS: stats, listings, opens)
// grow faster than class allows between sizes n and k·n. Postgres blocks
// are reported but not gated (see TableCost.Blocks).
//
// run builds a fixture of the given size, normally in a fresh pgtest
// database (or SQLite file), and returns the Measure (or MeasureSQLite)
// of only the operation under test.
//
// run also runs at size 1 and that baseline is subtracted from both
// sizes before the ratio: a fixed per-operation cost (setup lookups, a
// scan of an unrelated table) would otherwise pull the ratio toward 1 and
// hide a quadratic term from Linear or a linear term from Constant.
//
// The failure lists rows and blocks per table at each size.
func AssertScaling(t testing.TB, class Class, n, k int, run func(t testing.TB, n int) Cost) {
	t.Helper()
	if n < 2 || k < 2 {
		t.Fatalf("perfguard: AssertScaling needs n >= 2 and k >= 2, got n=%d k=%d", n, k)
	}
	small := run(t, n)
	large := run(t, k*n)
	base := run(t, 1)
	bound := class.Bound(k)
	bt, st, lt := base.Total(), small.Total(), large.Total()
	var over, report []string
	check := func(what string, b, s, l, floor int64) {
		r := ratio(s-b, l-b, floor)
		line := fmt.Sprintf("%s %d → %d less baseline %d (×%.1f)", what, s, l, b, r)
		report = append(report, line)
		if r > bound {
			over = append(over, line)
		}
	}
	check("rows", bt.Rows(), st.Rows(), lt.Rows(), minBase)
	check("seq pages", bt.SeqPages, st.SeqPages, lt.SeqPages, minBase)
	if small.Statements >= 0 && large.Statements >= 0 {
		check("statements", max(base.Statements, 0), small.Statements, large.Statements, minBaseStatements)
	}
	if small.SQLitePages > 0 || large.SQLitePages > 0 {
		check("sqlite pages", base.SQLitePages, small.SQLitePages, large.SQLitePages, minBase)
	}
	if small.FS != (FSCost{}) || large.FS != (FSCost{}) {
		check("fs stats", base.FS.Stats, small.FS.Stats, large.FS.Stats, minBase)
		check("fs lists", base.FS.Lists, small.FS.Lists, large.FS.Lists, minBase)
		check("fs opens", base.FS.Opens, small.FS.Opens, large.FS.Opens, minBase)
	}
	if len(over) == 0 {
		t.Logf("perfguard: %s scaling ok at n=%d k=%d (bound ×%.1f): %s; blocks %d → %d",
			class, n, k, bound, strings.Join(report, ", "), st.Blocks(), lt.Blocks())
		return
	}
	t.Errorf("perfguard: cost is not %s (n=%d → %d, bound ×%.1f): %s\n%s",
		class, n, k*n, bound, strings.Join(over, ", "), breakdown(base, small, large, bound))
}

// breakdown lists each table's cost at both sizes, flagging the tables
// whose rows grew past bound.
func breakdown(base, small, large Cost, bound float64) string {
	names := map[string]bool{}
	for _, c := range []Cost{small, large} {
		for name := range c.Tables {
			names[name] = true
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-40s %23s %23s %23s\n", "table", "rows small→large", "seq pages small→large", "blocks small→large")
	for _, name := range slices.Sorted(maps.Keys(names)) {
		z, s, l := base.Tables[name], small.Tables[name], large.Tables[name]
		if s.idle() && l.idle() {
			continue
		}
		flag := ""
		if ratio(s.Rows()-z.Rows(), l.Rows()-z.Rows(), minBase) > bound ||
			ratio(s.SeqPages-z.SeqPages, l.SeqPages-z.SeqPages, minBase) > bound {
			flag = "  <-- grows past bound"
		}
		fmt.Fprintf(&b, "%-40s %11d→%-11d %11d→%-11d %11d→%-11d%s\n", name, s.Rows(), l.Rows(), s.SeqPages, l.SeqPages, s.Blocks(), l.Blocks(), flag)
	}
	fmt.Fprintf(&b, "baseline (n=1): %s\n", base)
	fmt.Fprintf(&b, "small: %s\nlarge: %s", small, large)
	return b.String()
}
