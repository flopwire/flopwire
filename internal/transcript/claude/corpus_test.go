package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Real-corpus checks, read-only over this machine's Claude Code projects:
//
//	FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/claude
//
// TestCorpusParse measures a full parse (run it under /usr/bin/time -l for
// peak RSS). TestCorpusVerify re-reads every row's byte range and checks
// that the line's uuid prefixes the row's native id, and that resuming a
// sample of files mid-way matches a full parse.

func corpusSessions(t *testing.T) (string, []*Session) {
	t.Helper()
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to run over ~/.claude/projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := ProjectsRoot(os.Getenv, home)
	sessions, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, sessions
}

type countSink struct {
	kinds [16]atomic.Int64
	convs atomic.Int64
	check func(*transcript.Message) error
}

func (s *countSink) Conversation(*transcript.Conversation) error { s.convs.Add(1); return nil }
func (s *countSink) Message(m *transcript.Message) error {
	s.kinds[m.Kind].Add(1)
	if s.check != nil {
		return s.check(m)
	}
	return nil
}

// forEachSource runs fn over every transcript source with NumCPU workers.
func forEachSource(sessions []*Session, fn func(src transcript.Source) error) []error {
	ch := make(chan transcript.Source)
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for src := range ch {
				if err := fn(src); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for _, s := range sessions {
		for _, src := range s.Sources() {
			ch <- src
		}
	}
	close(ch)
	wg.Wait()
	return errs
}

func parseSource(p *Parser, src transcript.Source, sink transcript.Sink, cur transcript.Cursor, size int64) (transcript.Cursor, *os.File, error) {
	f, err := os.Open(src.Path)
	if err != nil {
		return cur, nil, err
	}
	if size < 0 {
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return cur, nil, err
		}
		size = st.Size()
	}
	next, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: size}, cur, sink)
	if err != nil {
		err = &os.PathError{Op: "parse", Path: src.Path, Err: err}
	}
	return next, f, err
}

func TestCorpusParse(t *testing.T) {
	root, sessions := corpusSessions(t)
	var files, subs, orphans, companions, indexedCompanions, bytesRead atomic.Int64
	for _, s := range sessions {
		if s.Orphaned() {
			orphans.Add(1)
		}
		subs.Add(int64(len(s.Subagents)))
		companions.Add(int64(len(s.Companions)))
		for _, c := range s.Companions {
			if c.Indexed() {
				indexedCompanions.Add(1)
			}
		}
	}
	stats := &Stats{}
	p := &Parser{Stats: stats}
	sink := &countSink{}
	var linked, unlinked atomic.Int64
	start := time.Now()
	errs := forEachSource(sessions, func(src transcript.Source) error {
		var c transcript.Collector
		conv := &convSink{inner: sink, last: &c}
		cur, f, err := parseSource(p, src, conv, transcript.Cursor{}, -1)
		if f != nil {
			f.Close()
		}
		files.Add(1)
		bytesRead.Add(cur.Offset)
		if n := len(c.Conversations); n > 0 && strings.HasPrefix(c.Conversations[n-1].SessionID, "agent-") {
			if c.Conversations[n-1].SpawnedByToolCallID != "" {
				linked.Add(1)
			} else {
				unlinked.Add(1)
			}
		}
		return err
	})
	wall := time.Since(start)
	for _, err := range errs {
		t.Error(err)
	}
	var total int64
	var byKind []string
	for k := transcript.KindUnknown; k <= transcript.KindAgentMessage; k++ {
		n := sink.kinds[k].Load()
		total += n
		if n > 0 {
			byKind = append(byKind, k.String()+"="+strconv.FormatInt(n, 10))
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("root %s", root)
	t.Logf("sessions %d (orphaned %d), transcript files %d (subagents %d), %.2f GB parsed",
		len(sessions), orphans.Load(), files.Load(), subs.Load(), float64(bytesRead.Load())/1e9)
	t.Logf("companions %d (indexed text %d)", companions.Load(), indexedCompanions.Load())
	t.Logf("rows %d: %s; conversation records %d", total, strings.Join(byKind, " "), sink.convs.Load())
	t.Logf("lines %d, malformed %d, oversized %d, too large %d, persisted filled %d, persisted missing %d",
		stats.Lines.Load(), stats.Malformed.Load(), stats.Oversized.Load(), stats.TooLarge.Load(), stats.Persisted.Load(), stats.PersistMiss.Load())
	t.Logf("subagents linked %d, unlinked %d", linked.Load(), unlinked.Load())
	t.Logf("wall %s (%d workers), %.0f MB/s, Go heap sys %d MB", wall.Round(time.Millisecond), runtime.NumCPU(),
		float64(bytesRead.Load())/1e6/wall.Seconds(), ms.HeapSys>>20)
	if len(errs) != 0 || total == 0 || sink.kinds[transcript.KindToolResult].Load() == 0 {
		t.Fatalf("errors %d, rows %d", len(errs), total)
	}
}

// convSink forwards messages and keeps only conversation records.
type convSink struct {
	inner transcript.Sink
	last  *transcript.Collector
}

func (s *convSink) Conversation(c *transcript.Conversation) error {
	s.last.Conversations = append(s.last.Conversations[:0], c)
	return s.inner.Conversation(c)
}
func (s *convSink) Message(m *transcript.Message) error { return s.inner.Message(m) }

type uuidMismatch struct{ path, native, got string }

func (e *uuidMismatch) Error() string {
	return e.path + ": row " + e.native + " re-reads to uuid " + e.got
}

func TestCorpusVerify(t *testing.T) {
	_, sessions := corpusSessions(t)
	p := &Parser{}
	var rows, sampled atomic.Int64
	errs := forEachSource(sessions, func(src transcript.Source) error {
		var f *os.File
		buf := []byte{}
		sink := &countSink{check: func(m *transcript.Message) error {
			rows.Add(1)
			if cap(buf) < int(m.ByteLen) {
				buf = make([]byte, m.ByteLen)
			}
			b := buf[:m.ByteLen]
			if _, err := f.ReadAt(b, m.ByteOffset); err != nil {
				return err
			}
			var rec struct {
				UUID string `json:"uuid"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(b), &rec); err != nil {
				return err
			}
			if id, _, _ := strings.Cut(m.NativeID, "#"); id != rec.UUID || rec.UUID == "" {
				return &uuidMismatch{src.Path, m.NativeID, rec.UUID}
			}
			return nil
		}}
		var err error
		f, err = os.Open(src.Path)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if _, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: st.Size()}, transcript.Cursor{}, sink); err != nil {
			return err
		}
		// Resume check on a sample: every 50th file by path hash.
		if h := fnv(src.Path); h%50 == 0 && st.Size() > 0 {
			sampled.Add(1)
			return resumeMatches(p, src, f, st.Size())
		}
		return nil
	})
	sort.Slice(errs, func(i, j int) bool { return errs[i].Error() < errs[j].Error() })
	for i, err := range errs {
		if i == 20 {
			t.Errorf("... %d more", len(errs)-20)
			break
		}
		t.Error(err)
	}
	t.Logf("verified %d rows; resume checked on %d files", rows.Load(), sampled.Load())
}

func resumeMatches(p *Parser, src transcript.Source, f *os.File, size int64) error {
	var full transcript.Collector
	if _, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: size}, transcript.Cursor{}, &full); err != nil {
		return err
	}
	var first, rest transcript.Collector
	cur, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: size / 2}, transcript.Cursor{}, &first)
	if err != nil {
		return err
	}
	if _, err := p.Parse(context.Background(), transcript.Input{Source: &src, R: f, Size: size}, cur, &rest); err != nil {
		return err
	}
	got := append(first.Messages, rest.Messages...)
	if !reflect.DeepEqual(got, full.Messages) {
		return &os.PathError{Op: "resume", Path: src.Path, Err: os.ErrInvalid}
	}
	return nil
}

func fnv(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
