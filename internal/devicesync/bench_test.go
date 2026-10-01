package devicesync

import (
	"bytes"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func BenchmarkScan(b *testing.B) {
	data := jsonlLines(1, 30000, 1000)
	buf := make([]byte, DefaultChunkParams.Max)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		Scan(DefaultChunkParams, bytes.NewReader(data), 0, int64(len(data)), buf, func(Chunk, []byte) error { return nil })
	}
}

func BenchmarkBlake3(b *testing.B) {
	data := jsonlLines(1, 30000, 1000)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		syncproto.Sum(data)
	}
}
