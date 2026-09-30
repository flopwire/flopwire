package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// countSink counts rows without retaining them.
type countSink struct {
	convs    int
	parents  int
	links    map[string]int
	kinds    map[transcript.Kind]int
	enriched int
	loose    int
	fromComp int
	withID   int
	rows     int
}

func newCountSink() *countSink {
	return &countSink{links: map[string]int{}, kinds: map[transcript.Kind]int{}}
}

func (s *countSink) Conversation(c *transcript.Conversation) error {
	s.convs++
	return nil
}

func (s *countSink) Message(m *transcript.Message) error {
	s.rows++
	s.kinds[m.Kind]++
	if m.NativeID != "" {
		s.withID++
	}
	if m.Parser == EventsVersion {
		s.loose++
	} else if m.Enrichment["enrichment"] == EventsVersion {
		s.enriched++
	}
	if m.Enrichment["from_compaction"] == true {
		s.fromComp++
	}
	return nil
}

// TestCorpus parses every rollout under $CODEX_HOME (or ~/.codex) read-only.
// Run: FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/codex/
func TestCorpus(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1 to parse the real Codex corpus")
	}
	home := Home()
	srcs, err := Discover(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) == 0 {
		t.Skip("no rollouts under " + home)
	}
	var stats Stats
	var bytesRead atomic.Int64
	var mu sync.Mutex
	total := newCountSink()
	links := map[string]int{}
	parents := 0
	var failures []string
	byKey := map[string]int{}

	start := time.Now()
	work := make(chan transcript.Source)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &Parser{Stats: &stats}
			for src := range work {
				f, err := os.Open(src.Path)
				if err != nil {
					mu.Lock()
					failures = append(failures, src.Path+": "+err.Error())
					mu.Unlock()
					continue
				}
				fi, _ := f.Stat()
				sink := newCountSink()
				var convs []*transcript.Conversation
				cs := &convCapture{inner: sink, out: &convs}
				_, err = p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: fi.Size()}, transcript.Cursor{}, cs)
				f.Close()
				bytesRead.Add(fi.Size())
				mu.Lock()
				if err != nil {
					failures = append(failures, src.Path+": "+err.Error())
				}
				total.convs += sink.convs
				total.rows += sink.rows
				total.withID += sink.withID
				total.enriched += sink.enriched
				total.loose += sink.loose
				total.fromComp += sink.fromComp
				for k, v := range sink.kinds {
					total.kinds[k] += v
				}
				if n := len(convs); n > 0 {
					c := convs[n-1]
					if l, _ := c.Extra["link"].(string); l != "" {
						links[l]++
					} else {
						links["top"]++
					}
					if c.ParentSessionID != "" {
						parents++
					}
					byKey[c.SessionID]++
				}
				mu.Unlock()
			}
		}()
	}
	for _, s := range srcs {
		work <- s
	}
	close(work)
	wg.Wait()
	wall := time.Since(start)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	kinds := make([]string, 0, len(total.kinds))
	for k, v := range total.kinds {
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(kinds)
	paired, late, loose := stats.EventsPaired.Load(), stats.EventsLate.Load(), stats.EventsLoose.Load()
	rate := 0.0
	if paired+late+loose > 0 {
		rate = 100 * float64(paired+late) / float64(paired+late+loose)
	}
	dupSessions := 0
	for id, n := range byKey {
		if n > 1 {
			dupSessions++
			t.Logf("session id %s in %d files", id, n)
		}
	}
	t.Logf("files=%d bytes=%.2fGB wall=%s (%.0f MB/s) workers=%d", len(srcs), float64(bytesRead.Load())/1e9, wall.Round(time.Millisecond),
		float64(bytesRead.Load())/1e6/wall.Seconds(), runtime.GOMAXPROCS(0))
	t.Logf("conversations=%d rows=%d with_native_id=%d (%.1f%%)", len(byKey), total.rows, total.withID, 100*float64(total.withID)/float64(max(total.rows, 1)))
	t.Logf("rows by kind: %v", kinds)
	t.Logf("links: %v, with parent=%d, session ids in >1 file=%d", links, parents, dupSessions)
	t.Logf("codex-events@1: paired=%d late=%d loose=%d attach_rate=%.1f%% event_errors=%d calls_reemitted=%d enriched_rows=%d loose_rows=%d",
		paired, late, loose, rate, stats.EventErrors.Load(), stats.CallsReemitted.Load(), total.enriched, total.loose)
	t.Logf("lines=%d malformed=%d type_errors=%d too_large=%d inherited_skipped=%d fork_prefix_skipped=%d prompt_dupes=%d", stats.Lines.Load(), stats.Malformed.Load(), stats.TypeErrors.Load(), stats.TooLarge.Load(), stats.InheritedSkipped.Load(), stats.ForkPrefixSkipped.Load(), stats.PromptDupes.Load())
	t.Logf("compaction: summaries=%d replay_skipped=%d replay_indexed=%d", stats.CompactionSummaries.Load(), stats.CompactionReplaySkipped.Load(), stats.CompactionReplayIndexed.Load())
	t.Logf("go heap: sys=%dMB", ms.Sys>>20)
	if len(failures) > 0 {
		for _, f := range failures[:min(len(failures), 20)] {
			t.Error(f)
		}
		t.Fatalf("%d files failed", len(failures))
	}
	if total.rows == 0 || total.kinds[transcript.KindUser] == 0 {
		t.Fatal("no rows parsed")
	}
}

type convCapture struct {
	inner transcript.Sink
	out   *[]*transcript.Conversation
}

func (c *convCapture) Conversation(v *transcript.Conversation) error {
	*c.out = append(*c.out, v)
	return c.inner.Conversation(v)
}
func (c *convCapture) Message(m *transcript.Message) error { return c.inner.Message(m) }

// TestCorpusResume checks, on the largest real rollouts, that a parse
// resumed at arbitrary cut points emits exactly what one full parse emits.
func TestCorpusResume(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	srcs, err := Discover(Home())
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(srcs, func(i, j int) bool { return fileSize(srcs[i].Path) > fileSize(srcs[j].Path) })
	n := min(len(srcs), 3)
	for _, src := range srcs[:n] {
		t.Run(filepath.Base(src.Path), func(t *testing.T) {
			data, err := os.ReadFile(src.Path)
			if err != nil {
				t.Fatal(err)
			}
			cuts := []int64{int64(len(data)) / 7, int64(len(data)) / 3, int64(len(data)) * 5 / 7}
			assertResumeMatches(t, &src, data, cuts)
		})
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// digestSink records a digest of every emission in order.
type digestSink struct{ out []string }

func (d *digestSink) Conversation(c *transcript.Conversation) error {
	e, _ := json.Marshal(c.Extra)
	d.out = append(d.out, fmt.Sprintf("C %s %q %s %d %s", c.SessionID, c.Title, c.ParentSessionID, c.Depth, e))
	return nil
}

func (d *digestSink) Message(m *transcript.Message) error {
	e, _ := json.Marshal(m.Enrichment)
	d.out = append(d.out, fmt.Sprintf("M %s %s %d %d %s %s %s %v %x %d %s", m.SessionID, m.NativeID, m.Ordinal, m.LineNo, m.Kind, m.ToolName, m.ToolCallID, m.IsError, m.ContentSHA[:6], m.TS.UnixNano(), e))
	return nil
}

// assertResumeMatches parses data whole, then again in pieces ending at
// cuts (each moved to wherever it falls, mid-line included), and compares.
func assertResumeMatches(t *testing.T, src *transcript.Source, data []byte, cuts []int64) {
	t.Helper()
	ctx := context.Background()
	p := &Parser{}
	var full digestSink
	endFull, err := p.Parse(ctx, transcript.Input{Source: src, R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &full)
	if err != nil {
		t.Fatal(err)
	}
	var inc digestSink
	cur := transcript.Cursor{}
	for _, c := range append(cuts, int64(len(data))) {
		in := transcript.Input{Source: src, R: bytes.NewReader(data[:c]), Size: c}
		if cur, err = p.Parse(ctx, in, cur, &inc); err != nil {
			t.Fatal(err)
		}
	}
	if cur.Offset != endFull.Offset || cur.LineNo != endFull.LineNo || !bytes.Equal(cur.State, endFull.State) {
		t.Errorf("final cursor differs: inc (%d,%d,%dB) full (%d,%d,%dB)", cur.Offset, cur.LineNo, len(cur.State), endFull.Offset, endFull.LineNo, len(endFull.State))
	}
	if len(inc.out) != len(full.out) {
		t.Fatalf("incremental emitted %d, full %d", len(inc.out), len(full.out))
	}
	for i := range full.out {
		if inc.out[i] != full.out[i] {
			t.Fatalf("emission %d differs:\n inc  %s\n full %s", i, inc.out[i], full.out[i])
		}
	}
}
