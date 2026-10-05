package devicesync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// errRestart: the server wants the current generation re-sent as a new one.
var errRestart = errors.New("devicesync: restart as a new generation")

// upload sends every pending generation of src, oldest first.
func (s *Syncer) upload(ctx context.Context, src *sourceRow) error {
	gens, err := s.store.pendingGens(ctx, src.ID)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if err := s.uploadGen(ctx, src, g); err != nil {
			return err
		}
	}
	if f := s.held[src.ID]; f != nil {
		f.Close()
		delete(s.held, src.ID)
	}
	if src.RepoSent == src.Spec.repoKey() || src.Gen < 0 {
		return nil
	}
	if len(gens) == 0 {
		// Every byte is acknowledged, and the server has not heard the
		// session's repository (placed or recovered since): tell it, header
		// only.
		if err := s.sendRepo(ctx, src); err != nil {
			return err
		}
	}
	return s.store.repoSent(ctx, src)
}

// sendRepo reports src's repository with a flush of its current
// generation that carries no entries and no tail: the server updates the
// source's row and leaves its bytes as they are.
func (s *Syncer) sendRepo(ctx context.Context, src *sourceRow) error {
	g, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil || g == nil {
		return err
	}
	h := syncproto.FlushHeader{
		Version:    syncproto.Version,
		Source:     s.describe(src, g),
		Generation: g.Gen,
		CapturedAt: time.Unix(0, g.CapturedAt).UTC(),
		Chunker:    s.cfg.Chunk.wire(),
	}
	if g.ChangeTime != 0 {
		h.ChangeTime = time.Unix(0, g.ChangeTime).UTC()
	}
	resp, err := s.tr.Flush(ctx, &syncproto.FlushRequest{Header: h})
	if err == nil && resp.Refused != "" {
		s.noteRefused(src.Spec.Path, resp.Refused)
	}
	return err
}

func (s *Syncer) uploadGen(ctx context.Context, src *sourceRow, g *genRow) error {
	for stalls := 0; !g.done(); {
		batch, bodies, err := s.nextBatch(ctx, g)
		if err != nil {
			return err
		}
		// Compress the bodies, cutting the batch where the compressed bytes
		// would pass MaxRequestBytes; then read the tail if it fits.
		pl := &payload{s: s, src: src, g: g, failed: -1}
		batch = pl.pack(batch, bodies)
		var tail *syncproto.Tail
		last := pl.err == nil && g.Acked+int64(len(batch)) == g.Entries
		if last && g.Tail.Size > 0 {
			if len(batch) > 0 && pl.wire+g.Tail.Size > s.cfg.MaxRequestBytes {
				last = false // the tail goes in the next request
			} else {
				t := g.Tail
				if g.SrvTailOff == t.Offset && g.SrvTailLen > 0 && g.SrvTailLen < t.Size {
					t.From = g.SrvTailLen // the server holds the start of this tail
				}
				tail = &t
				pl.loadTail(tail)
			}
		}
		var resp *syncproto.FlushResponse
		if pl.err == nil {
			h := syncproto.FlushHeader{
				Version:    syncproto.Version,
				Source:     s.describe(src, g),
				Generation: g.Gen,
				CapturedAt: time.Unix(0, g.CapturedAt).UTC(),
				Chunker:    s.cfg.Chunk.wire(),
				Entries:    batch,
				Tail:       tail,
				Redaction:  &syncproto.Redaction{Rules: redact.RulesVersion, Counts: g.Redactions},
			}
			if s.cfg.Device != (syncproto.DeviceDirs{}) {
				h.Device = &s.cfg.Device
			}
			if s.cfg.Live != nil {
				h.Live = append([]string{}, s.cfg.Live()...)
				if len(h.Live) > syncproto.MaxLive {
					h.Live = h.Live[:syncproto.MaxLive]
				}
			}
			if g.ChangeTime != 0 {
				h.ChangeTime = time.Unix(0, g.ChangeTime).UTC()
			}
			for _, p := range pl.parts {
				h.Bodies = append(h.Bodies, syncproto.Body{Hash: p.e.Hash, Size: p.e.Size, ZSize: int64(len(p.z))})
			}
			resp, err = s.tr.Flush(ctx, &syncproto.FlushRequest{Header: h, Payload: pl})
		}
		pl.close()
		if pl.err != nil {
			if pl.failed >= 0 && (g.Gen != src.Gen || s.vanished(src)) {
				// An older generation, or the current one of a file that
				// is gone: no capture will re-read these bytes, and the
				// spool lacks them (the server reported a chunk missing
				// that it was known to hold). Record the gap so the rest
				// of the source is not held up forever.
				s.cfg.Logger.Warn("devicesync: unacknowledged bytes lost before upload",
					"path", src.Spec.Path, "generation", g.Gen, "reason", pl.err, "kept_entries", pl.failed, "entries", g.Entries)
				if err := s.cut(ctx, src, g, pl.failed); err != nil {
					return err
				}
				continue
			}
			// The bytes under the watermark moved: force a new generation.
			if werr := s.store.setWatermark(ctx, src, nil); werr != nil {
				return werr
			}
			return pl.err
		}
		if err != nil {
			return err
		}

		switch resp.Status {
		case syncproto.StatusStaleGeneration, syncproto.StatusNewGeneration:
			s.cfg.Logger.Warn("devicesync: server rejected generation", "path", src.Spec.Path,
				"generation", g.Gen, "status", resp.Status, "server_generation", resp.Generation)
			g.Lost = true
			if err := s.store.updateGen(ctx, g, nil); err != nil {
				return err
			}
			if lost, err := s.store.entries(ctx, src.ID, g.Gen, g.Acked, int(g.Entries-g.Acked)); err == nil {
				hs := make([]syncproto.Hash, len(lost))
				for i, e := range lost {
					hs[i] = e.Hash
				}
				s.release(ctx, src, g, hs)
			}
			s.spool.DropTail(src.ID, g.Gen)
			if g.Gen != src.Gen {
				return nil // an older generation: the current one carries on
			}
			src.Gen = max(src.Gen, resp.Generation)
			if err := s.store.setWatermark(ctx, src, nil); err != nil {
				return err
			}
			return errRestart
		case syncproto.StatusOK, syncproto.StatusPartial:
			if resp.Refused != "" {
				s.noteRefused(src.Spec.Path, resp.Refused)
			}
		default:
			return fmt.Errorf("devicesync: unknown flush status %q", resp.Status)
		}

		prev := g.Acked
		g.Acked = min(max(resp.AckedEntries, 0), g.Entries)
		if last {
			g.TailAcked = resp.TailAcked && g.Acked == g.Entries
		}
		g.SrvTailOff, g.SrvTailLen = resp.TailOffset, resp.TailSize
		var acked []syncproto.Hash
		for _, e := range batch {
			if e.Ordinal >= prev && e.Ordinal < g.Acked {
				acked = append(acked, e.Hash)
			}
		}
		if err := s.store.forget(ctx, resp.Missing); err != nil {
			return err
		}
		if err := s.store.updateGen(ctx, g, acked); err != nil {
			return err
		}
		s.release(ctx, src, g, acked)
		if g.Acked == prev && !(last && g.TailAcked) {
			if stalls++; stalls > 2 {
				return fmt.Errorf("devicesync: %s generation %d: server made no progress (status %s, acked %d of %d)",
					src.Spec.Path, g.Gen, resp.Status, g.Acked, g.Entries)
			}
		}
	}
	return nil
}

// nextBatch picks the next entries to send, bounded by batchRatio times
// MaxRequestBytes of uncompressed chunk bodies (always at least one
// entry), and the bodies the server is not known to hold, asking /has when
// those exceed HasThreshold. payload.pack then cuts the batch where the
// compressed bodies reach MaxRequestBytes.
func (s *Syncer) nextBatch(ctx context.Context, g *genRow) ([]syncproto.Entry, []syncproto.Entry, error) {
	// Bounded by Entries: a gap cut leaves manifest rows past the cut.
	ents, err := s.store.entries(ctx, g.SourceID, g.Gen, g.Acked, int(min(g.Entries-g.Acked, 4096)))
	if err != nil {
		return nil, nil, err
	}
	var batch, bodies []syncproto.Entry
	seen := map[syncproto.Hash]bool{}
	var size int64
	known, err := s.store.known(ctx, entryHashes(ents))
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		need := !known[e.Hash] && !seen[e.Hash]
		if len(batch) > 0 && need && size+e.Size > batchRatio*s.cfg.MaxRequestBytes {
			break
		}
		batch = append(batch, e)
		if need {
			seen[e.Hash] = true
			bodies = append(bodies, e)
			size += e.Size
		}
	}
	if size <= s.cfg.HasThreshold {
		return batch, bodies, nil
	}
	hs := make([]syncproto.Hash, len(bodies))
	for i, b := range bodies {
		hs[i] = b.Hash
	}
	missing, err := s.tr.Has(ctx, hs)
	if err != nil {
		return nil, nil, err
	}
	want := map[syncproto.Hash]bool{}
	for _, h := range missing {
		want[h] = true
	}
	var keep []syncproto.Entry
	var present []syncproto.Hash
	for _, b := range bodies {
		if want[b.Hash] {
			keep = append(keep, b)
		} else {
			present = append(present, b.Hash)
		}
	}
	return batch, keep, s.store.remember(ctx, present)
}

// release drops spooled bytes nothing pending needs any more.
func (s *Syncer) release(ctx context.Context, src *sourceRow, g *genRow, acked []syncproto.Hash) {
	var spooled []syncproto.Hash
	for _, h := range acked {
		if _, ok, _ := s.spool.Chunk(h); ok {
			spooled = append(spooled, h)
		}
	}
	if ref, err := s.store.referenced(ctx, spooled); err == nil {
		for _, h := range spooled {
			if !ref[h] {
				s.spool.DropChunk(h)
			}
		}
	}
	if g.TailAcked && !(src.ExportState != nil && g.Gen == src.Gen && g.Tail.Size > 0) {
		s.spool.DropTail(src.ID, g.Gen) // an appending export's current tail is kept (keepTail)
	}
}

// vanished reports whether src's file disappeared (vanish forgot its
// watermark) and has not come back: its current generation is final.
func (s *Syncer) vanished(src *sourceRow) bool {
	return !src.Spec.Export && src.Watermark == nil && gone(src.Spec.Path)
}

func (s *Syncer) describe(src *sourceRow, g *genRow) syncproto.Source {
	d := syncproto.Source{
		Path:        src.Spec.Path,
		FileID:      g.FileID,
		Agent:       string(src.Spec.Agent),
		StorageKind: string(src.Spec.StorageKind),
		SessionKey:  src.Spec.SessionKey,
		Parser:      src.Spec.Parser,
		Previous:    g.Previous,
		Parent:      g.Parent,
		Checkout:    src.Spec.Checkout,
		Remote:      src.Spec.Remote,
	}
	return d
}

func bodySize(bs []syncproto.Entry) int64 {
	var n int64
	for _, b := range bs {
		n += b.Size
	}
	return n
}

// batchRatio bounds the uncompressed bodies nextBatch offers per request
// to this many times MaxRequestBytes: transcripts compress about 5-10x,
// so a request of compressed bodies can still reach the cap.
const batchRatio = 8

// payload holds a request's compressed chunk bodies and tail, read from
// the spool or else the source and verified against their hashes, and
// streams them. Compressed bodies stay under MaxRequestBytes (pack), and
// one uncompressed chunk is in memory at a time.
type payload struct {
	s     *Syncer
	src   *sourceRow
	g     *genRow
	parts []part
	wire  int64 // compressed body bytes
	tail  []byte
	cur   []byte
	next  int
	f     *os.File
	rr    *redact.ReaderAt
	open  bool
	err   error
	// failed is where the unreadable part starts: the ordinal of the first
	// entry that needs it, or g.Entries for the tail; -1 while none.
	failed int64
}

type part struct {
	e syncproto.Entry
	z []byte
}

// pack compresses bodies in order until the next would take the
// compressed total past MaxRequestBytes (the first is always taken) and
// returns batch cut before the first entry whose body was left out. On a
// read failure it sets err and failed.
func (p *payload) pack(batch, bodies []syncproto.Entry) []syncproto.Entry {
	keep := p.s.zkeep
	p.s.zkeep = part{}
	for i, e := range bodies {
		var z []byte
		var err error
		if i == 0 && keep.z != nil && keep.e.Hash == e.Hash {
			z = keep.z
		} else {
			z, err = p.compressed(e)
		}
		if err != nil {
			p.err, p.failed = err, e.Ordinal
			return batch
		}
		if i > 0 && p.wire+int64(len(z)) > p.s.cfg.MaxRequestBytes {
			p.s.zkeep = part{e, z} // the next request starts with it
			for j, b := range batch {
				if b.Ordinal >= e.Ordinal {
					return batch[:j]
				}
			}
			return batch
		}
		p.parts = append(p.parts, part{e, z})
		p.wire += int64(len(z))
	}
	return batch
}

// compressed returns the zstd frame of chunk e.
func (p *payload) compressed(e syncproto.Entry) ([]byte, error) {
	data, err := p.read(p.s.buf, e.Offset, e.Size, e.Hash, func() ([]byte, bool, error) { return p.s.spool.Chunk(e.Hash) })
	if err != nil {
		return nil, err
	}
	return syncproto.Compress(nil, data), nil
}

// loadTail reads and verifies the tail and keeps the bytes from t.From.
func (p *payload) loadTail(t *syncproto.Tail) {
	data, err := p.read(nil, t.Offset, t.Size, t.Hash, func() ([]byte, bool, error) { return p.s.spool.Tail(p.src.ID, p.g.Gen) })
	if err != nil {
		p.err, p.failed = err, p.g.Entries
		return
	}
	p.tail = data[t.From:]
}

// read returns size bytes at off of the source, from the spool when it
// holds them (else read into buf, grown as needed), verified against want.
func (p *payload) read(buf []byte, off, size int64, want syncproto.Hash, spooled func() ([]byte, bool, error)) ([]byte, error) {
	data, ok, _ := spooled()
	if !ok {
		data = buf[:0]
		if int64(cap(data)) < size {
			data = make([]byte, size)
		}
		data = data[:size]
		if err := p.readSource(data, off); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrSourceChanged, p.src.Spec.Path, err)
		}
	}
	if int64(len(data)) != size || syncproto.Sum(data) != want {
		return nil, fmt.Errorf("%w: %s at offset %d", ErrSourceChanged, p.src.Spec.Path, off)
	}
	return data, nil
}

func (p *payload) Read(b []byte) (int, error) {
	for len(p.cur) == 0 {
		switch {
		case p.next < len(p.parts):
			p.cur = p.parts[p.next].z
		case p.next == len(p.parts) && p.tail != nil:
			p.cur = p.tail
		default:
			return 0, io.EOF
		}
		p.next++
	}
	n := copy(b, p.cur)
	p.cur = p.cur[n:]
	return n, nil
}

func (p *payload) readSource(data []byte, off int64) error {
	if !p.open {
		p.open = true
		if f := p.s.held[p.src.ID]; f != nil {
			p.f = f
		} else if f, err := os.Open(p.src.Spec.Path); err == nil {
			p.f = f
		}
	}
	if p.f == nil {
		return errors.New("source not readable")
	}
	if p.rr == nil {
		p.rr = p.src.Spec.redacted(p.f)
	}
	n, err := p.rr.ReadAt(data, off)
	if n == len(data) && errors.Is(err, io.EOF) {
		err = nil
	}
	return err
}

func (p *payload) close() {
	if p.f != nil && p.f != p.s.held[p.src.ID] {
		p.f.Close()
	}
}
