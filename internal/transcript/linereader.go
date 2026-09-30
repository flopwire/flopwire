// Line reader for JSONL transcripts.
//
// Design informed by kenn-io/agentsview internal/parser/linereader.go and
// its readJSONLFrom resume rule (consumed bytes cover only complete lines)
// at commit 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT). No code copied:
// agentsview skips lines over a size cap; this reader never drops a line and
// instead exposes oversized lines as a head prefix plus a streaming reader.

package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

const (
	// DefaultMaxLine is the largest physical line materialized in memory.
	DefaultMaxLine = 16 << 20
	// DefaultHeadSize is how much of an oversized line Line.Head keeps.
	DefaultHeadSize = 64 << 10
	readBufSize     = 64 << 10
)

// LineReaderOptions tunes a LineReader. Zero values select the defaults.
type LineReaderOptions struct {
	MaxLine  int // lines longer than this are Oversized and not materialized
	HeadSize int // bytes of an oversized line kept in Line.Head
	// Budget, when set, bounds the memory of large lines across every
	// reader sharing it (parse workers running in parallel). See LineBudget.
	Budget *LineBudget
}

// LineBudget bounds how many large lines are in memory at once. A reader
// whose line grows past BigLine reserves MaxLine bytes before reading on,
// shrinks the reservation to the line's size once it is complete, and
// returns it on its next Next (the parser decodes one line at a time), or
// when the scan ends. A reservation is granted whenever nothing else is
// reserved, so a single line larger than the budget still proceeds.
type LineBudget struct {
	Cap int64

	mu   sync.Mutex
	cond *sync.Cond
	used int64
}

// BigLine is the size above which a line reserves from a LineBudget.
const BigLine = 1 << 20

// NewLineBudget returns a budget of capBytes.
func NewLineBudget(capBytes int64) *LineBudget {
	b := &LineBudget{Cap: capBytes}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *LineBudget) acquire(n int64) {
	b.mu.Lock()
	for b.used > 0 && b.used+n > b.Cap {
		b.cond.Wait()
	}
	b.used += n
	b.mu.Unlock()
}

func (b *LineBudget) release(n int64) {
	if n == 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Line is one complete physical line: bytes up to and including '\n'.
type Line struct {
	No         int64 // 1-based physical line number
	Offset     int64 // byte offset of the first byte of the line
	Len        int64 // physical length including the "\n" or "\r\n" terminator
	ContentLen int64 // length without the terminator

	// Data is the content without terminator. It is borrowed and valid only
	// until the next call to Next. Nil when Oversized.
	Data []byte
	// Oversized lines are not materialized: Head holds their first
	// HeadSize bytes and LineReader.Open streams the whole content.
	Oversized bool
	Head      []byte
}

// Prefix returns Data, or Head for an oversized line. Use it to classify a
// line (PeekString) before deciding to decode it.
func (l *Line) Prefix() []byte {
	if l.Oversized {
		return l.Head
	}
	return l.Data
}

// LineReader streams complete lines from a byte range of an io.ReaderAt with
// exact offsets and line numbers. A final line without '\n' is incomplete:
// Next returns io.EOF without emitting it, Pending reports its length, and
// Offset stays before it, so a resumed read picks it up once it completes.
type LineReader struct {
	ra       io.ReaderAt
	br       *bufio.Reader
	maxLine  int
	headSize int
	off      int64 // offset just past the last complete line
	no       int64 // physical lines before off
	pending  int64
	buf      []byte
	head     []byte
	line     Line
	err      error
	budget   *LineBudget
	held     int64 // reserved from budget for the current line
}

// NewLineReader reads ra from offset start (which must be a line start, with
// lineNo lines before it) up to end.
func NewLineReader(ra io.ReaderAt, start, lineNo, end int64, opts LineReaderOptions) *LineReader {
	if opts.MaxLine <= 0 {
		opts.MaxLine = DefaultMaxLine
	}
	if opts.HeadSize <= 0 {
		opts.HeadSize = DefaultHeadSize
	}
	if end < start {
		end = start
	}
	return &LineReader{
		ra:       ra,
		br:       bufio.NewReaderSize(io.NewSectionReader(ra, start, end-start), readBufSize),
		maxLine:  opts.MaxLine,
		headSize: opts.HeadSize,
		budget:   opts.Budget,
		off:      start,
		no:       lineNo,
	}
}

// Offset is the byte offset just past the last complete line returned.
func (r *LineReader) Offset() int64 { return r.off }

// LineNo is the number of physical lines before Offset.
func (r *LineReader) LineNo() int64 { return r.no }

// Pending is the length of an incomplete final line seen at EOF, or zero.
func (r *LineReader) Pending() int64 { return r.pending }

// Next returns the next complete line, or io.EOF when none remain. The
// returned Line is reused by the next call.
func (r *LineReader) Next() (*Line, error) {
	r.Release()
	if r.err != nil {
		return nil, r.err
	}
	if cap(r.buf) > BigLine {
		r.buf = nil // a large line's buffer is not kept for the small ones after it
	}
	r.buf, r.head = r.buf[:0], r.head[:0]
	var n int64
	var direct []byte // the whole line when it fit in one bufio read
	oversized := false
	var prevLast, last2 byte
	for {
		chunk, err := r.br.ReadSlice('\n')
		if len(chunk) > 0 {
			if len(chunk) >= 2 {
				last2 = chunk[len(chunk)-2]
			} else {
				last2 = prevLast
			}
			prevLast = chunk[len(chunk)-1]
		}
		n += int64(len(chunk))
		switch {
		case err == nil && n == int64(len(chunk)) && n <= int64(r.maxLine):
			direct = chunk
		case oversized:
			r.head = appendCapped(r.head, chunk, r.headSize)
		case n > int64(r.maxLine):
			oversized = true
			r.head = appendCapped(appendCapped(r.head, r.buf, r.headSize), chunk, r.headSize)
			r.buf = r.buf[:0]
		default:
			if r.budget != nil && r.held == 0 && n > BigLine {
				r.held = int64(r.maxLine)
				r.budget.acquire(r.held)
			}
			r.buf = append(r.buf, chunk...)
		}
		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			r.pending = n
			r.err = io.EOF
			return nil, io.EOF
		}
		r.err = err
		return nil, err
	}

	term := int64(1)
	if last2 == '\r' && n >= 2 {
		term = 2
	}
	r.line = Line{No: r.no + 1, Offset: r.off, Len: n, ContentLen: n - term, Oversized: oversized}
	switch {
	case oversized:
		if int64(len(r.head)) > r.line.ContentLen {
			r.head = r.head[:r.line.ContentLen]
		}
		r.line.Head = r.head
	case direct != nil:
		r.line.Data = direct[:n-term]
	default:
		r.line.Data = r.buf[:n-term]
	}
	if r.held > n && !oversized {
		r.budget.release(r.held - n) // keep what the line needs
		r.held = n
	}
	r.off += n
	r.no++
	return &r.line, nil
}

// Release returns the current line's LineBudget reservation. Next calls
// it; ScanJSONL calls it when the scan ends.
func (r *LineReader) Release() {
	if r.held > 0 {
		r.budget.release(r.held)
		r.held = 0
	}
}

// Open streams the full content of l (without terminator) from the
// underlying ReaderAt. It reserves nothing from the LineBudget: a caller
// that buffers the whole line (json.Decoder does) uses Materialize.
func (r *LineReader) Open(l *Line) io.Reader {
	return io.NewSectionReader(r.ra, l.Offset, l.ContentLen)
}

// Materialize reads the whole content of l (without terminator) into dst,
// reusing its capacity, for a parser that decodes an oversized line whole.
// It first trades the line's current LineBudget reservation for one of the
// line's full size, so a large line waits for room like any other; the
// reservation is returned on the next Next or Release. The returned slice
// is valid until the next Materialize into the same dst.
func (r *LineReader) Materialize(l *Line, dst []byte) ([]byte, error) {
	if r.budget != nil {
		r.Release()
		r.held = max(l.ContentLen, 1)
		r.budget.acquire(r.held)
	}
	if int64(cap(dst)) < l.ContentLen {
		dst = make([]byte, l.ContentLen)
	}
	dst = dst[:l.ContentLen]
	n, err := r.ra.ReadAt(dst, l.Offset)
	if int64(n) == l.ContentLen {
		return dst, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

func appendCapped(dst, src []byte, max int) []byte {
	if room := max - len(dst); room > 0 {
		if len(src) > room {
			src = src[:room]
		}
		dst = append(dst, src...)
	}
	return dst
}

// PeekString returns the string value at a path of object keys in a JSON
// document without decoding sibling values into Go values, e.g.
// PeekString(r, "payload", "type"). It stops at the first match, so for a
// line whose discriminator precedes its payload only the prefix is read.
// It reports false when the path is absent, not a string, or truncated.
func PeekString(r io.Reader, path ...string) (string, bool) {
	if len(path) == 0 {
		return "", false
	}
	dec := json.NewDecoder(r)
	for depth := 0; ; depth++ {
		if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
			return "", false
		}
		for {
			if !dec.More() {
				return "", false
			}
			tok, err := dec.Token()
			if err != nil {
				return "", false
			}
			if tok == path[depth] {
				break
			}
			if skipValue(dec) != nil {
				return "", false
			}
		}
		if depth == len(path)-1 {
			tok, err := dec.Token()
			s, ok := tok.(string)
			return s, err == nil && ok
		}
	}
}

// PeekLine is PeekString over a line's in-memory prefix.
func PeekLine(l *Line, path ...string) (string, bool) {
	return PeekString(bytes.NewReader(l.Prefix()), path...)
}

func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}
