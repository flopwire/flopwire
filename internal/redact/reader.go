package redact

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"path"
	"strings"
	"sync"
)

// Mode is how a source's bytes are cut into the segments Find scans.
// Redaction must be a pure function of the source bytes, so that a
// chunk read again later (upload, salvage, a server read) redacts the
// same way; segments are therefore fixed by content, never by the offset
// a read happens to start at.
type Mode int

const (
	// Off passes bytes through (binary files).
	Off Mode = iota
	// Lines redacts each newline-terminated line on its own: JSONL
	// transcripts and exports, where appending never changes an earlier
	// line's redaction.
	Lines
	// Whole redacts the file as one text, so a secret may span lines (a
	// PEM block in a tool output file).
	Whole
)

// SegMax bounds a segment. A longer line (or a Whole file) is cut every
// SegMax bytes from its start, and each piece is scanned with Overlap
// bytes of context on either side, so a secret across a cut is still
// found (secrets are far shorter than Overlap).
const (
	SegMax  = 1 << 20
	Overlap = 64 << 10
)

var binaryExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".bmp": true, ".ico": true, ".heic": true,
	".pdf": true, ".zip": true, ".gz": true, ".tgz": true, ".zst": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".mp3": true, ".mp4": true, ".mov": true, ".wav": true, ".db": true, ".sqlite": true, ".wasm": true,
}

// ModeFor picks the mode for a source by its storage kind (transcript
// StorageKind values) and path. JSONL transcripts, JSON documents and
// SQLite exports are line-oriented (a JSON string never holds a raw
// newline); companion files are redacted whole unless binary.
func ModeFor(storageKind, p string) Mode {
	switch storageKind {
	case "cass_export", "jsonl_append", "json_doc", "sqlite":
		return Lines
	}
	if binaryExt[strings.ToLower(path.Ext(p))] {
		return Off
	}
	if strings.HasSuffix(p, ".jsonl") {
		return Lines
	}
	return Whole
}

// ReaderAt serves the redacted view of an underlying ReaderAt, at the
// same offsets. It caches the segment it last redacted, so sequential
// reads redact each byte about once. Safe for concurrent use.
type ReaderAt struct {
	r    io.ReaderAt
	mode Mode

	mu sync.Mutex
	// eof is the underlying size once a read has hit it; -1 until then.
	eof int64
	// the current segment: [segOff, segOff+len(seg)) redacted, inside a
	// line (Lines) that starts at lineOff; nl: the segment ends its line.
	seg     []byte
	segOff  int64
	lineOff int64
	nl      bool
	valid   bool
	// raw block cache
	blk    []byte
	blkOff int64

	countFrom int64
	counts    map[string]int64
	seen      map[int64]bool

	lineMasks   *LineCatalog
	maskLineOff int64
	maskSpans   []Span
}

// LineSum identifies a record without its structural line terminators.
func LineSum(line []byte) [32]byte {
	return sha256.Sum256(bytes.TrimRight(line, "\r\n"))
}

// SetLineMasks makes Lines-mode reads also mask redacted message lines.
func (x *ReaderAt) SetLineMasks(f *LineCatalog) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.lineMasks, x.valid = f, false
	x.maskLineOff, x.maskSpans = -1, nil
}

// NewReaderAt wraps r. A zero Mode (Off) returns reads unchanged.
func NewReaderAt(r io.ReaderAt, mode Mode) *ReaderAt {
	return &ReaderAt{r: r, mode: mode, eof: -1, countFrom: -1, maskLineOff: -1}
}

// CountFrom starts counting matches that begin at or after off (each
// once, however often its segment is read). Counts returns them.
func (x *ReaderAt) CountFrom(off int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.countFrom, x.counts, x.seen = off, map[string]int64{}, map[int64]bool{}
}

// Counts returns matches per rule counted since CountFrom.
func (x *ReaderAt) Counts() map[string]int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make(map[string]int64, len(x.counts))
	for k, v := range x.counts {
		out[k] = v
	}
	return out
}

func (x *ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if x.mode == Off {
		return x.r.ReadAt(p, off)
	}
	if off < 0 {
		return 0, errors.New("redact: negative offset")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if x.eof >= 0 && pos >= x.eof {
			return n, io.EOF
		}
		if !x.valid || pos < x.segOff || pos >= x.segOff+int64(len(x.seg)) {
			if err := x.load(pos); err != nil {
				return n, err
			}
			if pos >= x.segOff+int64(len(x.seg)) {
				return n, io.EOF // past the end of the file
			}
		}
		n += copy(p[n:], x.seg[pos-x.segOff:])
	}
	return n, nil
}

// load redacts the segment that contains pos.
func (x *ReaderAt) load(pos int64) error {
	var line int64
	switch {
	case x.mode == Whole:
		line = 0
	case x.valid && pos == x.segOff+int64(len(x.seg)):
		line = x.lineOff
		if x.nl {
			line = pos
		}
	case x.valid && pos >= x.lineOff && pos < x.segOff+int64(len(x.seg)):
		line = x.lineOff
	default:
		var err error
		if line, err = x.lineStart(pos); err != nil {
			return err
		}
	}
	// load may reuse the old segment buffer before a later read fails.
	// Never retain a valid cache entry for that partially rebuilt segment.
	x.valid = false
	s := line + (pos-line)/SegMax*SegMax
	raw, err := x.raw(s, s+SegMax)
	if err != nil {
		return err
	}
	e := s + int64(len(raw))
	nl := false
	if x.mode == Lines {
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			e, nl = s+int64(i)+1, true
		}
	}
	// Context: back to the previous cut of a long line, forward past a
	// cut that is not the end of the line (or file).
	cs := max(line, s-Overlap)
	ce := e
	if !nl && (x.eof < 0 || e < x.eof) {
		ce = e + Overlap
	}
	ctx, err := x.raw(cs, ce)
	if err != nil {
		return err
	}
	if x.mode == Lines && ce > e {
		if i := bytes.IndexByte(ctx[e-cs:], '\n'); i >= 0 {
			ctx = ctx[:e-cs+int64(i)+1]
		}
	}
	ms := Find(ctx)
	seg := append(x.seg[:0], ctx[s-cs:e-cs]...)
	for _, m := range ms {
		ms, me := cs+int64(m.Start), cs+int64(m.End)
		if me <= s || ms >= e {
			continue
		}
		mk := make([]byte, m.End-m.Start)
		marker(mk, ctx[m.Start:m.End], m.Rule, m.hashed)
		lo, hi := max(ms, s), min(me, e)
		copy(seg[lo-s:hi-s], mk[lo-ms:hi-ms])
		if x.counts != nil && ms >= s && ms >= x.countFrom && !x.seen[ms] {
			x.seen[ms] = true
			x.counts[m.Rule]++
		}
	}
	if x.lineMasks != nil && x.mode == Lines {
		if x.maskLineOff != line {
			var spans []Span
			if s == line && (nl || x.eof >= 0 && e == x.eof) {
				// The entire short line is already in the raw context.
				spans = x.lineMasks.MatchBytes(ctx[s-cs : e-cs])
			} else {
				var err error
				spans, err = x.lineMasks.MatchLine(x.r, line)
				if err != nil {
					return err
				}
			}
			x.maskLineOff, x.maskSpans = line, spans
		}
		fillLineSpans(seg, s-line, x.maskSpans)
	}
	x.seg, x.segOff, x.lineOff, x.nl, x.valid = seg, s, line, nl, true
	return nil
}

// lineStart finds the start of the line holding pos. It reads backwards
// in small steps straight from the source: the block cache reads 2*SegMax
// forward of each request, which would read a long line many times over.
func (x *ReaderAt) lineStart(pos int64) (int64, error) {
	const step = 64 << 10
	if from := max(0, pos-step); from >= x.blkOff && pos <= x.blkOff+int64(len(x.blk)) {
		if i := bytes.LastIndexByte(x.blk[from-x.blkOff:pos-x.blkOff], '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
	}
	buf := make([]byte, step)
	for end := pos; end > 0; {
		from := max(0, end-step)
		b := buf[:end-from]
		n, err := x.r.ReadAt(b, from)
		if n < len(b) && err != nil && err != io.EOF {
			return 0, err
		}
		b = b[:n] // short only past EOF, where load finds nothing to serve
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
		end = from
	}
	return 0, nil
}

// raw returns underlying bytes [from, to), shorter at EOF. The slice is
// valid until the next call.
func (x *ReaderAt) raw(from, to int64) ([]byte, error) {
	if x.eof >= 0 {
		to = min(to, x.eof)
	}
	if to <= from {
		return nil, nil
	}
	if from >= x.blkOff && to <= x.blkOff+int64(len(x.blk)) {
		return x.blk[from-x.blkOff : to-x.blkOff], nil
	}
	size := max(to-from, 2*SegMax)
	if x.eof >= 0 {
		size = min(size, x.eof-from)
	}
	if int64(cap(x.blk)) < size {
		x.blk = make([]byte, size)
	}
	x.blk = x.blk[:size]
	n, err := x.r.ReadAt(x.blk, from)
	x.blk, x.blkOff = x.blk[:n], from
	if err == io.EOF || err == nil && int64(n) < size {
		x.eof = from + int64(n)
	} else if err != nil {
		x.blk = x.blk[:0]
		return nil, err
	}
	return x.blk[:min(int64(n), to-from)], nil
}
