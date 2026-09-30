// Append-versus-rewrite detection for append-mostly sources (spec §6.1-6.2).
//
// Design informed by kenn-io/agentsview internal/sync/checkpoint.go (stat
// identity + size + change time gate, tail anchor digest before resuming) at
// commit 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT), and by git's racy
// index rule. No code copied.

package transcript

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// HeadHashLen bounds the head hash: the first 4KB of the indexed prefix.
	HeadHashLen = 4 << 10
	// AnchorLen bounds the anchor: the bytes just before the resume offset.
	// It covers the whole last line unless that line is longer.
	AnchorLen = 64 << 10
	// RacyWindow: a change time this close to (or after) the moment the
	// identity was sampled may hide a later write in the same timestamp tick
	// (coarse kernel clocks, 1s HFS+ and 2s FAT resolution).
	RacyWindow = 2 * time.Second
)

// Identity is the sweep gate tuple: file identity, size and change time.
// CTime is the inode ctime in nanoseconds, which no user-space writer can
// restore; zero means unknown. Never gate on mtime.
type Identity struct {
	ID    FileID
	Size  int64
	CTime int64
}

// IdentityOf extracts the gate tuple from a stat result.
func IdentityOf(fi os.FileInfo) Identity {
	id, ctime, _ := statIdentity(fi)
	return Identity{ID: id, Size: fi.Size(), CTime: ctime}
}

// StatIdentity stats path and returns its gate tuple.
func StatIdentity(path string) (Identity, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Identity{}, err
	}
	return IdentityOf(fi), nil
}

// Watermark is the per-source state saved after a parse (spec §6.2).
type Watermark struct {
	Identity  Identity // sampled before the parse read the file
	SampledAt int64    // wall clock (ns) when Identity was sampled
	Offset    int64    // resume offset: end of the last complete line
	LineNo    int64
	HeadLen   int64 // bytes [0, HeadLen) hash to HeadHash
	HeadHash  [32]byte
	AnchorLen int64 // bytes [Offset-AnchorLen, Offset) hash to AnchorHash
	AnchorSum [32]byte
}

// NewWatermark records the state after parsing r up to cur. id and
// sampledAt must be taken before the parse started reading.
func NewWatermark(r io.ReaderAt, id Identity, sampledAt time.Time, cur Cursor) (Watermark, error) {
	w := Watermark{Identity: id, SampledAt: sampledAt.UnixNano(), Offset: cur.Offset, LineNo: cur.LineNo}
	w.HeadLen = min(HeadHashLen, cur.Offset)
	w.AnchorLen = min(AnchorLen, cur.Offset)
	var err error
	if w.HeadHash, err = hashRange(r, 0, w.HeadLen); err != nil {
		return Watermark{}, err
	}
	if w.AnchorSum, err = hashRange(r, cur.Offset-w.AnchorLen, w.AnchorLen); err != nil {
		return Watermark{}, err
	}
	return w, nil
}

// Racy reports whether the saved change time is too close to the sample
// time to trust an unchanged tuple.
func (w *Watermark) Racy() bool {
	return w.Identity.CTime != 0 && w.SampledAt-w.Identity.CTime < int64(RacyWindow)
}

// Decision is what to do with a source after a sweep.
type Decision int

const (
	Unchanged Decision = iota // nothing to read
	Append                    // resume at the watermark offset
	Rewrite                   // full reparse as a new generation
)

func (d Decision) String() string {
	return [...]string{"unchanged", "append", "rewrite"}[d]
}

// Change is Decide's verdict and why.
type Change struct {
	Decision Decision
	Reason   string
	// Verified is true when Decide read the head and anchor. After a
	// verified Unchanged, re-save the watermark with a fresh SampledAt so
	// a racy entry stops being racy.
	Verified bool
}

// Decide compares the saved watermark with the current identity of the file
// behind r. It reads at most HeadHashLen+AnchorLen bytes, and nothing when
// the tuple is unchanged and not racy.
//
// Rewrite when: there is no watermark; the identity changed; the file is
// shorter than the resume offset; the first-4KB hash changed; the anchor
// before the offset changed; or the change time moved without the size
// changing (a same-size rewrite). Otherwise Append if the size changed,
// Unchanged if not.
//
// Limit: an in-place edit beyond the head and anchor windows followed by an
// append looks like an append. The harness cases the spec lists (failed
// streaming message removed at the tail, compaction cleanup rewriting the
// head, truncation, new inode) all hit a check.
func Decide(prev *Watermark, cur Identity, r io.ReaderAt) (Change, error) {
	switch {
	case prev == nil:
		return Change{Decision: Rewrite, Reason: "no watermark"}, nil
	case prev.Identity.ID != cur.ID:
		return Change{Decision: Rewrite, Reason: "file identity changed"}, nil
	case cur.Size < prev.Offset:
		return Change{Decision: Rewrite, Reason: fmt.Sprintf("size %d below offset %d", cur.Size, prev.Offset)}, nil
	}
	sameTuple := cur.Size == prev.Identity.Size && cur.CTime == prev.Identity.CTime
	if sameTuple && !prev.Racy() {
		return Change{Decision: Unchanged, Reason: "tuple unchanged"}, nil
	}

	head, err := hashRange(r, 0, prev.HeadLen)
	if errors.Is(err, errShortRead) {
		return Change{Decision: Rewrite, Reason: "shrank during check"}, nil
	} else if err != nil {
		return Change{}, err
	}
	if head != prev.HeadHash {
		return Change{Decision: Rewrite, Reason: "first 4KB changed", Verified: true}, nil
	}
	anchor, err := hashRange(r, prev.Offset-prev.AnchorLen, prev.AnchorLen)
	if errors.Is(err, errShortRead) {
		return Change{Decision: Rewrite, Reason: "shrank during check"}, nil
	} else if err != nil {
		return Change{}, err
	}
	if anchor != prev.AnchorSum {
		return Change{Decision: Rewrite, Reason: "bytes before offset changed", Verified: true}, nil
	}
	if cur.Size == prev.Identity.Size {
		if sameTuple {
			return Change{Decision: Unchanged, Reason: "racy tuple verified", Verified: true}, nil
		}
		return Change{Decision: Rewrite, Reason: "changed without growing", Verified: true}, nil
	}
	return Change{Decision: Append, Reason: "grew", Verified: true}, nil
}

// errShortRead means the file shrank between stat and read.
var errShortRead = errors.New("transcript: file shorter than watermark")

func hashRange(r io.ReaderAt, off, n int64) ([32]byte, error) {
	h := sha256.New()
	if n > 0 {
		got, err := io.Copy(h, io.NewSectionReader(r, off, n))
		if err != nil {
			return [32]byte{}, err
		}
		if got != n {
			return [32]byte{}, errShortRead
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// ReadRecorder wraps the reader a parse reads from and keeps the bytes a
// Watermark hashes, so the watermark describes the bytes the parser
// consumed, not the file as it is after the parse. A rewrite that lands
// while a parse runs then leaves a watermark that no longer matches the
// file, and the next Decide sees the rewrite.
//
// NewReadRecorder primes it with the head and with the anchor window just
// before the resume offset, then the parse's own reads replace those bytes
// as they come: the head from any read of it, the anchor from the
// sequential stream the line reader consumes. Re-reads of earlier bytes
// (an oversized line opened again) overwrite them in place.
type ReadRecorder struct {
	r io.ReaderAt

	mu       sync.Mutex
	head     [HeadHashLen]byte
	headN    int64 // head[:headN] is recorded
	win      []byte
	winStart int64
}

// recordWindow bounds the recorded tail of the stream: the anchor plus the
// line reader's read-ahead, with room to spare.
const recordWindow = 4 * AnchorLen

var _ io.ReaderAt = (*ReadRecorder)(nil)

// NewReadRecorder wraps r for a parse that resumes at offset start.
func NewReadRecorder(r io.ReaderAt, start int64) (*ReadRecorder, error) {
	rr := &ReadRecorder{r: r, winStart: max(0, start-AnchorLen)}
	head := make([]byte, HeadHashLen)
	if _, err := rr.ReadAt(head, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n := start - rr.winStart; n > 0 {
		pre := make([]byte, n)
		if _, err := rr.ReadAt(pre, rr.winStart); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	return rr, nil
}

// ReadAt reads from the wrapped reader and records what it returned.
func (rr *ReadRecorder) ReadAt(p []byte, off int64) (int, error) {
	n, err := rr.r.ReadAt(p, off)
	if n > 0 {
		rr.mu.Lock()
		rr.record(p[:n], off)
		rr.mu.Unlock()
	}
	return n, err
}

func (rr *ReadRecorder) record(b []byte, off int64) {
	if off <= rr.headN && off < HeadHashLen {
		end := min(off+int64(len(b)), HeadHashLen)
		copy(rr.head[off:end], b)
		rr.headN = max(rr.headN, end)
	}
	winEnd := rr.winStart + int64(len(rr.win))
	end := off + int64(len(b))
	switch {
	case len(rr.win) == 0 || off > winEnd:
		// First read, or a gap: the stream starts over here.
		rr.win, rr.winStart = append(rr.win[:0], b...), off
	case end <= rr.winStart:
		return // bytes before the window: nothing the anchor needs
	default:
		if off < rr.winStart {
			b, off = b[rr.winStart-off:], rr.winStart
		}
		at := off - rr.winStart
		k := copy(rr.win[at:], b)
		rr.win = append(rr.win, b[k:]...)
	}
	if extra := len(rr.win) - recordWindow; extra > 0 {
		rr.win = append(rr.win[:0], rr.win[extra:]...)
		rr.winStart += int64(extra)
	}
}

// Watermark is NewWatermark over the recorded bytes. A range the parse did
// not read (it cannot happen for a parse that started at the offset the
// recorder was primed with) is read from the file instead.
func (rr *ReadRecorder) Watermark(id Identity, sampledAt time.Time, cur Cursor) (Watermark, error) {
	w := Watermark{Identity: id, SampledAt: sampledAt.UnixNano(), Offset: cur.Offset, LineNo: cur.LineNo}
	w.HeadLen = min(HeadHashLen, cur.Offset)
	w.AnchorLen = min(AnchorLen, cur.Offset)
	rr.mu.Lock()
	defer rr.mu.Unlock()
	var err error
	if w.HeadLen <= rr.headN {
		w.HeadHash = sha256.Sum256(rr.head[:w.HeadLen])
	} else if w.HeadHash, err = hashRange(rr.r, 0, w.HeadLen); err != nil {
		return Watermark{}, err
	}
	from := cur.Offset - w.AnchorLen
	if from >= rr.winStart && cur.Offset <= rr.winStart+int64(len(rr.win)) {
		w.AnchorSum = sha256.Sum256(rr.win[from-rr.winStart : cur.Offset-rr.winStart])
	} else if w.AnchorSum, err = hashRange(rr.r, from, w.AnchorLen); err != nil {
		return Watermark{}, err
	}
	return w, nil
}
