package localindex

import (
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// TestScale indexes a synthetic corpus and reports insert rate, size per
// million rows (per table, both FTS tables) and query latency.
//
//	FLOPWIRE_SCALE=1 [FLOPWIRE_SCALE_ROWS=1000000] [FLOPWIRE_SCALE_DETAIL=column/full,full/full,column/column] \
//	  [FLOPWIRE_SCALE_DIR=/some/dir] go test -run TestScale -timeout 2h -v ./internal/localindex/
func TestScale(t *testing.T) {
	if os.Getenv("FLOPWIRE_SCALE") == "" {
		t.Skip("set FLOPWIRE_SCALE=1")
	}
	rows := 1_000_000
	if v, err := strconv.Atoi(os.Getenv("FLOPWIRE_SCALE_ROWS")); err == nil && v > 0 {
		rows = v
	}
	// Each config is tok/tri detail.
	configs := os.Getenv("FLOPWIRE_SCALE_DETAIL")
	if configs == "" {
		configs = "column/full,full/full,column/column"
	}
	dir := os.Getenv("FLOPWIRE_SCALE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	for _, c := range strings.Split(configs, ",") {
		tok, tri, _ := strings.Cut(c, "/")
		d := Details{Tok: Detail(tok), Tri: Detail(tri)}
		t.Run("tok="+tok+",tri="+tri, func(t *testing.T) {
			runScale(t, filepath.Join(dir, "scale-"+tok+"-"+tri+".db"), d, rows)
		})
	}
}

// gen produces a deterministic transcript-like corpus: Zipf-distributed
// prose words, code identifiers, file paths with line numbers, flags, and
// kind-dependent lengths (tool results long, prompts short).
type gen struct {
	r      *rand.Rand
	words  []string
	idents []string
	paths  []string
	zw, zi *rand.Zipf
}

func newGen(seed uint64) *gen {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	g := &gen{r: r}
	syl := []string{"ka", "lo", "mi", "ne", "ru", "sa", "to", "vi", "po", "de", "an", "er", "in", "on", "st", "tr", "ch", "ex", "al", "or"}
	seen := map[string]bool{}
	for len(g.words) < 40000 {
		var b strings.Builder
		for range 1 + r.IntN(4) {
			b.WriteString(syl[r.IntN(len(syl))])
		}
		if w := b.String(); !seen[w] {
			seen[w] = true
			g.words = append(g.words, w)
		}
	}
	for range 20000 {
		a, b := g.words[r.IntN(5000)], g.words[r.IntN(20000)]
		g.idents = append(g.idents, a+strings.ToUpper(b[:1])+b[1:])
	}
	dirs := []string{"internal", "cmd", "pkg", "web/src", "scripts", "tools", "notes"}
	for range 5000 {
		g.paths = append(g.paths, dirs[r.IntN(len(dirs))]+"/"+g.words[r.IntN(3000)]+"/"+g.words[r.IntN(10000)]+[]string{".go", ".ts", ".md", "_test.go", ".sql"}[r.IntN(5)])
	}
	g.zw = rand.NewZipf(r, 1.07, 2, uint64(len(g.words)-1))
	g.zi = rand.NewZipf(r, 1.2, 2, uint64(len(g.idents)-1))
	return g
}

func (g *gen) text(kind transcript.Kind) string {
	var n int
	switch kind {
	case transcript.KindUser:
		n = 5 + g.r.IntN(55)
	case transcript.KindAssistant:
		n = 20 + g.r.IntN(180)
	case transcript.KindToolCall:
		n = 5 + g.r.IntN(45)
	case transcript.KindToolResult:
		n = 20 + g.r.IntN(430) // up to ~3.5KB, under the tool cap
	default:
		n = 30 + g.r.IntN(170)
	}
	var b strings.Builder
	for i := range n {
		if i > 0 {
			if g.r.IntN(14) == 0 {
				b.WriteString(".\n")
			} else {
				b.WriteByte(' ')
			}
		}
		switch x := g.r.IntN(100); {
		case x < 70:
			b.WriteString(g.words[g.zw.Uint64()])
		case x < 85:
			b.WriteString(g.idents[g.zi.Uint64()])
		case x < 93:
			fmt.Fprintf(&b, "%s:%d", g.paths[g.r.IntN(len(g.paths))], 1+g.r.IntN(400))
		case x < 97:
			b.WriteString("--" + g.words[g.r.IntN(200)])
		default:
			fmt.Fprintf(&b, "0x%08x", g.r.Uint32())
		}
	}
	return b.String()
}

// Kind mix and lengths aim at the spec §4.2 corpus: ~1KB of extracted text
// per row on average (1.62M rows, 0.5-0.7GB compressed).
var scaleKinds = []transcript.Kind{
	transcript.KindUser, transcript.KindAssistant, transcript.KindAssistant, transcript.KindAssistant,
	transcript.KindToolCall, transcript.KindToolCall, transcript.KindToolResult, transcript.KindToolResult,
	transcript.KindToolResult, transcript.KindThinking,
}

func cpuTime() time.Duration {
	var r syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}

func runScale(t *testing.T, path string, detail Details, total int) {
	os.Remove(path)
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
	s, err := Open(path, Options{TokDetail: detail.Tok, TriDetail: detail.Tri, RepoRoot: func(string) string { return "/repo" }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g := newGen(1)
	perConv, perBatch := 5000, 5000
	if v, err := strconv.Atoi(os.Getenv("FLOPWIRE_SCALE_BATCH")); err == nil && v > 0 {
		perBatch = v
	}
	var textBytes int64
	var applyDur time.Duration
	start, cpu0 := time.Now(), cpuTime()
	var src SourceState
	var batch []*transcript.Message
	var convs []*transcript.Conversation
	agents := []transcript.Agent{transcript.AgentClaude, transcript.AgentCodex, transcript.AgentDevin}
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ta := time.Now()
		if _, err := s.ApplyBatch(ctx, Batch{SourceID: src.ID, Generation: 1, Conversations: convs, Messages: batch}); err != nil {
			t.Fatal(err)
		}
		applyDur += time.Since(ta)
		batch, convs = batch[:0], nil
	}
	for i := range total {
		conv := i / perConv
		session := fmt.Sprintf("sess-%06d", conv)
		if i%perConv == 0 {
			flush()
			a := agents[conv%len(agents)]
			if src, err = s.EnsureSource(ctx, transcript.Source{Agent: a, Path: "/h/" + session + ".jsonl", FileID: transcript.FileID{Dev: 1, Ino: uint64(conv + 1)}, StorageKind: transcript.StorageJSONLAppend, Parser: string(a) + "@1"}); err != nil {
				t.Fatal(err)
			}
			convs = []*transcript.Conversation{{Agent: a, SessionID: session, Cwd: "/repo/x", Depth: min(conv%7, 1)}}
		}
		k := scaleKinds[g.r.IntN(len(scaleKinds))]
		text := g.text(k)
		textBytes += int64(len(text))
		off := int64(i%perConv) * 2000
		m := &transcript.Message{SessionID: session, NativeID: fmt.Sprintf("%s-%d", session, i), Ordinal: transcript.OrdinalAt(off, 0), Kind: k,
			TS: time.Unix(1_750_000_000+int64(i), 0), LineNo: int64(i%perConv) + 1, ByteOffset: off, ByteLen: int64(len(text)), Parser: "x@1",
			Text: text, FullLen: len(text), ContentSHA: sha256.Sum256([]byte(text))}
		batch = append(batch, m)
		if len(batch) >= perBatch {
			flush()
		}
		if (i+1)%200_000 == 0 {
			t.Logf("  %d rows, %.0f rows/s", i+1, float64(i+1)/time.Since(start).Seconds())
		}
	}
	flush()
	insert, insertCPU := time.Since(start), cpuTime()-cpu0
	t.Logf("wall in ApplyBatch %s, process CPU %s (includes the generator)", applyDur.Round(time.Second), insertCPU.Round(time.Second))
	if _, err := s.wdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	per := float64(1_000_000) / float64(total)
	t.Logf("detail=%+v rows=%d text=%.0fMB insert=%s (%.0f rows/s) file=%.0fMB (%.0fMB per 1M rows)",
		detail, total, float64(textBytes)/1e6, insert.Round(time.Second), float64(total)/insert.Seconds(), float64(fi.Size())/1e6, float64(fi.Size())/1e6*per)
	reportSizes(t, s, per)

	// Queries: search terms sampled across frequency bands; find
	// substrings cut from stored rows (so they hit) plus path:line forms.
	qg := newGen(2)
	var searches, finds []string
	for i := range 200 {
		switch i % 4 {
		case 0:
			searches = append(searches, qg.words[qg.r.IntN(50)]) // very common
		case 1:
			searches = append(searches, qg.words[500+qg.r.IntN(5000)])
		case 2:
			searches = append(searches, qg.words[qg.r.IntN(2000)]+" "+qg.words[qg.r.IntN(2000)])
		default:
			searches = append(searches, qg.paths[qg.r.IntN(len(qg.paths))])
		}
	}
	for i := range 200 {
		switch i % 4 {
		case 0:
			finds = append(finds, fmt.Sprintf("%s:%d", qg.paths[qg.r.IntN(len(qg.paths))], 1+qg.r.IntN(400)))
		case 1:
			finds = append(finds, qg.idents[qg.r.IntN(len(qg.idents))])
		case 2:
			finds = append(finds, strings.ToUpper(qg.words[qg.r.IntN(len(qg.words))])+" "+qg.words[qg.r.IntN(len(qg.words))])
		default:
			id := 1 + qg.r.Int64N(int64(total))
			rows, err := s.Messages(ctx, []int64{id})
			if err == nil && len(rows) == 1 && len(rows[0].Text) > 40 {
				o := qg.r.IntN(len(rows[0].Text) - 30)
				finds = append(finds, rows[0].Text[o:o+8+qg.r.IntN(20)])
			} else {
				finds = append(finds, "internal/")
			}
		}
	}
	measure := func(name string, qs []string, fn func(q string) (int, error)) {
		var lat []time.Duration
		hits := 0
		for _, q := range qs {
			t0 := time.Now()
			n, err := fn(q)
			if err != nil {
				t.Fatalf("%s %q: %v", name, q, err)
			}
			lat = append(lat, time.Since(t0))
			if n > 0 {
				hits++
			}
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		t.Logf("%-28s n=%d with-hits=%d p50=%s p90=%s p99=%s max=%s", name, len(qs), hits,
			lat[len(lat)/2].Round(time.Microsecond), lat[len(lat)*9/10].Round(time.Microsecond), lat[len(lat)*99/100].Round(time.Microsecond), lat[len(lat)-1].Round(time.Microsecond))
	}
	measure("search limit=20", searches, func(q string) (int, error) {
		h, err := s.Search(ctx, q, SearchOptions{})
		return len(h), err
	})
	measure("search limit=20 subagents-off", searches, func(q string) (int, error) {
		h, err := s.Search(ctx, q, SearchOptions{Filter: Filter{ExcludeSubagents: true, Kinds: []transcript.Kind{transcript.KindUser, transcript.KindAssistant}}})
		return len(h), err
	})
	measure("find limit=50", finds, func(q string) (int, error) {
		h, err := s.Find(ctx, q, FindOptions{})
		return len(h), err
	})
	measure("find case-sensitive", finds, func(q string) (int, error) {
		h, err := s.Find(ctx, q, FindOptions{CaseSensitive: true})
		return len(h), err
	})
	measure("find short (2 chars)", []string{"xq", "zz", "::", "ka", "0x"}, func(q string) (int, error) {
		h, err := s.Find(ctx, q, FindOptions{})
		if err == ErrScanLimit {
			err = nil
		}
		return len(h), err
	})
}

func reportSizes(t *testing.T, s *Store, per float64) {
	rows, err := s.wdb.Query(`SELECT CASE
		  WHEN name LIKE 'fts_tok%' THEN 'fts_tok'
		  WHEN name LIKE 'fts_tri%' THEN 'fts_tri'
		  WHEN name = 'messages' THEN 'messages (table, incl. zstd text)'
		  WHEN name LIKE 'messages_%' THEN 'messages indexes'
		  ELSE 'other' END AS grp, sum(pgsize) FROM dbstat GROUP BY grp ORDER BY 2 DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var size int64
		rows.Scan(&name, &size)
		t.Logf("  %-36s %8.0fMB per 1M rows", name, float64(size)/1e6*per)
	}
	// The FTS tables live in shard files (fts.go).
	for _, sh := range s.shards {
		if fi, err := os.Stat(sh.path); err == nil {
			t.Logf("  %-36s %8.0fMB per 1M rows", sh.schema+" ("+sh.table+" file)", float64(fi.Size())/1e6*per)
		}
	}
	var textZ int64
	s.wdb.QueryRow(`SELECT sum(length(text)) FROM messages`).Scan(&textZ)
	t.Logf("  %-36s %8.0fMB per 1M rows", "  of which zstd text", float64(textZ)/1e6*per)
}
