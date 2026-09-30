package devicesync

import (
	"bufio"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// FLOPWIRE_CORPUS=1 go test -run Corpus ./internal/devicesync
//
// Syncs the real ~/.claude/projects and ~/.codex transcript corpus,
// read-only, into the in-memory server keeping only chunk hashes and sizes.

func corpusRoots(t *testing.T) []string {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to run against the real corpus")
	}
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".codex", "archived_sessions"),
	}
}

// corpusSpec classifies a corpus file the way the device agent will.
func corpusSpec(path string) SourceSpec {
	agent := transcript.AgentClaude
	if strings.Contains(path, "/.codex/") {
		agent = transcript.AgentCodex
	}
	sp := SourceSpec{Path: path, Agent: agent, StorageKind: transcript.StorageJSONLAppend, Parser: string(agent) + "@1"}
	switch {
	case strings.Contains(path, "/tool-results/"):
		// <project>/<session>/tool-results/<file> belongs to <project>/<session>.jsonl.
		sess := filepath.Dir(filepath.Dir(path))
		sp.StorageKind, sp.Parent, sp.Parser = transcript.StorageDir, sess+".jsonl", ""
	case strings.HasSuffix(path, ".meta.json"):
		sp.StorageKind, sp.Parent, sp.Parser = transcript.StorageJSONDoc, strings.TrimSuffix(path, ".meta.json")+".jsonl", ""
	case strings.HasSuffix(path, ".json"):
		sp.StorageKind = transcript.StorageJSONDoc
	}
	return sp
}

func TestCorpusDryRun(t *testing.T) {
	roots := corpusRoots(t)
	e := newEnv(t, Config{Chunk: DefaultChunkParams}, 1<<30)
	e.srv.Discard = true
	ctx := context.Background()
	start := time.Now()
	var files, errs int
	var total int64
	kinds := map[transcript.StorageKind]int{}
	for _, root := range roots {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			sp := corpusSpec(path)
			if err := e.sy.Sync(ctx, sp); err != nil {
				errs++
				t.Errorf("%s: %v", path, err)
				return nil
			}
			files++
			total += fi.Size()
			kinds[sp.StorageKind]++
			return nil
		})
	}
	wall := time.Since(start)
	var refs, refBytes int64
	e.store.db.QueryRow(`SELECT count(*), coalesce(sum(size), 0) FROM devsync_manifest`).Scan(&refs, &refBytes)
	uniq, uniqBytes := e.srv.UniqueChunks()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("files %d (%v), %d errors, %.2f GB", files, kinds, errs, float64(total)/1e9)
	t.Logf("finalized chunk refs %d (%.2f GB); unique chunks %d (%.2f GB); dedupe ratio %.3f (%.1f%% saved); tails %.1f MB",
		refs, float64(refBytes)/1e9, uniq, float64(uniqBytes)/1e9, float64(refBytes)/float64(uniqBytes),
		100*(1-float64(uniqBytes)/float64(refBytes)), float64(e.srv.TailBytes)/1e6)
	t.Logf("chunk bodies sent %.1f MB uncompressed, %.1f MB on the wire (ratio %.2f)",
		float64(e.srv.BodyBytes)/1e6, float64(e.srv.WireBytes)/1e6, float64(e.srv.BodyBytes)/float64(max(e.srv.WireBytes, 1)))
	t.Logf("wall %v (%.0f MB/s); %d flush requests; go heap sys %d MB; spool %d bytes",
		wall.Round(time.Millisecond), float64(total)/1e6/wall.Seconds(), e.srv.FlushRequests, ms.Sys>>20, e.spool.Used())
	if files < 1000 || errs > 0 {
		t.Fatalf("files %d errors %d", files, errs)
	}
	if specs, _ := e.store.PendingSpecs(ctx); len(specs) > 20 { // live sessions may grow mid-run
		t.Fatalf("%d sources still pending", len(specs))
	}
}

// Replays a busy Codex rollout line by line at its recorded timestamps
// through the append-only flush cadence (300ms trailing debounce, 2s max
// wait) and measures what each flush uploads: newly finalized chunks plus
// the provisional tail. FLOPWIRE_CORPUS_REPLAY picks the file; default is the
// largest rollout under ~/.codex/sessions.
func TestCorpusCodexReplay(t *testing.T) {
	corpusRoots(t)
	path := os.Getenv("FLOPWIRE_CORPUS_REPLAY")
	if path == "" {
		home, _ := os.UserHomeDir()
		var best int64
		filepath.WalkDir(filepath.Join(home, ".codex", "sessions"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && strings.HasSuffix(p, ".jsonl") {
				if fi, err := d.Info(); err == nil && fi.Size() > best {
					best, path = fi.Size(), p
				}
			}
			return nil
		})
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Line end offsets and timestamps.
	var ends []int64
	var ts []time.Time
	br := bufio.NewReaderSize(f, 1<<20)
	var off int64
	for {
		line, err := br.ReadSlice('\n')
		head := append([]byte(nil), line[:min(len(line), 4096)]...) // timestamp comes first
		for err == bufio.ErrBufferFull {
			off += int64(len(line))
			line, err = br.ReadSlice('\n')
		}
		if len(line) == 0 || line[len(line)-1] != '\n' {
			break
		}
		off += int64(len(line))
		var rec struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if i := strings.Index(string(head), `"timestamp":"`); i >= 0 {
			rec.Timestamp, _ = time.Parse(time.RFC3339Nano, strings.SplitN(string(head[i+13:]), `"`, 2)[0])
		}
		if rec.Timestamp.IsZero() && len(ts) > 0 {
			rec.Timestamp = ts[len(ts)-1]
		}
		ends = append(ends, off)
		ts = append(ts, rec.Timestamp)
	}
	cad := Cadence{300 * time.Millisecond, 2 * time.Second}
	var flushAt []int // index of the last line in each flush
	for i := 0; i < len(ts); {
		first, last, j := ts[i], ts[i], i
		for j+1 < len(ts) {
			deadline := min(last.Add(cad.Debounce).UnixNano(), first.Add(cad.MaxWait).UnixNano())
			if ts[j+1].UnixNano() > deadline {
				break
			}
			j++
			last = ts[j]
		}
		flushAt = append(flushAt, j)
		i = j + 1
	}
	buf := make([]byte, DefaultChunkParams.Max)
	var sizes []int64
	var boundary, uploaded, finalized, srvTailEnd, wholeTails int64
	start := time.Now()
	for _, j := range flushAt {
		size := ends[j]
		var fresh int64
		tail, err := Scan(DefaultChunkParams, f, boundary, size, buf, func(c Chunk, _ []byte) error {
			fresh += c.Size
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// The tail goes as a delta when the server holds its start.
		tailSent := size - tail
		if tail == boundary {
			tailSent = size - max(tail, srvTailEnd)
		}
		boundary, srvTailEnd = tail, size
		finalized += fresh
		wholeTails += size - tail
		up := fresh + tailSent
		uploaded += up
		sizes = append(sizes, up)
	}
	slices.Sort(sizes)
	pct := func(p float64) int64 { return sizes[int(p*float64(len(sizes)-1))] }
	span := ts[len(ts)-1].Sub(ts[0])
	t.Logf("replay %s: %d lines, %.1f MB, %v of activity, %d flushes (%.1f lines/flush)",
		filepath.Base(path), len(ends), float64(ends[len(ends)-1])/1e6, span.Round(time.Second), len(sizes), float64(len(ends))/float64(len(sizes)))
	t.Logf("per-flush upload bytes: p50 %d, p90 %d, p99 %d, max %d; total uploaded %.1f MB for %.1f MB of file (%.2fx; %.1fx if whole tails were re-sent); chunking wall %v",
		pct(.5), pct(.9), pct(.99), sizes[len(sizes)-1], float64(uploaded)/1e6, float64(ends[len(ends)-1])/1e6,
		float64(uploaded)/float64(ends[len(ends)-1]), float64(finalized+wholeTails)/float64(ends[len(ends)-1]), time.Since(start).Round(time.Millisecond))
	if finalized > ends[len(ends)-1] || len(sizes) == 0 {
		t.Fatal("inconsistent replay")
	}

	// The same flush points through the real Syncer, HTTP and fake server:
	// append the lines to a temp copy (under $TMPDIR) and Sync at each point.
	e := newEnv(t, Config{Chunk: DefaultChunkParams}, 1<<20)
	e.srv.Discard = true
	sp := e.spec("replay.jsonl", transcript.StorageJSONLAppend)
	out, err := os.Create(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var copied int64
	start = time.Now()
	for _, j := range flushAt {
		if _, err := out.Write(readRange(t, f, copied, ends[j])); err != nil {
			t.Fatal(err)
		}
		copied = ends[j]
		e.sync(sp)
	}
	frames := slices.Clone(e.srv.FrameBytes)
	slices.Sort(frames)
	fp := func(p float64) int64 { return frames[int(p*float64(len(frames)-1))] }
	var sent int64
	for _, n := range frames {
		sent += n
	}
	t.Logf("syncer replay: %d requests; payload p50 %d, p90 %d, p99 %d, max %d; total %.1f MB (%.2fx); wall %v",
		len(frames), fp(.5), fp(.9), fp(.99), frames[len(frames)-1], float64(sent)/1e6,
		float64(sent)/float64(copied), time.Since(start).Round(time.Millisecond))
	entries, tail := e.srv.Manifest(sp.Path, fileIDOf(t, sp.Path), 0)
	var total int64
	for _, en := range entries {
		total += en.Size
	}
	if tail != nil {
		total += tail.Size
	}
	if total != copied {
		t.Fatalf("server holds %d of %d bytes", total, copied)
	}
}

func readRange(t *testing.T, f *os.File, from, to int64) []byte {
	b := make([]byte, to-from)
	if _, err := f.ReadAt(b, from); err != nil {
		t.Fatal(err)
	}
	return b
}
