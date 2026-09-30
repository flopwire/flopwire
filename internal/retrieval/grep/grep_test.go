package grep

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func query(t *testing.T, s Spec) format.GrepQuery {
	t.Helper()
	var q format.GrepQuery
	if err := s.Query(&q); err != nil {
		t.Fatal(err)
	}
	return q
}

func TestSpecSmartCaseWordsAndAlternation(t *testing.T) {
	for _, tc := range []struct {
		spec  Spec
		pat   string
		fixed bool
		cs    bool
	}{
		{Spec{Patterns: []string{"retry"}}, "retry", false, false},
		{Spec{Patterns: []string{"Retry"}}, "Retry", false, true},
		{Spec{Patterns: []string{`\S+\.go`}}, `\S+\.go`, false, false}, // \S is not an uppercase letter
		{Spec{Patterns: []string{`\p{Lu}x`}}, `\p{Lu}x`, false, false},
		{Spec{Patterns: []string{"Retry"}, IgnoreCase: true}, "Retry", false, false},
		{Spec{Patterns: []string{"retry"}, CaseSensitive: true}, "retry", false, true},
		{Spec{Patterns: []string{"a.b("}, Fixed: true}, "a.b(", true, false},
		{Spec{Patterns: []string{"a.b", "c"}, Fixed: true}, `(?:a\.b)|(?:c)`, false, false},
		{Spec{Patterns: []string{"go", "rust"}, Word: true}, `\b(?:(?:go)|(?:rust))\b`, false, false},
	} {
		q := query(t, tc.spec)
		if q.Pattern != tc.pat || q.Fixed != tc.fixed || q.CaseSensitive != tc.cs {
			t.Errorf("%+v: got %q fixed=%v cs=%v", tc.spec, q.Pattern, q.Fixed, q.CaseSensitive)
		}
	}
	if err := (Spec{}).Query(&format.GrepQuery{}); err == nil {
		t.Fatal("no pattern accepted")
	}
}

type row struct{ session, text string }

func collect(t *testing.T, q format.GrepQuery, rows []row) (*Collector, Result) {
	t.Helper()
	p, err := Compile(q)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCollector(p.Re, q)
	for i, r := range rows {
		if !c.Add(Row{Session: r.session, SHA: sha256.Sum256([]byte(r.text)), Text: r.text, Ref: i}) {
			break
		}
	}
	return c, c.Result()
}

func refs(fs []Found) string {
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprint(f.Ref))
	}
	return strings.Join(out, ",")
}

func TestCollector(t *testing.T) {
	rows := []row{
		{"s1", "retry once\nok\nretry twice"}, // 0
		{"s1", "nothing here"},                // 1
		{"s2", "retry once\nok\nretry twice"}, // 2: a copy of 0
		{"s2", "RETRY loud"},                  // 3
		{"s1", "third retry"},                 // 4
		{"s3", "retry in s3"},                 // 5
	}
	q := query(t, Spec{Patterns: []string{"retry"}})
	_, res := collect(t, q, rows)
	if refs(res.Hits) != "0,3,4,5" || res.Total != 4 || res.TotalSessions != 3 || res.Capped || res.Next != 0 {
		t.Fatalf("all: %s %+v", refs(res.Hits), res)
	}
	if res.Hits[0].Copies != 1 || len(res.Hits[0].Lines) != 2 || res.Hits[0].Lines[1].N != 3 || res.Hits[0].Lines[1].Text != "retry twice" {
		t.Fatalf("lines and copies: %+v", res.Hits[0])
	}
	q.Limit, q.Offset = 2, 1
	if _, res = collect(t, q, rows); refs(res.Hits) != "3,4" || res.Next != 3 || res.Total != 4 {
		t.Fatalf("page: %s %+v", refs(res.Hits), res)
	}
	q.Limit, q.Offset, q.MaxPerSession = 0, 0, 1
	if _, res = collect(t, q, rows); refs(res.Hits) != "0,3,5" {
		t.Fatalf("-m 1: %s", refs(res.Hits))
	}
	q.MaxPerSession, q.Mode = 0, format.ModeCount
	if _, res = collect(t, q, rows); len(res.Hits) != 0 || len(res.Sessions) != 3 || res.Sessions[0].Hits != 2 || res.Sessions[0].Ref != 0 || res.Sessions[1].Hits != 2 {
		t.Fatalf("count: %+v", res.Sessions)
	}
	q.Limit, q.Offset = 1, 1
	if _, res = collect(t, q, rows); len(res.Sessions) != 1 || res.Sessions[0].Session != "s2" || res.Next != 2 {
		t.Fatalf("count page: %+v", res)
	}
	// Context lines, merged and numbered; separated groups stay apart.
	text := "a\nretry 1\nb\nc\nd\ne\nretry 2\nf"
	q = query(t, Spec{Patterns: []string{"retry"}})
	q.Before, q.After = 1, 1
	_, res = collect(t, q, []row{{"s", text}})
	var got []string
	for _, l := range res.Hits[0].Lines {
		got = append(got, fmt.Sprintf("%d%v", l.N, l.Match))
	}
	if strings.Join(got, " ") != "1false 2true 3false 6false 7true 8false" {
		t.Fatalf("context: %s", got)
	}
	// A message with many matching lines keeps MaxLines and counts the rest.
	var b strings.Builder
	for i := range MaxLines + 5 {
		fmt.Fprintf(&b, "retry %d\n", i)
	}
	_, res = collect(t, query(t, Spec{Patterns: []string{"retry"}}), []row{{"s", b.String()}})
	if len(res.Hits[0].Lines) != MaxLines || res.Hits[0].MoreLines != 5 {
		t.Fatalf("line cap: %d lines, %d more", len(res.Hits[0].Lines), res.Hits[0].MoreLines)
	}
	// Long lines are clipped around the match.
	long := strings.Repeat("x", 1000) + "needle" + strings.Repeat("y", 1000)
	_, res = collect(t, query(t, Spec{Patterns: []string{"needle"}}), []row{{"s", long}})
	if l := res.Hits[0].Lines[0].Text; len(l) > LineWidth+10 || !strings.Contains(l, "needle") {
		t.Fatalf("clip: %d %q", len(l), l)
	}
}

// Counting after a full page stops at countExtra hits or countTime; the
// totals are then lower bounds and the page says there is more.
func TestCollectorCountingBounds(t *testing.T) {
	q := query(t, Spec{Patterns: []string{"x"}})
	q.Limit = 2
	var rows []row
	for i := range countExtra + 50 {
		rows = append(rows, row{fmt.Sprint(i % 7), fmt.Sprintf("x %d", i)})
	}
	_, res := collect(t, q, rows)
	if !res.Capped || res.Total != 2+countExtra || res.Next != 2 {
		t.Fatalf("count cap: %+v", res)
	}
	p, _ := Compile(q)
	c := NewCollector(p.Re, q)
	now := time.Now()
	c.SetClock(func() time.Time { return now })
	c.Add(Row{Session: "a", SHA: [32]byte{1}, Text: "x"})
	c.Add(Row{Session: "a", SHA: [32]byte{2}, Text: "x"})
	now = now.Add(2 * countTime)
	if c.Add(Row{Session: "a", SHA: [32]byte{3}, Text: "x"}) || !c.Result().Capped {
		t.Fatal("count time")
	}
}

// -l and -c list every session with a match, including one whose only
// match is a copy of another session's message; content collapses it.
func TestCollectorSessionsKeepCopies(t *testing.T) {
	rows := []row{{"s1", "same flaky text"}, {"s2", "same flaky text"}, {"s2", "same flaky text"}}
	q := query(t, Spec{Patterns: []string{"flaky"}})
	for _, mode := range []string{format.ModeSessions, format.ModeCount} {
		q.Mode = mode
		_, res := collect(t, q, rows)
		if len(res.Sessions) != 2 || res.TotalSessions != 2 || res.Sessions[1].Session != "s2" || res.Sessions[1].Hits != 1 {
			t.Fatalf("%s: %+v", mode, res)
		}
	}
	q.Mode = ""
	if _, res := collect(t, q, rows); len(res.Hits) != 1 || res.Hits[0].Copies != 2 {
		t.Fatalf("content: %+v", res)
	}
}

func TestOnlyMatchingAndMultiline(t *testing.T) {
	text := "alpha retry=3\nbeta\nretry again retry\nupload.test timeout\nexit 1"
	q := query(t, Spec{Patterns: []string{`retry\w*`}})
	q.OnlyMatching = true
	_, r := collect(t, q, []row{{"s", text}})
	var got []string
	for _, l := range r.Hits[0].Lines {
		got = append(got, fmt.Sprintf("%d:%s", l.N, l.Text))
	}
	if strings.Join(got, " ") != "1:retry 3:retry 3:retry" {
		t.Fatalf("-o lines %v", got)
	}
	// -U: . crosses lines within the message; every line of the match
	// prints, marked as matching.
	q = query(t, Spec{Patterns: []string{`timeout.*1`}})
	q.Multiline = true
	_, r = collect(t, q, []row{{"s", text}, {"t", "timeout\nnot here"}})
	if len(r.Hits) != 1 || len(r.Hits[0].Lines) != 2 || r.Hits[0].Lines[0].N != 4 || r.Hits[0].Lines[1].N != 5 || !r.Hits[0].Lines[1].Match {
		t.Fatalf("-U hit %+v", r.Hits)
	}
	// Without -U the same pattern does not cross the line break.
	q.Multiline = false
	if _, r = collect(t, q, []row{{"s", text}}); len(r.Hits) != 0 {
		t.Fatalf("matched across lines without -U: %+v", r.Hits)
	}
	// -U -o shows the newline as ⏎.
	q.Multiline, q.OnlyMatching = true, true
	if _, r = collect(t, q, []row{{"s", text}}); r.Hits[0].Lines[0].Text != "timeout⏎exit 1" {
		t.Fatalf("-U -o %+v", r.Hits[0].Lines)
	}
	// A match spanning more than MaxMultiLines lines is cut and counted.
	long := "start\n" + strings.Repeat("x\n", 80) + "end"
	q = query(t, Spec{Patterns: []string{`start.*end`}})
	q.Multiline = true
	if _, r = collect(t, q, []row{{"s", long}}); len(r.Hits[0].Lines) != MaxMultiLines || r.Hits[0].MoreLines != 82-MaxMultiLines {
		t.Fatalf("long -U hit: %d lines, %d more", len(r.Hits[0].Lines), r.Hits[0].MoreLines)
	}
}

func TestRelevanceSort(t *testing.T) {
	rows := []row{
		{"s1", "retry once"},             // 0: 1 match, newest
		{"s2", "retry retry retry"},      // 1: 3 matches
		{"s1", "retry retry"},            // 2: 2 matches
		{"s3", "retry retry retry"},      // 3: a copy of 1
		{"s3", "nothing"},                // 4
		{"s2", "retry and retry, retry"}, // 5: 3 matches, older than 1
	}
	q := query(t, Spec{Patterns: []string{"retry"}})
	q.Sort, q.Limit = format.SortRelevance, 2
	_, r := collect(t, q, rows)
	if refs(r.Hits) != "1,5" || r.Hits[0].Copies != 1 || r.Total != 4 || r.Next != 2 {
		t.Fatalf("relevance page 1: %s copies %d total %d next %d", refs(r.Hits), r.Hits[0].Copies, r.Total, r.Next)
	}
	q.Offset = 2
	if _, r = collect(t, q, rows); refs(r.Hits) != "2,0" || r.Next != 0 {
		t.Fatalf("relevance page 2: %s next %d", refs(r.Hits), r.Next)
	}
	// -l with relevance: the sessions with the most matching messages
	// first.
	q = query(t, Spec{Patterns: []string{"retry"}})
	q.Sort, q.Mode = format.SortRelevance, format.ModeSessions
	_, r = collect(t, q, rows)
	var ss []string
	for _, s := range r.Sessions {
		ss = append(ss, fmt.Sprintf("%s:%d", s.Session, s.Hits))
	}
	if strings.Join(ss, " ") != "s1:2 s2:2 s3:1" {
		t.Fatalf("relevance sessions %v", ss)
	}
}
