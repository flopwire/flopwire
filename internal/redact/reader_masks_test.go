package redact

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"
)

func TestReaderAtLargeLineMasks(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", "", string(bytes.Repeat([]byte{'\r'}, (64<<10)+7)) + "\n"} {
		t.Run("ending-length-"+strconv.Itoa(len(ending)), func(t *testing.T) {
			raw := bytes.Repeat([]byte{'x'}, 3*SegMax+41)
			raw[64<<10] = '\r' // Interior CR remains part of identity.
			spans := []Span{{SegMax - 5, SegMax + 37}, {2*SegMax + 15, 2*SegMax + 100}}
			middle := bytes.Clone(raw)
			FillSpans(middle, spans[:1], MessageRule)
			catalog := NewLineCatalog([]LineMask{
				{SHA: LineSum(raw), Spans: spans[:1], Proof: NewLineProof(raw, spans[:1])},
				{SHA: LineSum(middle), Spans: spans, Proof: NewLineProof(middle, spans)},
			})
			hybrid := bytes.Clone(raw)
			copy(hybrid[spans[0].Start:spans[0].Start+3], "***")
			for _, input := range [][]byte{raw, middle, hybrid} {
				before := []byte("neighbor\n")
				src := append(bytes.Clone(before), input...)
				src = append(src, ending...)
				want := bytes.Clone(src)
				fillLineSpans(want[len(before):], 0, spans)
				counted := &maskCountingReader{ReaderAt: bytes.NewReader(src)}
				r := NewReaderAt(counted, Lines)
				r.SetLineMasks(catalog)
				// First access starts in the last segment; then visit a marker across a cut.
				for _, off := range []int{len(before) + 2*SegMax + 15, len(before) + SegMax - 5, len(before) + SegMax + 3, 0} {
					b := make([]byte, 25)
					n, err := r.ReadAt(b, int64(off))
					if err != nil || !bytes.Equal(b[:n], want[off:off+n]) {
						t.Fatalf("random off=%d n=%d err=%v", off, n, err)
					}
				}
				// Fresh sequential reader verifies every byte, including terminators.
				counted.bytes = 0
				r = NewReaderAt(counted, Lines)
				r.SetLineMasks(catalog)
				got, err := io.ReadAll(io.NewSectionReader(r, 0, int64(len(src))))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("sequential mismatch err=%v", err)
				}
				if counted.bytes > int64(12*len(src)) {
					t.Fatalf("repeated whole-line scans: %d bytes for %d", counted.bytes, len(src))
				}
			}
		})
	}
}

type maskCountingReader struct {
	io.ReaderAt
	bytes int64
}

func (r *maskCountingReader) ReadAt(p []byte, off int64) (int, error) {
	n, e := r.ReaderAt.ReadAt(p, off)
	r.bytes += int64(n)
	return n, e
}

type maskFailReader struct {
	io.ReaderAt
	fail bool
}

var errMaskRead = errors.New("injected record read failure")

func (r *maskFailReader) ReadAt(p []byte, off int64) (int, error) {
	if r.fail && off >= 2*SegMax {
		return 0, errMaskRead
	}
	return r.ReaderAt.ReadAt(p, off)
}
func TestReaderAtLargeLineMaskReadFailureAndReset(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, 3*SegMax)
	spans := []Span{{10, 40}}
	catalog := NewLineCatalog([]LineMask{{SHA: LineSum(raw), Spans: spans}})
	src := &maskFailReader{ReaderAt: bytes.NewReader(raw), fail: true}
	r := NewReaderAt(src, Lines)
	r.SetLineMasks(catalog)
	out := make([]byte, 50)
	if n, err := r.ReadAt(out, 0); n != 0 || !errors.Is(err, errMaskRead) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	src.fail = false
	want := bytes.Clone(raw[:50])
	FillSpans(want, spans, MessageRule)
	if _, err := r.ReadAt(out, 0); err != nil || !bytes.Equal(out, want) {
		t.Fatalf("retry err=%v", err)
	}
	r.SetLineMasks(nil)
	if _, err := r.ReadAt(out, 0); err != nil || !bytes.Equal(out, raw[:50]) {
		t.Fatalf("reset err=%v", err)
	}
}

func TestReaderAtLegacyMaskClipsTerminators(t *testing.T) {
	raw := append(bytes.Repeat([]byte{'x'}, SegMax+3), '\r', '\n')
	catalog := NewLineCatalog([]LineMask{{SHA: LineSum(raw), Spans: []Span{{SegMax - 4, len(raw)}}}})
	r := NewReaderAt(bytes.NewReader(raw), Lines)
	r.SetLineMasks(catalog)
	got := make([]byte, 9)
	if _, err := r.ReadAt(got, SegMax-4); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[7:], []byte("\r\n")) || bytes.Contains(got[:7], []byte("x")) {
		t.Fatalf("terminators/spans %q", got)
	}
}

func TestReaderAtMaskFailureInvalidatesPriorSegment(t *testing.T) {
	before := []byte("neighbor must remain unchanged\n")
	raw := bytes.Repeat([]byte{'x'}, 3*SegMax)
	src := &maskFailReader{ReaderAt: bytes.NewReader(append(bytes.Clone(before), raw...))}
	r := NewReaderAt(src, Lines)
	r.SetLineMasks(NewLineCatalog([]LineMask{{SHA: LineSum(raw), Spans: []Span{{5, 35}}}}))
	got := make([]byte, len(before))
	if _, err := r.ReadAt(got, 0); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("initial neighbor err=%v", err)
	}
	src.fail = true
	if n, err := r.ReadAt(make([]byte, 50), int64(len(before))); n != 0 || !errors.Is(err, errMaskRead) {
		t.Fatalf("long record n=%d err=%v", n, err)
	}
	src.fail = false
	if _, err := r.ReadAt(got, 0); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("cached neighbor corrupted err=%v", err)
	}
}

func TestReaderAtShortMasksUseLoadedBytes(t *testing.T) {
	line := []byte("ordinary record with private message\n")
	src := bytes.Repeat(line, 2000)
	counted := &maskCountingReader{ReaderAt: bytes.NewReader(src)}
	r := NewReaderAt(counted, Lines)
	spans := []Span{{21, 36}}
	r.SetLineMasks(NewLineCatalog([]LineMask{{SHA: LineSum(line), Spans: spans}}))
	got, err := io.ReadAll(io.NewSectionReader(r, 0, int64(len(src))))
	wantLine := bytes.Clone(line)
	FillSpans(wantLine, spans, MessageRule)
	if err != nil || !bytes.Equal(got, bytes.Repeat(wantLine, 2000)) {
		t.Fatalf("short records err=%v", err)
	}
	if counted.bytes != int64(len(src)) {
		t.Fatalf("extra underlying short-record reads: %d, want %d", counted.bytes, len(src))
	}
}
