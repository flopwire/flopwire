package syncproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func frame(t *testing.T, bodies [][]byte, tail []byte) (*FlushRequest, []byte) {
	t.Helper()
	h := FlushHeader{Version: Version, Source: Source{Path: "/p.jsonl", FileID: "1:2", Agent: "codex", StorageKind: "jsonl_append"},
		CapturedAt: time.Unix(1, 0).UTC(), Chunker: ChunkerParams{Algorithm: "fastcdc-v1.0.0", Min: 1, Avg: 2, Max: 4}}
	var payload []byte
	var off int64
	for i, b := range bodies {
		h.Entries = append(h.Entries, Entry{Ordinal: int64(i), Hash: Sum(b), Offset: off, Size: int64(len(b))})
		body, z := EncodeBody(b)
		h.Bodies = append(h.Bodies, body)
		payload = append(payload, z...)
		off += int64(len(b))
	}
	if tail != nil {
		h.Tail = &Tail{Offset: off, Size: int64(len(tail)), Hash: Sum(tail)}
		payload = append(payload, tail...)
	}
	req := &FlushRequest{Header: h, Payload: bytes.NewReader(payload)}
	var buf bytes.Buffer
	if err := EncodeFlush(&buf, req); err != nil {
		t.Fatal(err)
	}
	return req, buf.Bytes()
}

func TestFlushFrameRoundTrip(t *testing.T) {
	bodies := [][]byte{[]byte("hello "), []byte("world\n"), {0, 1, 2, 255}}
	req, raw := frame(t, bodies, []byte("partial li"))
	h, pr, err := DecodeFlush(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Entries) != 3 || h.Tail == nil || h.Tail.Offset != 16 || h.Source != req.Header.Source {
		t.Fatalf("header mismatch: %+v", h)
	}
	for i := range bodies {
		b, data, _, err := pr.NextBody(nil, nil)
		if err != nil || !bytes.Equal(data, bodies[i]) || b.Hash != Sum(bodies[i]) {
			t.Fatalf("body %d: %v %q", i, err, data)
		}
	}
	if _, _, _, err := pr.NextBody(nil, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	tail, err := pr.ReadTail(nil)
	if err != nil || string(tail) != "partial li" {
		t.Fatalf("tail %q %v", tail, err)
	}
}

func TestFlushFrameRejects(t *testing.T) {
	_, raw := frame(t, [][]byte{[]byte("abcdef")}, []byte("xy"))
	// Flip a byte of the compressed body: it no longer decodes to its hash.
	bad := append([]byte(nil), raw...)
	bad[len(bad)-3] ^= 1
	h, pr, err := DecodeFlush(bytes.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	_ = h
	if _, _, _, err := pr.NextBody(nil, nil); !errors.Is(err, ErrBadBody) {
		t.Fatalf("want a bad body, got %v", err)
	}
	// Trailing bytes.
	_, pr, _ = DecodeFlush(bytes.NewReader(append(append([]byte(nil), raw...), 'z')))
	pr.NextBody(nil, nil)
	if _, err := pr.ReadTail(nil); err == nil {
		t.Fatal("want trailing-bytes error")
	}
	// Wrong version byte.
	v2 := append([]byte(nil), raw...)
	v2[3] = 2
	if _, _, err := DecodeFlush(bytes.NewReader(v2)); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("want unsupported version, got %v", err)
	}
}

func TestHeaderValidate(t *testing.T) {
	base := func() FlushHeader {
		return FlushHeader{Version: Version, Source: Source{Path: "p", Agent: "a", StorageKind: "jsonl_append"},
			Entries: []Entry{{Ordinal: 3, Offset: 10, Size: 5}, {Ordinal: 4, Offset: 15, Size: 5}},
			Tail:    &Tail{Offset: 20, Size: 1}}
	}
	ok := base()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*FlushHeader){
		"gap ordinal":  func(h *FlushHeader) { h.Entries[1].Ordinal = 5 },
		"gap offset":   func(h *FlushHeader) { h.Entries[1].Offset = 16 },
		"tail offset":  func(h *FlushHeader) { h.Tail.Offset = 21 },
		"empty tail":   func(h *FlushHeader) { h.Tail.Size = 0 },
		"no agent":     func(h *FlushHeader) { h.Source.Agent = "" },
		"version":      func(h *FlushHeader) { h.Version = 9 },
		"zero entry":   func(h *FlushHeader) { h.Entries[0].Size = 0 },
		"negative gen": func(h *FlushHeader) { h.Generation = -1 },
		// S2: announcements that would allocate unbounded memory, or whose
		// end overflows, are refused before any payload is read.
		"huge body":            func(h *FlushHeader) { h.Bodies = []Body{{Size: MaxPartBytes + 1, ZSize: 10}} },
		"huge compressed body": func(h *FlushHeader) { h.Bodies = []Body{{Size: 10, ZSize: MaxPartBytes + maxFrameOverhead + 1}} },
		"no compressed size":   func(h *FlushHeader) { h.Bodies = []Body{{Size: 10}} },
		"huge tail":            func(h *FlushHeader) { h.Tail.Size = MaxPartBytes + 1 },
		"huge entry": func(h *FlushHeader) {
			h.Entries = []Entry{{Ordinal: 0, Offset: 0, Size: MaxPartBytes + 1}}
			h.Tail = nil
		},
		"entry end overflow": func(h *FlushHeader) {
			h.Entries = []Entry{{Ordinal: 0, Offset: math.MaxInt64 - 2, Size: 5}}
			h.Tail = nil
		},
		"tail end overflow": func(h *FlushHeader) { h.Entries, h.Tail = nil, &Tail{Offset: math.MaxInt64 - 2, Size: 5} },
		"payload total": func(h *FlushHeader) {
			for range MaxPayloadBytes/MaxPartBytes + 1 {
				h.Bodies = append(h.Bodies, Body{Size: MaxPartBytes, ZSize: MaxPartBytes})
			}
		},
	} {
		h := base()
		mut(&h)
		if h.Validate() == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestHashJSON(t *testing.T) {
	h := Sum([]byte("x"))
	raw, _ := json.Marshal(h)
	if len(raw) != 66 {
		t.Fatalf("hex form: %s", raw)
	}
	var back Hash
	if err := json.Unmarshal(raw, &back); err != nil || back != h {
		t.Fatalf("round trip: %v", err)
	}
	if json.Unmarshal([]byte(`"abc"`), &back) == nil {
		t.Fatal("short hash accepted")
	}
}

// A body is checked after decoding: a valid frame of other bytes, or one
// that decodes past MaxPartBytes (a zip bomb: 16MB of zeros is a few
// hundred bytes compressed), is refused, the bomb while decoding.
func TestDecompressBounds(t *testing.T) {
	a, b := []byte("the chunk"), []byte("other bytes")
	body, _ := EncodeBody(a)
	if _, err := Decompress(nil, Compress(nil, b), body.Size, body.Hash); !errors.Is(err, ErrBadBody) {
		t.Fatalf("other bytes: %v", err)
	}
	over := make([]byte, MaxPartBytes+1)
	z := Compress(nil, over)
	if len(z) > 64<<10 {
		t.Fatalf("zeros compressed to %d bytes", len(z))
	}
	if _, err := Decompress(nil, z, MaxPartBytes, Sum(over[:MaxPartBytes])); !errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		t.Fatalf("bomb: %v", err)
	}
	// The same without a content size in the frame header (a streamed
	// frame): refused while decoding.
	var streamed bytes.Buffer
	w, _ := zstd.NewWriter(&streamed)
	w.Write(over)
	w.Close()
	if _, err := Decompress(nil, streamed.Bytes(), MaxPartBytes, Sum(over[:MaxPartBytes])); !errors.Is(err, ErrBadBody) || !errors.Is(err, zstd.ErrDecoderSizeExceeded) {
		t.Fatalf("streamed bomb: %v", err)
	}
	// At the bound it decodes.
	at := over[:MaxPartBytes]
	if got, err := Decompress(nil, Compress(nil, at), MaxPartBytes, Sum(at)); err != nil || len(got) != MaxPartBytes {
		t.Fatalf("at the bound: %d %v", len(got), err)
	}
	// A frame whose content decodes shorter than announced.
	if _, err := Decompress(nil, Compress(nil, a[:4]), body.Size, body.Hash); !errors.Is(err, ErrBadBody) {
		t.Fatalf("short: %v", err)
	}
}

// A body is decoded no further than the size it declares: a frame that
// expands past it, with or without a content size in its header, is
// refused before its output is allocated or decoded.
func TestDecompressBoundedByDeclaredSize(t *testing.T) {
	const declared = 1 << 10
	big := make([]byte, 4<<20) // past the declared size, far under MaxPartBytes
	var streamed bytes.Buffer
	w, _ := zstd.NewWriter(&streamed)
	w.Write(big)
	w.Close()
	for name, z := range map[string][]byte{"content size": Compress(nil, big), "streamed": streamed.Bytes()} {
		Decompress(nil, z, declared, Sum(big[:declared])) // warm the decoder
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := Decompress(nil, z, declared, Sum(big[:declared]))
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrBadBody) {
			t.Errorf("%s: %v", name, err)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > 1<<20 {
			t.Errorf("%s: decoding a body declared at %d bytes allocated %d bytes", name, declared, n)
		}
	}
}
