package localindex

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// TestCorpus indexes the real Claude Code, Codex and Devin transcripts on
// this machine, read-only, through the real parsers (NumCPU parse workers
// feeding the single writer), and reports counts, time, size and query
// latency. Devin's sessions.db is copied to a temp dir first.
//
//	FLOPWIRE_CORPUS=1 [FLOPWIRE_CORPUS_DB=/path/index.db] [FLOPWIRE_CORPUS_MAX_FILES=n] \
//	  go test -run TestCorpus -timeout 3h -v ./internal/localindex/
//
// FLOPWIRE_CORPUS_MAX_FILES samples JSONL files evenly across both harnesses.
func TestCorpus(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	home, _ := os.UserHomeDir()
	dbPath := os.Getenv("FLOPWIRE_CORPUS_DB")
	if dbPath == "" {
		dbPath = filepath.Join(t.TempDir(), "corpus.db")
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		os.Remove(p)
	}
	maxFiles, _ := strconv.Atoi(os.Getenv("FLOPWIRE_CORPUS_MAX_FILES"))
	s, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	type job struct {
		p   transcript.Parser
		src transcript.Source
	}
	cp, xp := &claude.Parser{}, &codex.Parser{}
	var jobs []job
	sessions, err := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	if err != nil {
		t.Fatal(err)
	}
	var stubs []*transcript.Conversation
	for _, sess := range sessions {
		if c := sess.Stub(); c != nil {
			stubs = append(stubs, c)
		}
		for _, src := range sess.Sources() {
			jobs = append(jobs, job{cp, src})
		}
	}
	xsrcs, err := codex.Discover(codex.Home())
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range xsrcs {
		jobs = append(jobs, job{xp, src})
	}
	if maxFiles > 0 && len(jobs) > maxFiles {
		step := float64(len(jobs)) / float64(maxFiles)
		var sample []job
		for i := 0.0; int(i) < len(jobs); i += step {
			sample = append(sample, jobs[int(i)])
		}
		jobs = sample
	}
	if db := devin.DefaultPath(home); fileExists(db) {
		copied := copyDevinStore(t, db, t.TempDir())
		jobs = append(jobs, job{&devin.Parser{}, transcript.Source{Agent: transcript.AgentDevin, Path: copied,
			StorageKind: transcript.StorageSQLite, Parser: devin.Name}})
	}

	start, cpu0 := time.Now(), cpuTime()
	var mu sync.Mutex
	var total BatchResult
	var rawBytes int64
	var failures []error
	ch := make(chan job)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ch {
				res, n, err := indexSource(s, j.p, j.src)
				mu.Lock()
				if err != nil {
					failures = append(failures, err)
				}
				rawBytes += n
				total.Inserted += res.Inserted
				total.Grown += res.Grown
				total.Touched += res.Touched
				total.Versioned += res.Versioned
				total.Conversations += res.Conversations
				mu.Unlock()
			}
		}()
	}
	for _, j := range jobs {
		ch <- j
	}
	close(ch)
	wg.Wait()
	for _, err := range failures {
		t.Error(err)
	}
	if len(stubs) > 0 {
		st, err := s.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: "(orphans)", StorageKind: transcript.StorageJSONLAppend, Parser: claude.ParserName})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyBatch(ctx, Batch{SourceID: st.ID, Generation: 1, Conversations: stubs}); err != nil {
			t.Fatal(err)
		}
	}
	wall, cpu := time.Since(start), cpuTime()-cpu0
	if _, err := s.wdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dbPath)
	var msgs, convs, subs, linked, textBytes int64
	s.rdb.QueryRow(`SELECT count(*), coalesce(sum(text_len), 0) FROM messages`).Scan(&msgs, &textBytes)
	s.rdb.QueryRow(`SELECT count(*), count(parent_session_id), count(parent_conversation_id) FROM conversations`).Scan(&convs, &subs, &linked)
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	t.Logf("sources=%d raw=%.2fGB messages=%d conversations=%d (subagents %d, parent resolved %d) capped text=%.0fMB",
		len(jobs), float64(rawBytes)/1e9, msgs, convs, subs, linked, float64(textBytes)/1e6)
	t.Logf("apply %+v", total)
	t.Logf("index wall=%s cpu=%s (%.0f rows/s wall) db=%.0fMB maxrss=%.0fMB",
		wall.Round(time.Second), cpu.Round(time.Second), float64(msgs)/wall.Seconds(), float64(fi.Size())/1e6, float64(ru.Maxrss)/1e6)
	s.rdb.QueryRow(`SELECT count(*) FROM messages WHERE superseded = 1`).Scan(&msgs)
	t.Logf("superseded rows=%d", msgs)
	if total.Inserted == 0 {
		t.Fatal("no messages indexed")
	}
	if subs > 0 && linked == 0 {
		t.Error("no subagent parent link resolved")
	}
	s.rdb.QueryRow(`SELECT count(*) FROM messages`).Scan(&msgs)
	reportSizes(t, s, 1e6/float64(msgs))

	for _, q := range []string{"error", "internal/transcript", "sqlite fts5 trigram", "go test -race", "worktree", "cass search", "devin session", "--no-verify"} {
		lat := timeIt(3, func() error { _, err := s.Search(ctx, q, SearchOptions{}); return err })
		hits, _ := s.Search(ctx, q, SearchOptions{})
		t.Logf("search %-26q hits=%-3d warm=%s", q, len(hits), lat)
	}
	for _, q := range []string{"internal/transcript/model.go", "TODO", "func main()", "go test -race ./...", "ENOENT", "gh pr create", "no such file or directory", "zq", "::"} {
		lat := timeIt(3, func() error {
			_, err := s.Find(ctx, q, FindOptions{})
			if errors.Is(err, ErrScanLimit) {
				err = nil
			}
			return err
		})
		hits, _ := s.Find(ctx, q, FindOptions{})
		t.Logf("find   %-30q hits=%-3d warm=%s", q, len(hits), lat)
	}
}

// indexSource parses one source from the start into the store, saving the
// watermark with the last batch.
func indexSource(s *Store, p transcript.Parser, src transcript.Source) (BatchResult, int64, error) {
	in := transcript.Input{Source: &src}
	sampled := time.Now()
	var id transcript.Identity
	if src.StorageKind != transcript.StorageSQLite {
		f, err := os.Open(src.Path)
		if err != nil {
			return BatchResult{}, 0, nil // vanished since discovery
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return BatchResult{}, 0, err
		}
		id = transcript.IdentityOf(fi)
		in.R, in.Size = f, fi.Size()
	} else if st, err := transcript.StatIdentity(src.Path); err == nil {
		id = st
	}
	src.FileID = id.ID
	st, err := s.EnsureSource(ctx, src)
	if err != nil {
		return BatchResult{}, 0, err
	}
	gen := transcript.Generation{Generation: 1, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: time.Now(), Complete: true}
	if err := s.StartGeneration(ctx, st.ID, gen, "initial"); err != nil {
		return BatchResult{}, 0, err
	}
	sink := s.NewSink(ctx, st.ID, 1)
	cur, err := p.Parse(ctx, in, transcript.Cursor{}, sink)
	if err != nil {
		return sink.Result, 0, err
	}
	wm := transcript.Watermark{Identity: id, SampledAt: sampled.UnixNano(), Offset: cur.Offset, LineNo: cur.LineNo}
	if in.R != nil {
		if wm, err = transcript.NewWatermark(in.R, id, sampled, cur); err != nil {
			return sink.Result, 0, err
		}
	}
	return sink.Result, id.Size, sink.Flush(&wm, cur.State)
}

func timeIt(n int, fn func() error) time.Duration {
	var best time.Duration
	for i := range n {
		t0 := time.Now()
		if err := fn(); err != nil {
			return -1
		}
		if d := time.Since(t0); i == 0 || d < best {
			best = d
		}
	}
	return best.Round(time.Microsecond)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// copyDevinStore copies sessions.db and its -wal/-shm into dir; the parser
// then opens the copy read-only.
func copyDevinStore(t *testing.T, src, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, "sessions.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	return dst
}
