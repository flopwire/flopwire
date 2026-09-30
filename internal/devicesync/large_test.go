package devicesync

import (
	"bufio"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// A 200MB source syncs with memory bounded by the chunk and request sizes,
// not the file size.
func TestLargeFileBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("200MB file")
	}
	e := newEnv(t, Config{Chunk: DefaultChunkParams}, 1<<20)
	e.srv.Discard = true
	sp := e.spec("big.jsonl", transcript.StorageJSONLAppend)
	const size = 200 << 20
	f, err := os.Create(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	rng := rand.NewChaCha8([32]byte{42})
	raw := make([]byte, 1500)
	line := make([]byte, 0, 4000)
	var written int64
	for written < size {
		rng.Read(raw[:500+int(written%1000)])
		line = append(line[:0], `{"type":"response_item","payload":{"text":"`...)
		line = hex.AppendEncode(line, raw[:500+int(written%1000)])
		line = append(line, "\"}}\n"...)
		w.Write(line)
		written += int64(len(line))
	}
	w.Flush()
	f.Close()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	}()
	start := time.Now()
	e.sync(sp)
	close(stop)
	wg.Wait()
	growth := int64(peak.Load()) - int64(base.HeapInuse)
	t.Logf("synced %d MB in %v; %d requests; peak heap growth %d MB", written>>20, time.Since(start), e.srv.FlushRequests, growth>>20)
	if growth > 128<<20 {
		t.Fatalf("peak heap grew %d MB for a %d MB file", growth>>20, written>>20)
	}
	entries, tail := e.srv.Manifest(sp.Path, fileIDOf(t, sp.Path), 0)
	var total int64
	for _, en := range entries {
		total += en.Size
	}
	if tail != nil {
		total += tail.Size
	}
	if total != written {
		t.Fatalf("server holds %d of %d bytes", total, written)
	}
	// Spot-check manifest hashes against the file.
	fh, _ := os.Open(sp.Path)
	defer fh.Close()
	for _, en := range []syncproto.Entry{entries[0], entries[len(entries)/2], entries[len(entries)-1]} {
		b := make([]byte, en.Size)
		fh.ReadAt(b, en.Offset)
		if syncproto.Sum(b) != en.Hash {
			t.Fatalf("entry %d hash mismatch", en.Ordinal)
		}
	}
}
