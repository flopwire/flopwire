package regexq

import (
	"context"
	"crypto/sha256"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

// The alphabet mixes case, the two non-ASCII runes Go's (?i) folds onto
// ASCII letters (U+017F LONG S, U+212A KELVIN SIGN), other non-ASCII with
// and without case, line breaks, and regexp metacharacters.
var alphabet = []rune("abcsk ABSK\u017f\u212aé\u00c9日\n._-")

func randText(r *rand.Rand) string {
	n := r.IntN(48)
	b := make([]rune, n)
	for i := range b {
		// Bias toward a few letters so trigrams repeat across rows.
		if r.IntN(3) == 0 {
			b[i] = alphabet[r.IntN(len(alphabet))]
		} else {
			b[i] = []rune("abcsAB ")[r.IntN(7)]
		}
	}
	return string(b)
}

// randRegexp builds a random RE2 pattern over the alphabet.
func randRegexp(r *rand.Rand, depth int) string {
	lit := func() string {
		var b strings.Builder
		for range 1 + r.IntN(4) {
			c := alphabet[r.IntN(len(alphabet))]
			if c == '\n' {
				b.WriteString(`\n`)
				continue
			}
			b.WriteString(regexpQuote(c))
		}
		return b.String()
	}
	if depth <= 0 {
		return lit()
	}
	switch r.IntN(13) {
	case 0, 1, 2:
		return lit()
	case 3:
		return randRegexp(r, depth-1) + randRegexp(r, depth-1) + randRegexp(r, depth-1)
	case 4:
		return "(" + randRegexp(r, depth-1) + "|" + randRegexp(r, depth-1) + ")"
	case 5:
		return "(" + randRegexp(r, depth-1) + ")" + []string{"*", "+", "?", "{1,2}", "{2}", "{0,1}"}[r.IntN(6)]
	case 6:
		var b strings.Builder
		b.WriteString("[")
		if r.IntN(4) == 0 {
			b.WriteString("^")
		}
		for range 1 + r.IntN(3) {
			c := alphabet[r.IntN(len(alphabet))]
			if c == '\n' {
				b.WriteString(`\n`)
				continue
			}
			b.WriteString(regexpQuote(c))
		}
		return b.String() + "]"
	case 7:
		return []string{".", `\s`, `\w`, `\S`, "[a-c]", "[A-Z]"}[r.IntN(6)]
	case 8:
		return []string{"^", "$", `\b`, `\B`}[r.IntN(4)] + randRegexp(r, depth-1)
	case 9:
		return "(?i:" + randRegexp(r, depth-1) + ")"
	case 10:
		return randRegexp(r, depth-1) + ".*" + randRegexp(r, depth-1)
	default:
		return lit() + randRegexp(r, depth-1)
	}
}

func regexpQuote(c rune) string {
	if strings.ContainsRune(`\.+*?()|[]{}^$-`, c) {
		return `\` + string(c)
	}
	return string(c)
}

func buildStore(t testing.TB, tri localindex.Detail, texts []string) *localindex.Store {
	t.Helper()
	s, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{TriDetail: tri, RepoRoot: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	src, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "/p.jsonl", FileID: transcript.FileID{Dev: 1, Ino: 1},
		StorageKind: transcript.StorageJSONLAppend, Parser: "claude@1"})
	if err != nil {
		t.Fatal(err)
	}
	b := localindex.Batch{SourceID: src.ID, Generation: 1, Conversations: []*transcript.Conversation{{Agent: transcript.AgentClaude, SessionID: "s"}}}
	for i, text := range texts {
		m := &transcript.Message{SessionID: "s", NativeID: "m" + strconv.Itoa(i), Ordinal: int64(i), Kind: transcript.KindUser,
			TS: time.Unix(int64(i), 0), LineNo: int64(i + 1), ByteOffset: int64(i) * 100, ByteLen: 100, Parser: "claude@1", Text: text, FullLen: len(text)}
		m.ContentSHA = sha256.Sum256([]byte(text))
		b.Messages = append(b.Messages, m)
	}
	if _, err := s.ApplyBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPlannerMatchesBruteForce: for random regexes over a random corpus,
// trigram-planned find returns exactly the rows a full regexp scan returns,
// at both trigram detail levels and in both case modes.
func TestPlannerMatchesBruteForce(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if v, err := strconv.ParseUint(os.Getenv("REGEXQ_SEED"), 10, 64); err == nil {
		seed = v
	}
	n := 400
	if testing.Short() || raceEnabled {
		n = 80
	}
	t.Logf("seed %d (REGEXQ_SEED to replay)", seed)
	r := rand.New(rand.NewPCG(seed, 7))
	texts := make([]string, 600)
	for i := range texts {
		texts[i] = randText(r)
	}
	ctx := context.Background()
	for _, tri := range []localindex.Detail{localindex.DetailFull, localindex.DetailColumn} {
		s := buildStore(t, tri, texts)
		all := allRows(t, s)
		indexed := 0
		for i := range n {
			pat := randRegexp(r, 3)
			cs := i%2 == 0
			p, err := Compile(pat, cs)
			if err != nil {
				continue
			}
			// Identical texts collapse into the newest row (decision D6).
			var want []int64
			newest := map[string]int64{}
			matched := 0
			for _, row := range all {
				if p.Re.MatchString(row.Text) {
					matched++
					newest[row.Text] = max(newest[row.Text], row.ID)
				}
			}
			for _, id := range newest {
				want = append(want, id)
			}
			slices.Sort(want)
			got, st, err := Find(ctx, s, p, Options{Limit: 1 << 20, MaxScan: 1 << 20, MaxCandidates: 1 << 20})
			if err != nil {
				t.Fatalf("%s %q: %v", tri, pat, err)
			}
			if !st.Unindexed {
				indexed++
			}
			ids := make([]int64, len(got))
			copies := 0
			for j, m := range got {
				ids[j] = m.ID
				copies += 1 + m.Copies
			}
			slices.Sort(ids)
			if !slices.Equal(ids, want) || copies != matched {
				t.Fatalf("detail=%s case_sensitive=%v pattern %q plan %s expr %q:\n got %v\nwant %v", tri, cs, pat, p.Query, st.Expr, ids, want)
			}
		}
		if indexed < n/4 {
			t.Errorf("only %d of %d random patterns used the index", indexed, n)
		}
		t.Logf("detail=%s: %d patterns, %d indexed", tri, n, indexed)
	}
}

func allRows(t testing.TB, s *localindex.Store) []*localindex.Row {
	rows, err := s.DB().Query(`SELECT id FROM messages ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out, err := s.Messages(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFindNotes: unindexed patterns scan with a bound and say so.
func TestFindNotes(t *testing.T) {
	texts := []string{"alpha beta", "gamma", "alphabet soup", "x"}
	s := buildStore(t, localindex.DetailFull, texts)
	ctx := context.Background()
	p, _ := Compile(`alpha.?bet`, false)
	got, st, err := Find(ctx, s, p, Options{})
	if err != nil || len(got) != 2 || st.Unindexed || st.Note(Options{}) != "" {
		t.Fatalf("indexed: %d hits %+v %v", len(got), st, err)
	}
	if got[0].Line != 1 || got[0].LineText != "alphabet soup" {
		t.Fatalf("line %+v", got[0])
	}
	p, _ = Compile(`a.b`, false)
	got, st, err = Find(ctx, s, p, Options{MaxScan: 2})
	if err != nil || !st.Unindexed || !st.Truncated || !strings.HasPrefix(st.Note(Options{MaxScan: 2}), "unindexed") {
		t.Fatalf("unindexed: %d %+v %v", len(got), st, err)
	}
	p, _ = Compile(`[^\s\S]`, false)
	if got, _, err = Find(ctx, s, p, Options{}); err != nil || len(got) != 0 {
		t.Fatalf("no-match plan: %v %v", got, err)
	}
	if expr, all := p.MatchExpr(); expr != "" || all {
		t.Fatal("none plan expr")
	}
	p, _ = Compile(`say "hi"`, true)
	if expr, _ := p.MatchExpr(); !strings.Contains(expr, `"hi"""`) {
		t.Fatalf("quote escaping: %s", expr)
	}
}
