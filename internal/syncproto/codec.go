package syncproto

// Chunk bodies travel and rest compressed: each Body in a flush payload is
// one zstd frame of the chunk's bytes, and the server stores that frame
// as the chunk's object without re-encoding it (chunks.encoding 'zstd').
// The content address stays the BLAKE3 of the uncompressed bytes, so
// deduplication and manifests do not depend on the encoder.

import (
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Encoding names how a chunk object is stored: the only one.
const Encoding = "zstd"

// maxFrameOverhead bounds what zstd adds to an incompressible part.
const maxFrameOverhead = 64 << 10

var (
	// One encoder per concurrent caller, pooled: the device compresses one
	// chunk at a time, and a window of the largest chunk keeps every chunk
	// one match window.
	zencPool = sync.Pool{New: func() any {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(4<<20), zstd.WithLowerEncoderMem(true))
		return e
	}}
	// The decoder writes no further than the capacity of its output (decode
	// gives it exactly the declared size), so a small frame that claims or
	// expands to more (a zip bomb) is refused at the declared size, before
	// more is allocated; and never past MaxPartBytes.
	zdec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(0), zstd.WithDecoderLowmem(true), zstd.WithDecodeAllCapLimit(true),
		zstd.WithDecoderMaxMemory(MaxPartBytes), zstd.WithDecoderMaxWindow(MaxPartBytes))
)

// Compress appends the zstd frame of data to dst.
func Compress(dst, data []byte) []byte {
	enc := zencPool.Get().(*zstd.Encoder)
	defer zencPool.Put(enc)
	return enc.EncodeAll(data, dst)
}

// ErrBadBody: a compressed chunk does not decode to what it claims.
var ErrBadBody = errors.New("syncproto: chunk body does not decode to its hash and size")

// Decompress decodes a chunk object or body frame into buf (grown as
// needed) and checks it is exactly size bytes hashing to want. Output
// past size is refused while decoding.
func Decompress(buf, z []byte, size int64, want Hash) ([]byte, error) {
	data, err := decode(buf, z, size)
	if err == nil && Sum(data) != want {
		err = ErrBadBody
	}
	return data, err
}

// DecompressAny decodes a chunk object that must be exactly size bytes,
// without checking a hash (the caller hashes the result).
func DecompressAny(z []byte, size int64) ([]byte, error) { return decode(nil, z, size) }

func decode(buf, z []byte, size int64) ([]byte, error) {
	if size <= 0 || size > MaxPartBytes {
		return nil, fmt.Errorf("%w: bad size %d", ErrBadBody, size)
	}
	if int64(cap(buf)) < size {
		buf = make([]byte, 0, size)
	}
	// Capacity exactly size: output past it is refused (WithDecodeAllCapLimit).
	data, err := zdec.DecodeAll(z, buf[:0:size])
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadBody, err)
	}
	if int64(len(data)) != size {
		return nil, ErrBadBody
	}
	return data, nil
}

// EncodeBody is the Body announcing data and its compressed frame.
func EncodeBody(data []byte) (Body, []byte) {
	z := Compress(nil, data)
	return Body{Hash: Sum(data), Size: int64(len(data)), ZSize: int64(len(z))}, z
}
