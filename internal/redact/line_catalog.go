package redact

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"io"
	"math"
	"slices"
)

// LineProof recognizes a record even when some of its masked bytes have
// returned. It stores hashes of the stable prefix and canonical masked record,
// never text. PrefixSize stops before the first masked span.
type LineProof struct {
	Size                 int64
	PrefixSize           int
	PrefixSHA, MaskedSHA [32]byte
}

type LineMask struct {
	SHA         [32]byte
	Spans       []Span
	RedactionID string
	Proof       *LineProof
}
type lineAnchor struct {
	size   int64
	prefix int
	sha    [32]byte
}
type LineCatalog struct {
	exact    map[[32]byte]LineMask
	variants map[lineAnchor][]LineMask
	prefixes []int
}

func NewLineProof(raw []byte, spans []Span) *LineProof {
	raw = bytes.TrimRight(raw, "\r\n")
	spans = lineSpans(spans, int64(len(raw)))
	prefix := min(64, len(raw))
	for _, s := range spans {
		prefix = max(0, min(prefix, s.Start))
	}
	masked := bytes.Clone(raw)
	fillLineSpans(masked, 0, spans)
	return &LineProof{Size: int64(len(raw)), PrefixSize: prefix, PrefixSHA: sha256.Sum256(raw[:prefix]), MaskedSHA: sha256.Sum256(masked)}
}

func NewLineCatalog(records []LineMask) *LineCatalog {
	c := &LineCatalog{exact: map[[32]byte]LineMask{}, variants: map[lineAnchor][]LineMask{}}
	for _, m := range records {
		if m.Proof != nil {
			m.Spans = lineSpans(m.Spans, m.Proof.Size)
		}
		c.exact[m.SHA] = m
		if p := m.Proof; p != nil && p.PrefixSize >= 0 && p.PrefixSize <= 64 && int64(p.PrefixSize) <= p.Size {
			k := lineAnchor{p.Size, p.PrefixSize, p.PrefixSHA}
			c.variants[k] = append(c.variants[k], m)
			if !slices.Contains(c.prefixes, p.PrefixSize) {
				c.prefixes = append(c.prefixes, p.PrefixSize)
			}
		}
	}
	slices.Sort(c.prefixes)
	return c
}

func (c *LineCatalog) Empty() bool { return len(c.exact) == 0 }

// Match reaches a fixed point: masks may reveal a second redaction whose key
// is the partly masked record. Canonical hashes also match mixed old/new chunk
// or tail bytes. Whole-record hashing streams through a bounded buffer.
func (c *LineCatalog) Match(r io.ReaderAt, start, size int64, sum [32]byte, prefix []byte) ([]LineMask, error) {
	var found []LineMask
	var applied []Span
	seen := map[[32]byte]bool{}
	for {
		candidates := []LineMask{}
		if m, ok := c.exact[sum]; ok && !seen[m.SHA] {
			candidates = append(candidates, m)
		}
		for _, n := range c.prefixes {
			if n > len(prefix) {
				continue
			}
			key := lineAnchor{size, n, sha256.Sum256(prefix[:n])}
			for _, m := range c.variants[key] {
				if seen[m.SHA] {
					continue
				}
				combined := append(slices.Clone(applied), m.Spans...)
				normalized, err := maskedLineHash(r, start, size, combined)
				if err != nil {
					return nil, err
				}
				if normalized == m.Proof.MaskedSHA {
					candidates = append(candidates, m)
				}
			}
		}
		if len(candidates) == 0 {
			return found, nil
		}
		for _, m := range candidates {
			if seen[m.SHA] {
				continue
			}
			seen[m.SHA] = true
			found = append(found, m)
			applied = append(applied, m.Spans...)
		}
		var err error
		sum, err = maskedLineHash(r, start, size, applied)
		if err != nil {
			return nil, err
		}
		prefix = make([]byte, min(size, 64))
		if len(prefix) > 0 {
			n, err := r.ReadAt(prefix, start)
			if n < len(prefix) && err != nil {
				return nil, err
			}
			fillLineSpans(prefix, 0, applied)
		}
	}
}

// MatchLine identifies the complete line starting at start without retaining
// its bytes. The caller can apply the returned spans to any segment of that
// line. Read failures propagate so an incomplete identity cannot expose bytes.
func (c *LineCatalog) MatchLine(r io.ReaderAt, start int64) ([]Span, error) {
	if c.Empty() {
		return nil, nil
	}
	br := bufio.NewReaderSize(io.NewSectionReader(r, start, math.MaxInt64-start), 64<<10)
	h := sha256.New()
	var size, pendingCR int64
	var prefix []byte
	crs := bytes.Repeat([]byte{'\r'}, 1024)
	for {
		part, err := br.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
			return nil, err
		}
		data := bytes.TrimSuffix(part, []byte{'\n'})
		if len(prefix) < 64 {
			prefix = append(prefix, data[:min(len(data), 64-len(prefix))]...)
		}
		size += int64(len(data))
		trim := bytes.TrimRight(data, "\r")
		if len(trim) > 0 {
			for pendingCR > 0 {
				n := min(pendingCR, int64(len(crs)))
				h.Write(crs[:n])
				pendingCR -= n
			}
			h.Write(trim)
		}
		pendingCR += int64(len(data) - len(trim))
		if err == bufio.ErrBufferFull {
			continue
		}
		size -= pendingCR
		var sum [32]byte
		copy(sum[:], h.Sum(nil))
		records, err := c.Match(r, start, size, sum, prefix[:min(int64(len(prefix)), size)])
		if err != nil {
			return nil, err
		}
		var spans []Span
		for _, m := range records {
			spans = append(spans, lineSpans(m.Spans, size)...)
		}
		return spans, nil
	}
}

func (c *LineCatalog) MatchBytes(raw []byte) []Span {
	raw = bytes.TrimRight(raw, "\r\n")
	records, err := c.Match(bytes.NewReader(raw), 0, int64(len(raw)), LineSum(raw), raw[:min(len(raw), 64)])
	if err != nil {
		return nil
	}
	var out []Span
	for _, m := range records {
		out = append(out, lineSpans(m.Spans, int64(len(raw)))...)
	}
	return out
}

func maskedLineHash(r io.ReaderAt, start, size int64, spans []Span) ([32]byte, error) {
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for off := int64(0); off < size; {
		b := buf[:min(int64(len(buf)), size-off)]
		n, err := r.ReadAt(b, start+off)
		if n < len(b) && err != nil {
			return [32]byte{}, err
		}
		fillLineSpans(b, off, spans)
		h.Write(b)
		off += int64(len(b))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func fillLineSpans(b []byte, off int64, spans []Span) {
	for _, s := range spans {
		lo, hi := max(off, int64(s.Start)), min(off+int64(len(b)), int64(s.End))
		if lo >= hi {
			continue
		}
		marker := "[REDACTED:message]"
		if len(marker) > s.End-s.Start {
			marker = "[REDACTED]"
		}
		if len(marker) > s.End-s.Start {
			marker = ""
		}
		for p := lo; p < hi; p++ {
			i := p - int64(s.Start)
			b[p-off] = '*'
			if i < int64(len(marker)) {
				b[p-off] = marker[i]
			}
		}
	}
}

// Terminators are excluded from record identity and must remain structural bytes.
func lineSpans(spans []Span, size int64) []Span {
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		s.Start = max(0, s.Start)
		s.End = int(min(int64(s.End), size))
		if s.Start < s.End {
			out = append(out, s)
		}
	}
	return out
}
