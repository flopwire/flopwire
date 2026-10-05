package devicesync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// SourceSpec describes a source to sync. Path is the device-local key.
type SourceSpec struct {
	Path        string
	Agent       transcript.Agent
	StorageKind transcript.StorageKind
	SessionKey  string
	Parser      string
	// Parent is the path of the source a companion file belongs to (Claude
	// tool-results/*, subagent agent-*.meta.json). Companions sync as their
	// own sources and carry the link.
	Parent string
	// Export marks a source whose bytes come from SyncExport, not a file.
	Export bool
	// Checkout and Remote are the repository the session ran in, as the
	// device agent placed it (syncproto.Source.Checkout). A change is
	// reported to the server with the next flush, header only when no
	// bytes are pending.
	Checkout string `json:",omitempty"`
	Remote   string `json:",omitempty"`
}

// repoKey is what of the spec the server must hear again when it changes:
// "" for no repository, which a source starts with.
func (sp *SourceSpec) repoKey() string {
	if sp.Checkout == "" && sp.Remote == "" {
		return ""
	}
	return sp.Checkout + "\x00" + sp.Remote
}

// rewriteProne reports whether the source rewrites itself whole, so every
// unacknowledged version must be spooled (spec §6.5).
func (sp *SourceSpec) rewriteProne() bool {
	return sp.StorageKind == transcript.StorageJSONDoc || sp.StorageKind == transcript.StorageSQLite
}

// Config tunes a Syncer. Zero fields take defaults.
type Config struct {
	Chunk ChunkParams
	// MaxRequestBytes caps one request's chunk bodies as sent, compressed
	// (syncproto codec.go). A flush that has more (the first sync of a
	// large file, a backlog after an outage) is sent as several requests; steady state is one. The server gives a
	// sync request 30s plus its length at the client's MinRate (64KB/s)
	// to arrive, so any link the client accepts fits; the default is the
	// larger of 4MB and one chunk. A tail that would pass the cap goes in
	// its own request.
	MaxRequestBytes int64
	// HasThreshold: when a request would carry more than this many bytes of
	// chunks the server was never seen to hold, ask /has first. It must be
	// below MaxRequestBytes, which bounds those bytes; default a quarter
	// of it (1MB).
	HasThreshold int64
	// MaxHeldFiles bounds the open descriptors kept on sources with
	// unacknowledged bytes, so a rewrite or unlink can still be salvaged
	// from the old inode. Default 512.
	MaxHeldFiles int
	// SealAfter: a source whose change time is this old has its provisional
	// tail sealed into a final chunk, so finished transcripts end up wholly
	// in object storage. Default 5m; negative disables.
	SealAfter time.Duration
	// Device is reported with every flush: the home directory and harness
	// directories the server needs to apply admin path rules as this
	// device's agent does. Zero reports nothing.
	Device syncproto.DeviceDirs
	// Live, when set, lists the sessions this device's harnesses hold
	// open; every flush reports them (syncproto.FlushHeader.Live).
	Live   func() []string
	Logger *slog.Logger
	Now    func() time.Time
}

func (c *Config) defaults() {
	if c.Chunk == (ChunkParams{}) {
		c.Chunk = DefaultChunkParams
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = max(4<<20, int64(c.Chunk.Max))
	}
	if c.HasThreshold == 0 {
		c.HasThreshold = c.MaxRequestBytes / 4
	}
	if c.MaxHeldFiles == 0 {
		c.MaxHeldFiles = 512
	}
	if c.SealAfter == 0 {
		c.SealAfter = 5 * time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Syncer captures sources into chunked generations and uploads them. It is
// safe for concurrent use; calls are serialized.
type Syncer struct {
	cfg   Config
	store *Store
	spool *Spool
	tr    syncproto.Transport

	mu   sync.Mutex
	held map[int64]*os.File
	buf  []byte
	// zkeep is a body compressed for a request it did not fit: the next
	// request starts with it.
	zkeep part

	refMu   sync.Mutex
	refused map[string]string // path -> the admin path rule the server refused it under
}

// maxRefused bounds the refused sources Status lists.
const maxRefused = 20

// noteRefused records a source the server refused by an admin path rule
// (FlushResponse.Refused).
func (s *Syncer) noteRefused(path, rule string) {
	s.refMu.Lock()
	defer s.refMu.Unlock()
	if s.refused == nil {
		s.refused = map[string]string{}
	}
	s.refused[path] = rule
}

// Refused lists (up to maxRefused, by path) the sources the server
// refused by an admin path rule since the agent started, and how many
// there are.
func (s *Syncer) Refused() ([]SourceRefusal, int) {
	s.refMu.Lock()
	defer s.refMu.Unlock()
	out := make([]SourceRefusal, 0, min(len(s.refused), maxRefused))
	for p, r := range s.refused {
		out = append(out, SourceRefusal{Path: p, Rule: r})
	}
	slices.SortFunc(out, func(a, b SourceRefusal) int { return strings.Compare(a.Path, b.Path) })
	if len(out) > maxRefused {
		out = out[:maxRefused]
	}
	return out, len(s.refused)
}

// SourceRefusal is a source the server refused by an admin path rule.
type SourceRefusal struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// ErrSourceChanged means the source bytes no longer match what was
// captured; the next Sync re-captures them.
var ErrSourceChanged = errors.New("devicesync: source changed since capture")

func NewSyncer(cfg Config, store *Store, spool *Spool, tr syncproto.Transport) (*Syncer, error) {
	cfg.defaults()
	if err := cfg.Chunk.Validate(); err != nil {
		return nil, err
	}
	if cfg.MaxRequestBytes < int64(cfg.Chunk.Max) {
		return nil, errors.New("devicesync: MaxRequestBytes below the chunk maximum")
	}
	s := &Syncer{cfg: cfg, store: store, spool: spool, tr: tr, held: map[int64]*os.File{}, buf: make([]byte, cfg.Chunk.Max)}
	if err := s.sweepSpool(context.Background()); err != nil {
		return nil, fmt.Errorf("devicesync: sweep spool: %w", err)
	}
	return s, nil
}

// sweepSpool drops spool files nothing pending needs: partial writes, and
// chunks or tails spooled by a capture that crashed before it committed.
func (s *Syncer) sweepSpool(ctx context.Context) error {
	return s.spool.sweep(func(h *syncproto.Hash, sid, gen int64) (bool, error) {
		if h != nil {
			ref, err := s.store.referenced(ctx, []syncproto.Hash{*h})
			return ref[*h], err
		}
		return s.store.pendingTail(ctx, sid, gen)
	})
}

// Close releases held descriptors.
func (s *Syncer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, f := range s.held {
		f.Close()
		delete(s.held, id)
	}
}

// Sync captures the file at spec.Path and uploads everything pending for
// it. A transport error leaves the capture persisted: the watermark is the
// queue, and the next Sync resumes from the acknowledged offset.
func (s *Syncer) Sync(ctx context.Context, spec SourceSpec) error {
	return s.run(ctx, spec, nil, -1, nil)
}

// SyncSnapshot captures an immutable staged file under the original source's
// path and identity. The caller must keep snapshot unchanged until this call
// returns. Capture and upload read snapshot, never spec.Path. This allows a
// checksum-verified, disk-backed import without buffering the whole source.
func (s *Syncer) SyncSnapshot(ctx context.Context, spec SourceSpec, snapshot string, identity transcript.Identity) error {
	if snapshot == "" || identity.Size < 0 {
		return errors.New("devicesync: invalid snapshot")
	}
	return s.run(ctx, spec, nil, -1, &snapshotSource{path: snapshot, identity: identity})
}

type snapshotSource struct {
	path     string
	identity transcript.Identity
}

// SyncUpTo is Sync capturing at most the file's first upTo bytes: the
// device agent bounds a transcript to what it has read (a later line may
// name a directory a path rule covers, D18). The rest waits for a later
// sync with a higher bound.
func (s *Syncer) SyncUpTo(ctx context.Context, spec SourceSpec, upTo int64) error {
	return s.run(ctx, spec, nil, upTo, nil)
}

// SyncExport syncs bytes that exist only in memory, such as rows exported
// from a SQLite source (Devin). data is the whole current export; when it
// extends the previous export it appends, otherwise it starts a new
// generation. Every unacknowledged byte is spooled.
func (s *Syncer) SyncExport(ctx context.Context, spec SourceSpec, data []byte) error {
	if data == nil {
		data = []byte{}
	}
	return s.run(ctx, spec, data, -1, nil)
}

// Resume uploads what is pending for a source without capturing it again.
func (s *Syncer) Resume(ctx context.Context, spec SourceSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.store.source(ctx, spec.Path, &spec)
	if err != nil {
		return err
	}
	return s.upload(ctx, src)
}

func (s *Syncer) run(ctx context.Context, spec SourceSpec, export []byte, upTo int64, snapshot *snapshotSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.store.source(ctx, spec.Path, &spec)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		if err := s.capture(ctx, src, export, upTo, snapshot); errors.Is(err, ErrSpoolFull) {
			// Uploading what is pending is what frees the spool.
			if uerr := s.upload(ctx, src); uerr != nil {
				return errors.Join(err, uerr)
			}
			if err := s.capture(ctx, src, export, upTo, snapshot); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		err := s.upload(ctx, src)
		if !errors.Is(err, errRestart) || attempt > 0 {
			return err
		}
	}
}

// capture chunks whatever the source gained since the last capture, up
// to upTo bytes of a file when upTo >= 0.
func (s *Syncer) capture(ctx context.Context, src *sourceRow, export []byte, upTo int64, snapshot *snapshotSource) error {
	var (
		r  io.ReaderAt
		id transcript.Identity
		f  *os.File
	)
	now := s.cfg.Now()
	if export != nil {
		r, id = bytes.NewReader(export), transcript.Identity{Size: int64(len(export))}
	} else {
		var err error
		path := src.Spec.Path
		if snapshot != nil {
			if s.held[src.ID] == nil && len(s.held) >= s.cfg.MaxHeldFiles {
				return errors.New("devicesync: snapshot descriptor limit reached")
			}
			path = snapshot.path
		}
		f, err = os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			if snapshot != nil {
				return err
			}
			return s.vanish(ctx, src)
		} else if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		r, id = f, transcript.IdentityOf(fi)
		if snapshot != nil {
			if !fi.Mode().IsRegular() || fi.Size() != snapshot.identity.Size {
				f.Close()
				return errors.New("devicesync: invalid snapshot")
			}
			id = snapshot.identity
		}
		if upTo >= 0 && id.Size > upTo {
			if w := src.Watermark; w != nil && w.Identity.ID == id.ID && upTo < w.Offset {
				// Captured further before: nothing new within the bound. (A
				// smaller size would read as a truncation.)
				f.Close()
				return nil
			}
			id.Size = upTo
		}
	}
	keep := false
	defer func() {
		if snapshot != nil && f != nil {
			// Also retain on the unchanged/pending path: upload must not fall
			// back to reopening the original source after a retry.
			if old := s.held[src.ID]; old != nil && old != f {
				old.Close()
			}
			s.held[src.ID], keep = f, true
		}
		if f != nil && !keep {
			f.Close()
		}
	}()

	cur, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil {
		return err
	}
	var change transcript.Change
	if export != nil {
		change = decideExport(src.Watermark, export)
	} else if change, err = transcript.Decide(src.Watermark, id, r); err != nil {
		return err
	}
	if cur == nil && change.Decision != transcript.Rewrite {
		change = transcript.Change{Decision: transcript.Rewrite, Reason: "no generation"}
	}
	if export != nil {
		// An export has no file change time: it changed when its bytes last
		// did, so an unchanged export keeps the time of its last change and
		// seals like a quiet file (D21).
		id.CTime = now.UnixNano()
		if change.Decision == transcript.Unchanged && cur.ChangeTime != 0 {
			id.CTime = cur.ChangeTime
		}
	}

	g := cur
	switch change.Decision {
	case transcript.Unchanged:
		if cur.Tail.Size == 0 || !s.idle(id, now) {
			return nil
		}
		// Idle with a captured complete-record tail: fall through to seal it.
	case transcript.Rewrite:
		if cur != nil && !cur.done() {
			s.salvage(ctx, src, cur, change.Reason)
		}
		g = &genRow{SourceID: src.ID, Gen: src.Gen + 1, FileID: fileID(id), TailAcked: true}
		if cur != nil && cur.FileID != g.FileID {
			g.Previous = &syncproto.SourceRef{Path: src.Spec.Path, FileID: cur.FileID}
		} else if cur == nil && export == nil {
			// A file new at this path may be one that moved here: link it
			// to the old path so the server supersedes that copy. Its
			// chunks are known, so only the manifest is sent.
			if g.Previous, err = s.store.renamedFrom(ctx, &src.Spec, g.FileID, gone); err != nil {
				return err
			}
		}
	}
	if p := src.Spec.Parent; p != "" {
		// The companion belongs to the parent as it is now, not as it is
		// when the upload finally runs.
		g.Parent = &syncproto.SourceRef{Path: p}
		if pid, err := transcript.StatIdentity(p); err == nil {
			g.Parent.FileID = fileID(pid)
		}
	}
	g.ChangeTime, g.CapturedAt = id.CTime, now.UnixNano()
	from := g.boundary()
	// Everything uploaded is read through the redactor (notes/redaction.md);
	// r itself stays raw for change detection. An append-only transcript
	// is always captured up to its last newline, even when idle, so a line is
	// never redacted (and uploaded) half-written.
	rr := src.Spec.redacted(r)
	end := id.Size
	if src.Spec.StorageKind == transcript.StorageJSONLAppend && export == nil {
		if end, err = lastLineEnd(r, from, id.Size); err != nil {
			return err
		}
	}
	counted := int64(0)
	if g == cur {
		counted = cur.Size
	} else {
		g.Redactions = nil
	}
	rr.CountFrom(counted)
	if src.Spec.rewriteProne() {
		// Worst case: every new byte is a chunk the spool lacks.
		if err := s.spool.Reserve(end - from); err != nil {
			s.cfg.Logger.Error("devicesync: spool full, not capturing rewritten source", "path", src.Spec.Path, "err", err)
			return err
		}
	}

	var add []syncproto.Entry
	// A rewrite-prone source spools the chunks the server is not known to
	// hold. They are looked up in batches (bounded by count and bytes), not
	// one query per chunk.
	var pend []spooled
	pendBytes := 0
	flush := func() error {
		if len(pend) == 0 {
			return nil
		}
		hs := make([]syncproto.Hash, len(pend))
		for i, p := range pend {
			hs[i] = p.hash
		}
		known, err := s.store.known(ctx, hs)
		if err != nil {
			return err
		}
		for _, p := range pend {
			if !known[p.hash] {
				if err := s.spool.PutChunk(p.hash, p.data); err != nil {
					return err
				}
			}
		}
		pend, pendBytes = pend[:0], 0
		return nil
	}
	tail, err := Scan(s.cfg.Chunk, rr, from, end, s.buf, func(c Chunk, data []byte) error {
		add = append(add, syncproto.Entry{Ordinal: g.Entries + int64(len(add)), Hash: c.Hash, Offset: c.Offset, Size: c.Size})
		if !src.Spec.rewriteProne() {
			return nil
		}
		pend = append(pend, spooled{c.Hash, bytes.Clone(data)})
		if pendBytes += len(data); len(pend) >= batchRows || pendBytes >= spoolBatchBytes {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return fmt.Errorf("devicesync: chunk %s: %w", src.Spec.Path, err)
	}
	newTail := syncproto.Tail{Offset: tail, Size: end - tail}
	if newTail.Size > 0 {
		data := make([]byte, newTail.Size)
		if _, err := rr.ReadAt(data, tail); err != nil && !(errors.Is(err, io.EOF) && tail+newTail.Size == end) {
			return fmt.Errorf("devicesync: read tail %s: %w", src.Spec.Path, err)
		}
		newTail.Hash = syncproto.Sum(data)
		if s.idle(id, now) {
			// Seal: the source went quiet, so its tail becomes a final chunk
			// (shorter than a CDC cut) instead of living in provisional_tails
			// forever. If the file grows again, chunking resumes after it.
			add = append(add, syncproto.Entry{Ordinal: g.Entries + int64(len(add)), Hash: newTail.Hash, Offset: tail, Size: newTail.Size})
			newTail = syncproto.Tail{Offset: end}
			if src.Spec.rewriteProne() {
				if err := s.spool.PutChunk(add[len(add)-1].Hash, data); err != nil {
					return err
				}
			}
		} else if src.Spec.rewriteProne() {
			if err := s.spool.PutTail(src.ID, g.Gen, data); err != nil {
				return err
			}
		}
	}
	if newTail != g.Tail {
		g.Tail, g.TailAcked = newTail, false
	}
	g.Entries += int64(len(add))
	g.Size = end
	for rule, n := range rr.Counts() {
		if g.Redactions == nil {
			g.Redactions = map[string]int64{}
		}
		g.Redactions[rule] += n
	}

	var wm *transcript.Watermark
	if export != nil {
		wm = exportWatermark(export)
	} else {
		w, err := transcript.NewWatermark(r, id, now, transcript.Cursor{Offset: end})
		if err != nil {
			return err
		}
		wm = &w
	}
	if err := s.store.saveCapture(ctx, src, g, add, wm); err != nil {
		return err
	}
	if f != nil && !src.Spec.rewriteProne() && !g.done() {
		if old := s.held[src.ID]; old != nil {
			old.Close()
			delete(s.held, src.ID)
		}
		if len(s.held) < s.cfg.MaxHeldFiles {
			s.held[src.ID], keep = f, true
		}
	}
	return nil
}

// vanish handles a source file that disappeared: salvage what is pending
// from the held descriptor and forget the watermark, so a file that
// reappears at the path starts a new generation.
func (s *Syncer) vanish(ctx context.Context, src *sourceRow) error {
	if src.Watermark == nil {
		return nil
	}
	if g, err := s.store.gen(ctx, src.ID, src.Gen); err != nil {
		return err
	} else if g != nil && !g.done() {
		s.salvage(ctx, src, g, "source disappeared")
	}
	return s.store.setWatermark(ctx, src, nil)
}

// salvage copies a generation's unacknowledged bytes into the spool before
// the source destroys them, reading from the descriptor held since capture
// (which still reaches a replaced or unlinked inode). Bytes that cannot be
// recovered (in-place rewrite, spool full, no held descriptor) are recorded
// as a gap: the generation is cut back to the recoverable prefix.
func (s *Syncer) salvage(ctx context.Context, src *sourceRow, g *genRow, why string) {
	f := s.held[src.ID]
	defer func() {
		if f != nil {
			f.Close()
			delete(s.held, src.ID)
		}
	}()
	var rr *redact.ReaderAt
	if f != nil {
		rr = src.Spec.redacted(f)
	}
	readVerified := func(off, size int64, want syncproto.Hash) ([]byte, bool) {
		if f == nil {
			return nil, false
		}
		data := make([]byte, size)
		if n, err := rr.ReadAt(data, off); n != len(data) || err != nil && !errors.Is(err, io.EOF) || syncproto.Sum(data) != want {
			return nil, false
		}
		return data, true
	}
	keep := g.Entries
	entries, err := s.store.entries(ctx, src.ID, g.Gen, g.Acked, int(g.Entries-g.Acked))
	if err != nil {
		keep = g.Acked
		entries = nil
	}
	known, _ := s.store.known(ctx, entryHashes(entries)) // on error, nothing is known: read every body
	for _, e := range entries {
		if _, ok, _ := s.spool.Chunk(e.Hash); ok {
			continue
		}
		if known[e.Hash] {
			// The server holds this chunk (an earlier version shared it):
			// the entry needs no body. Should the server later report it
			// missing, the upload records the gap then.
			continue
		}
		data, ok := readVerified(e.Offset, e.Size, e.Hash)
		if !ok || s.spool.PutChunk(e.Hash, data) != nil {
			keep = e.Ordinal
			break
		}
	}
	tailOK := g.TailAcked || g.Tail.Size == 0 || keep < g.Entries
	if !tailOK {
		if _, ok, _ := s.spool.Tail(src.ID, g.Gen); ok {
			tailOK = true
		} else if data, ok := readVerified(g.Tail.Offset, g.Tail.Size, g.Tail.Hash); ok && s.spool.PutTail(src.ID, g.Gen, data) == nil {
			tailOK = true
		}
	}
	if keep < g.Entries || !tailOK {
		s.cfg.Logger.Warn("devicesync: unacknowledged bytes lost before upload",
			"path", src.Spec.Path, "generation", g.Gen, "reason", why,
			"kept_entries", keep, "entries", g.Entries, "tail_bytes", g.Tail.Size, "spool_blocked", s.spool.Blocked())
		if err := s.cut(ctx, src, g, keep); err != nil {
			s.cfg.Logger.Error("devicesync: record gap", "path", src.Spec.Path, "err", err)
		}
	}
}

// spooled is a chunk body waiting for the batched known lookup.
type spooled struct {
	hash syncproto.Hash
	data []byte
}

// spoolBatchBytes bounds the chunk bodies capture holds for one batched
// known lookup.
const spoolBatchBytes = 4 << 20

func entryHashes(ents []syncproto.Entry) []syncproto.Hash {
	hs := make([]syncproto.Hash, len(ents))
	for i, e := range ents {
		hs[i] = e.Hash
	}
	return hs
}

// cut records a gap: the generation ends at entry keep, without a tail.
// The recoverable prefix still uploads, and the server's stored tail stays
// as it was: it is the last evidence of the lost bytes. Spooled chunks
// only the cut entries needed are released.
func (s *Syncer) cut(ctx context.Context, src *sourceRow, g *genRow, keep int64) error {
	var dropped []syncproto.Hash
	if keep < g.Entries {
		ents, err := s.store.entries(ctx, src.ID, g.Gen, keep, int(g.Entries-keep))
		if err != nil {
			return err
		}
		for _, e := range ents {
			dropped = append(dropped, e.Hash)
		}
	}
	g.Entries, g.Tail, g.TailAcked = keep, syncproto.Tail{Offset: g.Tail.Offset}, true
	if err := s.store.updateGen(ctx, g, nil); err != nil {
		return err
	}
	s.release(ctx, src, g, dropped)
	s.spool.DropTail(src.ID, g.Gen)
	return nil
}

func gone(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

// provisional reports whether the source at path ends in a provisional
// tail that a later capture would seal. A failed read answers yes: the
// scheduler drops the seal check of a source without a tail, and an
// unsealed tail must never be left without one.
func (s *Syncer) provisional(ctx context.Context, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.store.source(ctx, path, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		return true
	}
	g, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil || g != nil && g.Tail.Size > 0 {
		return true
	}
	// An unfinished JSONL record waits for a file change, not an idle timer.
	return false
}

// idle reports whether the source has been quiet for SealAfter.
func (s *Syncer) idle(id transcript.Identity, now time.Time) bool {
	return s.cfg.SealAfter > 0 && id.CTime != 0 && now.UnixNano()-id.CTime >= int64(s.cfg.SealAfter)
}

func fileID(id transcript.Identity) string {
	if id.ID.IsZero() {
		return ""
	}
	return id.ID.String()
}

// decideExport: an export that extends the previous one appends; any other
// change is a rewrite. The watermark's anchor holds the hash of the whole
// previous export.
func decideExport(prev *transcript.Watermark, data []byte) transcript.Change {
	switch {
	case prev == nil:
		return transcript.Change{Decision: transcript.Rewrite, Reason: "no watermark"}
	case int64(len(data)) < prev.Offset || syncproto.Sum(data[:prev.Offset]) != syncproto.Hash(prev.AnchorSum):
		return transcript.Change{Decision: transcript.Rewrite, Reason: "export changed"}
	case int64(len(data)) == prev.Offset:
		return transcript.Change{Decision: transcript.Unchanged}
	}
	return transcript.Change{Decision: transcript.Append}
}

func exportWatermark(data []byte) *transcript.Watermark {
	return &transcript.Watermark{Offset: int64(len(data)), AnchorLen: int64(len(data)), AnchorSum: syncproto.Sum(data)}
}

// redacted wraps a reader of this source's bytes in the redactor.
func (sp *SourceSpec) redacted(r io.ReaderAt) *redact.ReaderAt {
	return redact.NewReaderAt(r, redact.ModeFor(string(sp.StorageKind), sp.Path))
}

// lastLineEnd returns the offset just past the last newline in [from,
// size), or from when there is none.
func lastLineEnd(r io.ReaderAt, from, size int64) (int64, error) {
	buf := make([]byte, 64<<10)
	for end := size; end > from; {
		lo := max(from, end-int64(len(buf)))
		b := buf[:end-lo]
		if n, err := r.ReadAt(b, lo); n < len(b) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
			return lo + int64(i) + 1, nil
		}
		end = lo
	}
	return from, nil
}
