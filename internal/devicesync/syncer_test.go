package devicesync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Resuming at the last finalized boundary must give exactly the cuts of a
// whole-file scan, however the bytes arrived.
func TestScanResumeMatchesFullScan(t *testing.T) {
	data := jsonlLines(1, 3000, 300)
	want, wantTail := scanAll(t, small, data)
	if len(want) < 50 {
		t.Fatalf("only %d chunks", len(want))
	}
	for _, c := range want[:len(want)-1] {
		if c.Size < int64(small.Min) || c.Size > int64(small.Max) {
			t.Fatalf("chunk size %d out of bounds", c.Size)
		}
	}
	r := rand.New(rand.NewPCG(2, 2))
	var got []Chunk
	var from, size int64
	for size < int64(len(data)) {
		size = min(int64(len(data)), size+int64(r.IntN(20000)))
		tail, err := Scan(small, bytes.NewReader(data[:size]), from, size, nil, func(c Chunk, _ []byte) error {
			got = append(got, c)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		from = tail
	}
	if from != wantTail || len(got) != len(want) {
		t.Fatalf("incremental: %d chunks tail %d; full: %d chunks tail %d", len(got), from, len(want), wantTail)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunk %d differs: %+v vs %+v", i, got[i], want[i])
		}
	}
}

// Append stream: entries never change once committed, the provisional tail
// moves forward and turns into finalized chunks, and the server always
// reconstructs exactly the bytes the device saw.
func TestAppendStreamTailTransitions(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("rollout.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(3, 800, 250)
	var committed []syncproto.Entry
	sawTailOnly, sawFinalize := false, false
	for pos := 0; pos < len(data); {
		next := min(len(data), pos+3000+pos%5000)
		appendFile(t, sp.Path, data[pos:next])
		pos = next
		e.sync(sp)
		id := fileIDOf(t, sp.Path)
		// A partial last line waits for its newline.
		e.requireServerHas(sp.Path, id, 0, completeLines(data[:pos]))
		entries, tail := e.srv.Manifest(sp.Path, id, 0)
		if len(entries) < len(committed) {
			t.Fatal("manifest shrank")
		}
		for i := range committed {
			if entries[i] != committed[i] {
				t.Fatalf("entry %d changed", i)
			}
		}
		if len(entries) == len(committed) && tail != nil {
			sawTailOnly = true
		}
		if len(entries) > len(committed) {
			sawFinalize = true
		}
		committed = entries
	}
	if !sawTailOnly || !sawFinalize {
		t.Fatalf("tail-only flush %v, finalizing flush %v", sawTailOnly, sawFinalize)
	}
	// Nothing pending, spool untouched (append-only files are never copied).
	if specs, _ := e.store.PendingSpecs(context.Background()); len(specs) != 0 {
		t.Fatalf("pending after sync: %v", specs)
	}
	if e.spool.Used() != 0 {
		t.Fatalf("append-only source spooled %d bytes", e.spool.Used())
	}
	// Unchanged source: no request.
	before := e.srv.FlushRequests
	e.sync(sp)
	if e.srv.FlushRequests != before {
		t.Fatal("unchanged source flushed")
	}
}

// A rewritten JSON document starts a new generation; CDC dedupes the
// unchanged prefix, so the upload carries only the changed region.
func TestRewritePrefixDedupe(t *testing.T) {
	e := newEnv(t, Config{Chunk: DefaultChunkParams}, 64<<20)
	sp := e.spec("session.json", transcript.StorageJSONDoc)
	sp.Agent = "gemini"
	doc := func(n int, edit string) []byte {
		var b strings.Builder
		b.WriteString(`{"sessionId":"s1","lastUpdated":"` + edit + `","messages":[`)
		r := rand.New(rand.NewPCG(9, 9))
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":"m%d","type":"gemini","content":"%x"}`, i, r.Uint64()*uint64(i+1))
			b.WriteString(strings.Repeat(" padding text", r.IntN(40)))
		}
		b.WriteString("]}")
		return []byte(b.String())
	}
	v1 := doc(40000, "t1")
	if err := os.WriteFile(sp.Path, v1, 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(sp)
	_, bytes1 := e.srv.UniqueChunks()
	// Rewrite: new timestamp in the head and 200 more messages at the end,
	// written to a temp file and renamed over (new inode), as editors and
	// Gemini CLI do.
	v2 := doc(40200, "t2")
	tmp := sp.Path + ".tmp"
	if err := os.WriteFile(tmp, v2, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, sp.Path); err != nil {
		t.Fatal(err)
	}
	e.sync(sp)
	_, bytes2 := e.srv.UniqueChunks()
	id := fileIDOf(t, sp.Path)
	e.requireServerHas(sp.Path, id, 1, v2)
	desc, latest, _ := e.srv.Source(sp.Path, id)
	if latest != 1 || desc.Previous == nil {
		t.Fatalf("want generation 1 with a previous link, got %d %+v", latest, desc.Previous)
	}
	newBytes := bytes2 - bytes1
	ratio := 1 - float64(newBytes)/float64(len(v2))
	t.Logf("doc %d bytes; rewrite uploaded %d new chunk bytes; dedupe %.1f%%", len(v2), newBytes, 100*ratio)
	if ratio < 0.7 {
		t.Fatalf("dedupe ratio %.2f too low", ratio)
	}
	if e.spool.Used() != 0 {
		t.Fatalf("spool not released after ack: %d bytes", e.spool.Used())
	}
}

// Server down: nothing is lost, the capture persists, and the upload
// resumes from the acknowledged offset when the server returns. A lost
// acknowledgement is retried idempotently.
func TestServerDownUpAndLostAck(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("r.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(4, 600, 200)
	appendFile(t, sp.Path, data[:40000])
	e.srv.SetDown(true)
	err := e.sy.Sync(context.Background(), sp)
	if err == nil || !syncproto.Retryable(err) {
		t.Fatalf("want retryable error, got %v", err)
	}
	if specs, _ := e.store.PendingSpecs(context.Background()); len(specs) != 1 {
		t.Fatalf("want 1 pending source, got %v", specs)
	}
	appendFile(t, sp.Path, data[40000:70000])
	e.srv.SetDown(false)
	e.srv.SetDropAck(true)
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("want error on lost ack")
	}
	e.srv.SetDropAck(false)
	appendFile(t, sp.Path, data[70000:])
	e.sync(sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	// Re-send the whole generation (device forgot its acks): idempotent.
	if _, err := e.store.db.Exec(`UPDATE devsync_gens SET acked = 0, tail_acked = 0`); err != nil {
		t.Fatal(err)
	}
	e.sync(sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
}

// Partial acknowledgement: the server commits one entry per request; the
// device keeps going from the acknowledged watermark. A chunk the device
// believes the server has, but which is missing, is re-sent.
func TestPartialAckAndMissing(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("p.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(5, 500, 200)
	appendFile(t, sp.Path, data)
	chunks, _ := scanAll(t, small, data)
	// Claim the server has the third chunk: the device omits its body, the
	// server answers Missing, the device forgets and re-sends it.
	if err := e.store.remember(context.Background(), []syncproto.Hash{chunks[2].Hash}); err != nil {
		t.Fatal(err)
	}
	e.srv.SetAckCap(1)
	e.sync(sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	if e.srv.FlushRequests < len(chunks) {
		t.Fatalf("want at least %d requests, got %d", len(chunks), e.srv.FlushRequests)
	}
}

// Large first sync and backlog: split into requests of at most
// MaxRequestBytes, with /has used above the threshold.
func TestBatchingAndHas(t *testing.T) {
	e := newEnv(t, Config{MaxRequestBytes: 64 << 10, HasThreshold: 32 << 10}, 1<<20)
	a := e.spec("a.jsonl", transcript.StorageJSONLAppend)
	b := e.spec("b.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(6, 2000, 200)
	appendFile(t, a.Path, data)
	e.sync(a)
	for _, n := range e.srv.FrameBytes {
		if n > 64<<10+int64(small.Max) {
			t.Fatalf("request payload %d over cap", n)
		}
	}
	// b is a copy of a (a Codex fork copies its parent's history): the
	// device learns from /has that the server holds the chunks.
	appendFile(t, b.Path, data)
	if _, err := e.store.db.Exec(`DELETE FROM devsync_known`); err != nil {
		t.Fatal(err)
	}
	before := e.srv.BodyBytes
	e.sync(b)
	e.requireServerHas(b.Path, fileIDOf(t, b.Path), 0, data)
	if sent := e.srv.BodyBytes - before; sent != 0 {
		t.Fatalf("duplicate file re-sent %d chunk bytes", sent)
	}
}

// 1.2MB lines are chunked like anything else (Max cut) and round-trip.
func TestHugeLines(t *testing.T) {
	e := newEnv(t, Config{Chunk: DefaultChunkParams}, 1<<20)
	sp := e.spec("huge.jsonl", transcript.StorageJSONLAppend)
	var data []byte
	for i := range 4 {
		data = append(data, jsonlLines(uint64(10+i), 20, 300)...)
		data = append(data, []byte(`{"type":"compacted","payload":"`+fmt.Sprint(i)+strings.Repeat("0123456789abcdef", 1_200_000/16)+"\"}\n")...)
	}
	appendFile(t, sp.Path, data[:len(data)/2])
	e.sync(sp)
	appendFile(t, sp.Path, data[len(data)/2:])
	e.sync(sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	if len(data) < 4_800_000 {
		t.Fatalf("fixture too small: %d", len(data))
	}
}

// Companions sync as their own sources with a parent link.
func TestCompanionFiles(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	parent := e.spec("sess.jsonl", transcript.StorageJSONLAppend)
	parent.Agent = transcript.AgentClaude
	appendFile(t, parent.Path, jsonlLines(7, 50, 100))
	os.MkdirAll(e.path("sess/tool-results"), 0o700)
	png := SourceSpec{Path: e.path("sess/tool-results/shot.png"), Agent: transcript.AgentClaude,
		StorageKind: transcript.StorageDir, Parent: parent.Path}
	img := make([]byte, 70000)
	rand.NewChaCha8([32]byte{1}).Read(img)
	if err := os.WriteFile(png.Path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	e.sync(parent)
	e.sync(png)
	id := fileIDOf(t, png.Path)
	e.requireServerHas(png.Path, id, 0, img)
	desc, _, _ := e.srv.Source(png.Path, id)
	if desc.Parent == nil || desc.Parent.Path != parent.Path || desc.Parent.FileID != fileIDOf(t, parent.Path) {
		t.Fatalf("parent link: %+v", desc.Parent)
	}
}

// An append-only file replaced before upload: the unacknowledged chunks are
// salvaged from the held descriptor into the spool, and both generations
// reach the server once it is back.
func TestReplacedBeforeAckIsSalvaged(t *testing.T) {
	e := newEnv(t, Config{}, 4<<20)
	sp := e.spec("s.jsonl", transcript.StorageJSONLAppend)
	old := jsonlLines(8, 400, 200)
	appendFile(t, sp.Path, old)
	oldID := fileIDOf(t, sp.Path)
	e.srv.SetDown(true)
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("want error while down")
	}
	// Compaction cleanup: write a new file and rename it over.
	nu := append([]byte(`{"type":"summary"}`+"\n"), old[len(old)/2:]...)
	os.WriteFile(sp.Path+".new", nu, 0o600)
	os.Rename(sp.Path+".new", sp.Path)
	if err := e.sy.Sync(context.Background(), sp); err == nil {
		t.Fatal("want error while down")
	}
	if e.spool.Used() == 0 {
		t.Fatal("nothing salvaged")
	}
	e.srv.SetDown(false)
	e.sync(sp)
	e.requireServerHas(sp.Path, oldID, 0, old)
	newID := fileIDOf(t, sp.Path)
	e.requireServerHas(sp.Path, newID, 1, nu)
	if e.spool.Used() != 0 {
		t.Fatalf("spool not released: %d", e.spool.Used())
	}
}

// In-place truncation destroys unacknowledged bytes: the gap is recorded,
// the recoverable prefix uploaded, and the new generation proceeds.
func TestTruncatedBeforeAckRecordsGap(t *testing.T) {
	e := newEnv(t, Config{}, 4<<20)
	sp := e.spec("t.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(9, 400, 200)
	appendFile(t, sp.Path, data)
	e.srv.SetDown(true)
	e.sy.Sync(context.Background(), sp)
	if err := os.WriteFile(sp.Path, data[:5000], 0o600); err != nil { // same inode, truncated
		t.Fatal(err)
	}
	e.sy.Sync(context.Background(), sp)
	e.srv.SetDown(false)
	e.sync(sp)
	if !strings.Contains(e.logs.String(), "lost before upload") {
		t.Fatalf("gap not logged:\n%s", e.logs)
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 1, completeLines(data[:5000]))
}

// completeLines is b up to its last newline: what the device uploads of
// an append-only transcript that is not idle.
func completeLines(b []byte) []byte {
	return b[:bytes.LastIndexByte(b, '\n')+1]
}

// The spool cap blocks loudly and never evicts: a rewritten document whose
// new version does not fit is not captured until the pending version is
// acknowledged.
func TestSpoolCapBlocks(t *testing.T) {
	e := newEnv(t, Config{}, 60<<10)
	sp := e.spec("d.json", transcript.StorageJSONDoc)
	v1 := jsonlLines(11, 150, 200) // ~45KB
	os.WriteFile(sp.Path, v1, 0o600)
	e.srv.SetDown(true)
	e.sy.Sync(context.Background(), sp)
	used := e.spool.Used()
	if used < int64(len(v1)) {
		t.Fatalf("v1 not spooled: %d", used)
	}
	v2 := jsonlLines(12, 150, 200)
	os.WriteFile(sp.Path+".n", v2, 0o600)
	os.Rename(sp.Path+".n", sp.Path)
	err := e.sy.Sync(context.Background(), sp)
	if !errors.Is(err, ErrSpoolFull) || !e.spool.Blocked() {
		t.Fatalf("want spool full, got %v blocked=%v", err, e.spool.Blocked())
	}
	if e.spool.Used() != used {
		t.Fatal("spool evicted or grew past the cap")
	}
	if !strings.Contains(e.logs.String(), "spool full") {
		t.Fatal("spool full not logged")
	}
	// Server back: v1 uploads, frees the spool, then v2 is captured.
	e.srv.SetDown(false)
	e.sync(sp)
	src, _, _ := e.srv.Source(sp.Path, fileIDOf(t, sp.Path))
	_ = src
	if _, err := e.srv.Reconstruct(sp.Path, fileIDOf(t, sp.Path), 1); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 1, v2)
	if e.spool.Blocked() || e.spool.Used() != 0 {
		t.Fatalf("spool still blocked=%v used=%d", e.spool.Blocked(), e.spool.Used())
	}
}

// SQLite exports: append when the export extends the previous one, new
// generation otherwise; retries come from the spool.
func TestExportSource(t *testing.T) {
	e := newEnv(t, Config{}, 4<<20)
	sp := SourceSpec{Path: "devin:sessions.db#s1", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin@1", Export: true}
	rows := jsonlLines(13, 300, 150)
	ctx := context.Background()
	e.srv.SetDown(true)
	if err := e.sy.SyncExport(ctx, sp, rows[:20000]); err == nil {
		t.Fatal("want error while down")
	}
	e.srv.SetDown(false)
	if err := e.sy.Resume(ctx, sp); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, "", 0, rows[:20000])
	if err := e.sy.SyncExport(ctx, sp, rows); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, "", 0, rows)
	// A session delete rewrites the export.
	if err := e.sy.SyncExport(ctx, sp, rows[5000:]); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, "", 1, rows[5000:])
	if e.spool.Used() != 0 {
		t.Fatalf("spool not released: %d", e.spool.Used())
	}
}

// Device state lost (fresh store) while the server has a generation with
// other content: the server demands a new generation and the device
// complies.
func TestNewGenerationRequired(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("g.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(14, 200, 200))
	e.sync(sp)
	// Rewrite in place with the same size at the same inode, then wipe the
	// device state so it re-sends generation 0 with different chunks.
	b, _ := os.ReadFile(sp.Path)
	copy(b, bytes.ToUpper(b[:len(b)/2]))
	os.WriteFile(sp.Path, b, 0o600)
	if _, err := e.store.db.Exec(`DELETE FROM devsync_sources; DELETE FROM devsync_gens; DELETE FROM devsync_manifest`); err != nil {
		t.Fatal(err)
	}
	e.sync(sp)
	id := fileIDOf(t, sp.Path)
	if _, latest, _ := e.srv.Source(sp.Path, id); latest != 1 {
		t.Fatalf("want server generation 1, got %d", latest)
	}
	e.requireServerHas(sp.Path, id, 1, b)
}

// A source that goes quiet has its tail sealed into a final chunk, so a
// finished transcript ends up wholly in finalized chunks; appends resume
// after the sealed boundary.
func TestIdleTailSealed(t *testing.T) {
	e := newEnv(t, Config{SealAfter: 300 * time.Millisecond}, 1<<20)
	sp := e.spec("idle.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(30, 300, 200)
	appendFile(t, sp.Path, data[:30000])
	e.sync(sp)
	id := fileIDOf(t, sp.Path)
	if _, tail := e.srv.Manifest(sp.Path, id, 0); tail == nil {
		t.Fatal("want a provisional tail while active")
	}
	time.Sleep(400 * time.Millisecond)
	e.sync(sp)
	entries, tail := e.srv.Manifest(sp.Path, id, 0)
	if tail != nil || entries[len(entries)-1].End() != int64(len(completeLines(data[:30000]))) {
		t.Fatalf("tail not sealed: tail %+v, end %d", tail, entries[len(entries)-1].End())
	}
	appendFile(t, sp.Path, data[30000:])
	e.sync(sp)
	e.requireServerHas(sp.Path, id, 0, data)
}

// Bodies travel as zstd frames, and MaxRequestBytes caps the compressed
// bytes: a compressible backlog fills each request to near the cap in
// fewer requests than its uncompressed size needs, the server decodes
// every body to the uploaded file, and no request passes the cap (plus
// the uncompressed tail).
func TestCompressedRequestCap(t *testing.T) {
	const limit = 64 << 10
	e := newEnv(t, Config{MaxRequestBytes: limit}, 1<<20)
	sp := e.spec("z.jsonl", transcript.StorageJSONLAppend)
	words := strings.Fields("the agent read the file and ran the tests then fixed the failing assertion in the parser")
	r := rand.New(rand.NewPCG(3, 9))
	var data []byte
	for i := 0; len(data) < 2<<20; i++ {
		var text []string
		for range 30 + r.IntN(60) {
			text = append(text, words[r.IntN(len(words))])
		}
		data = fmt.Appendf(data, `{"ordinal":%d,"type":"response_item","payload":{"text":"%s"}}`+"\n", i, strings.Join(text, " "))
	}
	appendFile(t, sp.Path, data)
	e.sync(sp)
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	var fullest int64
	for _, n := range e.srv.FrameBytes {
		if n > limit+int64(small.Max) {
			t.Fatalf("request payload %d over the cap", n)
		}
		fullest = max(fullest, n)
	}
	if e.srv.WireBytes*3 > e.srv.BodyBytes {
		t.Fatalf("bodies sent %d bytes for %d uncompressed", e.srv.WireBytes, e.srv.BodyBytes)
	}
	if uncompressed := e.srv.BodyBytes / limit; int64(e.srv.FlushRequests)*2 > uncompressed || fullest < limit/2 {
		t.Fatalf("%d requests (fullest %d bytes) for %d bytes of bodies: requests not filled with compressed bodies",
			e.srv.FlushRequests, fullest, e.srv.BodyBytes)
	}
}

// SyncUpTo captures no further than its bound: the device agent holds
// back bytes it has not parsed (a later line may name a directory a path
// rule covers). A lower bound than an earlier capture changes nothing.
func TestSyncUpToStopsAtBound(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	sp := e.spec("rollout.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(4, 200, 250)
	appendFile(t, sp.Path, data)
	half := int64(bytes.LastIndexByte(data[:len(data)/2], '\n') + 1)
	if err := e.sy.SyncUpTo(context.Background(), sp, half); err != nil {
		t.Fatal(err)
	}
	id := fileIDOf(t, sp.Path)
	e.requireServerHas(sp.Path, id, 0, data[:half])
	if err := e.sy.SyncUpTo(context.Background(), sp, half/2); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, id, 0, data[:half])
	e.sync(sp)
	e.requireServerHas(sp.Path, id, 0, data)
}
