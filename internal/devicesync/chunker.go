// Package devicesync is the device side of raw-evidence sync (spec §6.4,
// §6.5): content-defined chunking of each source into generations of
// finalized chunks plus a provisional tail, a persisted per-source watermark
// that doubles as the upload queue, a spool for bytes the source may
// destroy before the server acknowledges them, and a debounced, backing-off
// uploader. The wire protocol is internal/syncproto.
package devicesync

import (
	"fmt"
	"io"

	chunkers "github.com/PlakarKorp/go-cdc-chunkers"
	_ "github.com/PlakarKorp/go-cdc-chunkers/chunkers/fastcdc" // registers fastcdc-v1.0.0

	"github.com/flopwire/flopwire/internal/syncproto"
)

// Algorithm is the go-cdc-chunkers algorithm name. The non-legacy FastCDC
// derives its masks from the average size.
const Algorithm = "fastcdc-v1.0.0"

// ChunkParams are the FastCDC sizes. Avg must be a power of two.
type ChunkParams struct {
	Min, Avg, Max int
}

// DefaultChunkParams: 128KB minimum, 512KB average, 4MB maximum (spec §6.4).
var DefaultChunkParams = ChunkParams{Min: 128 << 10, Avg: 512 << 10, Max: 4 << 20}

func (p ChunkParams) wire() syncproto.ChunkerParams {
	return syncproto.ChunkerParams{Algorithm: Algorithm, Min: p.Min, Avg: p.Avg, Max: p.Max}
}

// Chunk is one finalized chunk: a cut the chunker would make identically
// however many bytes followed it.
type Chunk struct {
	Offset int64
	Size   int64
	Hash   syncproto.Hash
}

// Scan cuts r's bytes [from, size) into content-defined chunks and calls fn
// for each finalized chunk, with its bytes (valid only during the call). It
// returns the offset where the provisional tail starts; the tail is
// [tail, size).
//
// FastCDC restarts its rolling hash at every cut: a cut depends only on the
// bytes from the previous cut to at most Max bytes after it. So resuming at
// the last finalized boundary yields the same cuts as scanning the whole
// file, and a cut is final exactly when bytes follow it (the algorithm found
// a content cut inside the window) or the chunk hit Max. The chunk that ends
// at size without reaching Max may move when bytes are appended: it is the
// tail.
//
// Memory is one scan buffer of Max bytes, reused through buf when it is
// large enough.
func Scan(p ChunkParams, r io.ReaderAt, from, size int64, buf []byte, fn func(Chunk, []byte) error) (tail int64, err error) {
	if from >= size {
		return from, nil
	}
	if len(buf) < p.Max {
		buf = make([]byte, p.Max)
	}
	opts := &chunkers.ChunkerOpts{MinSize: p.Min, NormalSize: p.Avg, MaxSize: p.Max}
	c, err := chunkers.NewChunkerBuffer(Algorithm, io.NewSectionReader(r, from, size-from), opts, buf)
	if err != nil {
		return 0, fmt.Errorf("devicesync: chunker: %w", err)
	}
	off := from
	for {
		data, err := c.Next()
		if err != nil && err != io.EOF {
			return 0, err
		}
		n := int64(len(data))
		if n > 0 {
			if off+n < size || n == int64(p.Max) {
				if ferr := fn(Chunk{Offset: off, Size: n, Hash: syncproto.Sum(data)}, data); ferr != nil {
					return 0, ferr
				}
				off += n
			} else {
				return off, nil
			}
		}
		if err == io.EOF {
			return off, nil
		}
	}
}

// Validate checks p the way the chunker will.
func (p ChunkParams) Validate() error {
	_, err := chunkers.NewChunker(Algorithm, eofReader{}, &chunkers.ChunkerOpts{MinSize: p.Min, NormalSize: p.Avg, MaxSize: p.Max})
	if err == nil && (p.Avg&(p.Avg-1) != 0 || p.Min >= p.Avg || p.Max <= p.Avg) {
		err = fmt.Errorf("devicesync: need min < avg < max and avg a power of two")
	}
	return err
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }
