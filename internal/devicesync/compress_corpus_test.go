package devicesync

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/klauspost/compress/zstd"
)

// FLOPWIRE_CORPUS=1 go test -run CorpusCompression ./internal/devicesync
//
// Cuts every source of the real corpus (Claude, Codex, and each Devin
// session's export; read-only) into chunks the way a sync does, and
// compresses each unique finalized chunk once as the device would, then
// decodes it as the server does. Reports the ratio per harness, and the
// single-core cost of each side.
func TestCorpusCompression(t *testing.T) {
	roots := corpusRoots(t)
	ctx := context.Background()
	type tally struct {
		files               int
		raw, z, tail        int64
		enc, dec            time.Duration
		refs, refRaw, refsZ int64
	}
	by := map[string]*tally{}
	seen := map[syncproto.Hash]int64{} // compressed size of each unique chunk
	buf := make([]byte, DefaultChunkParams.Max)
	var zbuf, dbuf []byte
	var sample [][]byte // every 16th unique chunk, up to 256MB, for the level comparison
	var sampled int64
	add := func(harness string, r io.ReaderAt, size int64) {
		tl := by[harness]
		if tl == nil {
			tl = &tally{}
			by[harness] = tl
		}
		tl.files++
		tail, err := Scan(DefaultChunkParams, r, 0, size, buf, func(c Chunk, b []byte) error {
			tl.refs++
			tl.refRaw += c.Size
			if zn, ok := seen[c.Hash]; ok {
				tl.refsZ += zn
				return nil
			}
			t0 := cpuNow()
			zbuf = syncproto.Compress(zbuf[:0], b)
			tl.enc += cpuNow() - t0
			t0 = cpuNow()
			out, err := syncproto.Decompress(dbuf, zbuf, c.Size, c.Hash)
			tl.dec += cpuNow() - t0
			if err != nil {
				return err
			}
			dbuf = out[:0]
			seen[c.Hash] = int64(len(zbuf))
			if len(seen)%16 == 0 && sampled < 256<<20 {
				sample = append(sample, slices.Clone(b))
				sampled += int64(len(b))
			}
			tl.raw += c.Size
			tl.z += int64(len(zbuf))
			tl.refsZ += int64(len(zbuf))
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", harness, err)
		}
		tl.tail += size - tail
	}
	for _, root := range roots {
		harness := "claude"
		if strings.Contains(root, "/.codex/") {
			harness = "codex"
		}
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			f, err := os.Open(path)
			if err != nil {
				return nil
			}
			defer f.Close()
			if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
				add(harness, f, fi.Size())
			}
			return nil
		})
	}
	home, _ := os.UserHomeDir()
	if db := devin.DefaultPath(home); db != "" {
		if sessions, err := devin.ListSessions(ctx, db); err == nil {
			for _, s := range sessions {
				if data, err := devin.Export(ctx, db, s); err == nil && len(data) > 0 {
					add("devin", bytes.NewReader(data), int64(len(data)))
				}
			}
		} else {
			t.Logf("devin: %v", err)
		}
	}
	var all tally
	names := make([]string, 0, len(by))
	for h := range by {
		names = append(names, h)
	}
	slices.Sort(names)
	mb := func(n int64) float64 { return float64(n) / 1e6 }
	rate := func(n int64, d time.Duration) float64 { return mb(n) / max(d.Seconds(), 1e-9) }
	for _, h := range append(names, "all") {
		tl := by[h]
		if h == "all" {
			tl = &all
		} else {
			all.files += tl.files
			all.raw += tl.raw
			all.z += tl.z
			all.tail += tl.tail
			all.enc += tl.enc
			all.dec += tl.dec
			all.refs += tl.refs
			all.refRaw += tl.refRaw
			all.refsZ += tl.refsZ
		}
		t.Logf("%-6s files %6d  unique chunks %8.1f MB -> %7.1f MB (ratio %.2f)  all refs %8.1f MB -> %7.1f MB  tails %6.1f MB  compress %6.1f cpu-s (%4.0f MB/s)  decode %5.1f cpu-s (%5.0f MB/s)",
			h, tl.files, mb(tl.raw), mb(tl.z), float64(tl.raw)/float64(max(tl.z, 1)), mb(tl.refRaw), mb(tl.refsZ), mb(tl.tail),
			tl.enc.Seconds(), rate(tl.raw, tl.enc), tl.dec.Seconds(), rate(tl.raw, tl.dec))
	}
	// Encoder levels on the sample: ratio and CPU per side.
	for _, lv := range []zstd.EncoderLevel{zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression} {
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(lv), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(4<<20), zstd.WithLowerEncoderMem(true))
		dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		var raw, z int64
		var ec, dc time.Duration
		for _, b := range sample {
			t0 := cpuNow()
			zb := enc.EncodeAll(b, zbuf[:0])
			ec += cpuNow() - t0
			t0 = cpuNow()
			out, err := dec.DecodeAll(zb, dbuf[:0])
			dc += cpuNow() - t0
			if err != nil || !bytes.Equal(out, b) {
				t.Fatalf("level %v: %v", lv, err)
			}
			zbuf, dbuf = zb[:0], out[:0]
			raw += int64(len(b))
			z += int64(len(zb))
		}
		dec.Close()
		t.Logf("level %-22v sample %.1f MB: ratio %.2f, compress %4.0f MB/cpu-s, decode %5.0f MB/cpu-s", lv, mb(raw), float64(raw)/float64(z), rate(raw, ec), rate(raw, dc))
	}
}

// cpuNow is the process's CPU time (user+system).
func cpuNow() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
