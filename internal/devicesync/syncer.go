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
	"sync/atomic"
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

	mu            sync.Mutex
	held          map[int64]*os.File
	serialScratch syncScratch
	stalls        map[[2]int64]int // source/generation no-progress responses across scheduler turns

	refMu   sync.Mutex
	refused map[string]string // path -> the admin path rule the server refused it under

	captures, exported, scanned atomic.Int64 // CaptureStats
}

// CaptureStats counts the work of captures since the syncer started.
type CaptureStats struct {
	Captures int64 // capture passes that finished
	Exported int64 // bytes export functions returned
	Scanned  int64 // bytes read through the redactor and chunked
}

// CaptureStats returns the work captures did so far.
func (s *Syncer) CaptureStats() CaptureStats {
	return CaptureStats{Captures: s.captures.Load(), Exported: s.exported.Load(), Scanned: s.scanned.Load()}
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
	s := &Syncer{cfg: cfg, store: store, spool: spool, tr: tr, held: map[int64]*os.File{}, serialScratch: syncScratch{buf: make([]byte, cfg.Chunk.Max)}}
	if err := s.sweepSpool(context.Background()); err != nil {
		return nil, fmt.Errorf("devicesync: sweep spool: %w", err)
	}
	return s, nil
}

// sweepSpool drops spool files nothing pending needs: partial writes, and
// chunks or tails spooled by a capture that crashed before it committed.
func (s *Syncer) sweepSpool(ctx context.Context) error {
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return scope.sweep(ctx)
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
	return s.run(ctx, spec, nil, -1, nil, nil)
}

// SyncSnapshot captures an immutable staged file under the original source's
// path and identity. The caller must keep snapshot unchanged until this call
// returns. Capture and upload read snapshot, never spec.Path. This allows a
// checksum-verified, disk-backed import without buffering the whole source.
func (s *Syncer) SyncSnapshot(ctx context.Context, spec SourceSpec, snapshot string, identity transcript.Identity) error {
	if snapshot == "" || identity.Size < 0 {
		return errors.New("devicesync: invalid snapshot")
	}
	return s.run(ctx, spec, nil, -1, &snapshotSource{path: snapshot, identity: identity}, nil)
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
	return s.run(ctx, spec, nil, upTo, nil, nil)
}

// SyncExport syncs bytes that exist only in memory, such as rows exported
// from a SQLite source (opencode). data is the whole current export; when
// it extends the previous export it appends, otherwise it starts a new
// generation. Every unacknowledged byte is spooled.
func (s *Syncer) SyncExport(ctx context.Context, spec SourceSpec, data []byte) error {
	if data == nil {
		data = []byte{}
	}
	return s.run(ctx, spec, func(context.Context, []byte) (Export, error) { return Export{Data: data}, nil }, -1, nil, nil)
}

// Export is one version of an export source's bytes (SyncExportFunc).
type Export struct {
	// Data is the whole export, or with Append what the export gained
	// since the one the prev state describes.
	Data   []byte
	Append bool
	// State is what the exporter resumes from after Data. It is saved
	// with the capture and passed back as prev.
	State []byte
}

// ExportFunc produces an export source's bytes. prev is the State saved
// with the source's last capture, nil when there is none (a first sync, a
// new generation): the function must then return the whole export. With
// prev it may return an append; when the syncer cannot use the append
// (the generation it extends ended, or the provisional tail is gone) it
// asks again with nil. An append must end a line, like the export it
// extends: the syncer redacts it on its own, which is the whole export's
// redaction only from a line start.
type ExportFunc func(ctx context.Context, prev []byte) (Export, error)

// SyncExportFunc syncs an export produced by fn. An exporter that appends
// (Devin's devin-export@2) costs what the source gained since the last
// capture: only the appended bytes, and the provisional tail before them,
// are redacted and chunked.
func (s *Syncer) SyncExportFunc(ctx context.Context, spec SourceSpec, fn ExportFunc) error {
	return s.run(ctx, spec, fn, -1, nil, nil)
}

// Resume uploads what is pending for a source without capturing it again.
func (s *Syncer) Resume(ctx context.Context, spec SourceSpec) error { return s.resume(ctx, spec, nil) }
func (s *Syncer) resume(ctx context.Context, spec SourceSpec, auth *CaptureAuthorization) error {
	_, err := s.operation(auth).resumeTurn(ctx, spec, nil)
	return err
}

func (op *syncOperation) resumeTurn(ctx context.Context, spec SourceSpec, turn *uploadTurn) (syncOutcome, error) {
	s := op.syncer
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.store.source(ctx, spec.Path, &spec)
	if err != nil {
		return syncDone, err
	}
	if err := op.validateAuthorization(src); err != nil {
		return syncDone, err
	}
	if err := op.protect(ctx, src); err != nil {
		return syncDone, err
	}
	if err := op.preflight(ctx, src, false); err != nil {
		return syncDone, err
	}
	return op.uploadTurn(ctx, src, turn)
}

func (s *Syncer) run(ctx context.Context, spec SourceSpec, export ExportFunc, upTo int64, snapshot *snapshotSource, auth *CaptureAuthorization) error {
	_, err := s.operation(auth).runTurn(ctx, spec, export, upTo, snapshot, nil)
	return err
}

// syncTurn is scheduler-only. Public Sync and import calls still drain fully.
func (s *Syncer) syncTurn(ctx context.Context, spec SourceSpec, export ExportFunc, upTo int64, auth *CaptureAuthorization, action syncAction) (syncOutcome, error) {
	if auth != nil {
		if spec.Export {
			return syncDone, errors.New("devicesync: authorized file capture cannot export")
		}
		a, err := freezeAuthorization(auth)
		if err != nil {
			return syncDone, err
		}
		auth, upTo = a, a.Proof.Offset
	}
	op := s.operation(auth)
	turn := &uploadTurn{remaining: 1}
	if action == resumeUpload || spec.Export && export == nil {
		outcome, err := op.resumeTurn(ctx, spec, turn)
		if !errors.Is(err, errRestart) || spec.Export && export == nil {
			return outcome, err
		}
		// A rejected current generation needs a durable replacement even
		// when this turn began upload-only and spent its one request.
	}

	return op.runTurn(ctx, spec, export, upTo, nil, turn)
}

func (op *syncOperation) runTurn(ctx context.Context, spec SourceSpec, export ExportFunc, upTo int64, snapshot *snapshotSource, turn *uploadTurn) (syncOutcome, error) {
	s := op.syncer
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.store.source(ctx, spec.Path, &spec)
	if err != nil {
		return syncDone, err
	}
	if err := op.validateAuthorization(src); err != nil {
		return syncDone, err
	}
	if err := op.protect(ctx, src); err != nil {
		return syncDone, err
	}
	if err := op.preflight(ctx, src, true); err != nil {
		return syncDone, err
	}
	for attempt := 0; ; attempt++ {
		if err := op.capture(ctx, src, export, upTo, snapshot); errors.Is(err, ErrSpoolFull) {
			if uerr := op.checkAuthorization(ctx); uerr != nil {
				return syncDone, errors.Join(err, uerr)
			}
			if _, uerr := op.uploadTurn(ctx, src, turn); uerr != nil {
				return syncDone, errors.Join(err, uerr)
			}
			if cerr := op.capture(ctx, src, export, upTo, snapshot); cerr != nil {
				if turn != nil && turn.remaining == 0 && errors.Is(cerr, ErrSpoolFull) {
					gens, gerr := s.store.pendingGens(ctx, src.ID)
					if gerr != nil {
						return syncDone, gerr
					}
					if len(gens) > 0 {
						return capturePending, nil
					}
				}
				return syncDone, cerr
			}
		} else if err != nil {
			return syncDone, err
		}
		pending, err := op.uploadTurn(ctx, src, turn)
		if !errors.Is(err, errRestart) || attempt > 0 {
			return pending, err
		}
		// Even with the Flush budget spent, recapture the rejected current
		// generation durably before yielding. PendingSpecs can then resume it.
	}
}

// capture chunks whatever the source gained since the last capture, up
// to upTo bytes of a file when upTo >= 0.
func (op *syncOperation) capture(ctx context.Context, src *sourceRow, export ExportFunc, upTo int64, snapshot *snapshotSource) error {
	s := op.syncer
	return op.captureWithCommit(ctx, src, export, upTo, snapshot, s.store.saveCapture)
}

// captureCommit is an explicit per-call persistence operation. Production uses
// Store.saveCapture directly; tests can guard its transaction or stop after its
// successful return to prove the actual capture loop's crash boundaries.
type captureCommit func(context.Context, *sourceRow, *genRow, []syncproto.Entry, *transcript.Watermark, []byte, ...func() error) error

func (op *syncOperation) captureWithCommit(ctx context.Context, src *sourceRow, export ExportFunc, upTo int64, snapshot *snapshotSource, commit captureCommit) error {
	s := op.syncer
	now := s.cfg.Now()
	defer s.captures.Add(1)
	var ex *exportRead
	if export != nil {
		var err error
		if ex, err = s.readExport(ctx, src, export); err != nil {
			return err
		}
	}
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return op.captureOwnedWithCommit(ctx, scope, src, ex, upTo, snapshot, now, commit)
	})
}

func (op *syncOperation) captureOwnedWithCommit(ctx context.Context, scope *spoolReferenceScope, src *sourceRow, ex *exportRead, upTo int64, snapshot *snapshotSource, now time.Time, commit captureCommit) error {
	s := op.syncer
	var (
		r  io.ReaderAt
		id transcript.Identity
		f  *os.File
	)
	if ex != nil {
		r, id = ex.r, transcript.Identity{Size: ex.size}
	} else {
		var err error
		path := src.Spec.Path
		if snapshot != nil {
			if s.held[src.ID] == nil && len(s.held) >= s.cfg.MaxHeldFiles {
				return errors.New("devicesync: snapshot descriptor limit reached")
			}
			path = snapshot.path
		}
		if op.authorization != nil {
			f, err = op.authorization.Open(ctx, src.Spec)
		} else {
			f, err = os.Open(path)
		}
		if errors.Is(err, os.ErrNotExist) {
			if op.authorization != nil {
				return err
			}
			if snapshot != nil {
				return err
			}
			return op.vanishOwned(ctx, scope, src)
		} else if err != nil {
			return err
		}
		if op.authorization != nil {
			if err := validateProofFile(f, &op.authorization.Proof); err != nil {
				f.Close()
				return err
			}
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
	// g may alias cur and mutate it below. Retain the old durable identity by
	// value so successful append, seal and rewrite can release exactly it.
	var oldTail syncproto.Tail
	var oldGen int64
	if cur != nil {
		oldTail, oldGen = cur.Tail, cur.Gen
	}
	var change transcript.Change
	if ex != nil {
		change = ex.change
	} else if change, err = transcript.Decide(src.Watermark, id, r); err != nil {
		return err
	}
	if op.canReplaceEmptyCurrent(src, cur) {
		// Materialize and attest a fresh generation even when the file remains empty.
		change = transcript.Change{Decision: transcript.Rewrite, Reason: "qualify empty native generation"}
	} else if op.authorization != nil && cur != nil {
		if err := op.validateGeneration(src, cur); err != nil {
			return err
		}
	}
	if cur == nil && change.Decision != transcript.Rewrite {
		change = transcript.Change{Decision: transcript.Rewrite, Reason: "no generation"}
	}
	if ex != nil {
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
		if ex != nil {
			if err := s.keepExportOwned(ctx, scope, src, cur, ex); err != nil {
				return err
			}
		}
		if cur.Tail.Size == 0 || !s.idle(id, now) {
			return nil
		}
		// Idle with a captured complete-record tail: fall through to seal it.
	case transcript.Rewrite:
		if cur != nil && !cur.done() {
			op.salvageOwned(ctx, scope, src, cur, change.Reason)
		}
		g = &genRow{SourceID: src.ID, Gen: src.Gen + 1, FileID: fileID(id), TailAcked: true}
		if cur != nil && cur.FileID != g.FileID {
			g.Previous = &syncproto.SourceRef{Path: src.Spec.Path, FileID: cur.FileID}
		} else if cur == nil && ex == nil {
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
	var scan io.ReaderAt = rr
	end := id.Size
	if src.Spec.StorageKind == transcript.StorageJSONLAppend && ex == nil {
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
	if ex != nil && ex.appended && g == cur {
		// An append: the bytes before it are the generation's provisional
		// tail, already redacted; only the appended bytes go through the
		// redactor. They start a line, so they redact as they would in
		// the whole export.
		rr = src.Spec.redacted(bytes.NewReader(ex.data))
		rr.CountFrom(0)
		scan = &appendReader{off: cur.Tail.Offset, tail: ex.tail, base: ex.base, rest: rr}
	}
	if src.Spec.rewriteProne() {
		// Worst case: every new byte is a chunk the spool lacks.
		if err := s.reserveCaptureOwned(ctx, scope, src.ID, end-from); err != nil {
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
				if err := scope.putChunk(p.hash, p.data); err != nil {
					return err
				}
			}
		}
		pend, pendBytes = pend[:0], 0
		return nil
	}
	tail, err := Scan(s.cfg.Chunk, scan, from, end, op.scratch.buf, func(c Chunk, data []byte) error {
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
	s.scanned.Add(end - from)
	newTail := syncproto.Tail{Offset: tail, Size: end - tail}
	if newTail.Size > 0 {
		data := make([]byte, newTail.Size)
		if _, err := scan.ReadAt(data, tail); err != nil && !(errors.Is(err, io.EOF) && tail+newTail.Size == end) {
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
				if err := scope.putChunk(add[len(add)-1].Hash, data); err != nil {
					return err
				}
			}
		} else if src.Spec.rewriteProne() {
			if err := scope.putTailVersion(src.ID, g.Gen, newTail.Hash, data); err != nil {
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
	var exportState []byte
	if ex != nil {
		wm, exportState = ex.watermark(), ex.state
	} else {
		w, err := transcript.NewWatermark(r, id, now, transcript.Cursor{Offset: end})
		if err != nil {
			return err
		}
		wm = &w
	}
	if op.authorization != nil {
		if err := op.checkAuthorization(ctx); err != nil {
			return err
		}
		if err := validateProofFile(f, &op.authorization.Proof); err != nil {
			return err
		}
		proof := op.authorization.Proof
		proof.ContentSHA = bytes.Clone(proof.ContentSHA)
		g.Proof = &proof
	}
	var guards []func() error
	if op.authorization != nil {
		guards = append(guards, func() error {
			return validateProofIdentity(f, &op.authorization.Proof)
		})
	}
	if err := commit(ctx, src, g, add, wm, exportState, guards...); err != nil {
		return err
	}
	s.releaseTailOwned(ctx, scope, src.ID, oldGen, oldTail)
	if op.authorization != nil {
		if err := validateProofFile(f, &op.authorization.Proof); err != nil {
			return err // verified capture remains a fact; a changed live source requires reindexing
		}
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
func (op *syncOperation) vanish(ctx context.Context, src *sourceRow) error {
	s := op.syncer
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return op.vanishOwned(ctx, scope, src)
	})
}

func (op *syncOperation) vanishOwned(ctx context.Context, scope *spoolReferenceScope, src *sourceRow) error {
	s := op.syncer
	if src.Watermark == nil {
		return nil
	}
	if g, err := s.store.gen(ctx, src.ID, src.Gen); err != nil {
		return err
	} else if g != nil && !g.done() {
		op.salvageOwned(ctx, scope, src, g, "source disappeared")
	}
	return scope.setWatermark(ctx, src, nil)
}

// salvage copies a generation's unacknowledged bytes into the spool before
// the source destroys them, reading from the descriptor held since capture
// (which still reaches a replaced or unlinked inode). Bytes that cannot be
// recovered (in-place rewrite, spool full, no held descriptor) are recorded
// as a gap: the generation is cut back to the recoverable prefix.
func (op *syncOperation) salvage(ctx context.Context, src *sourceRow, g *genRow, why string) {
	s := op.syncer
	if err := s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		op.salvageOwned(ctx, scope, src, g, why)
		return nil
	}); err != nil {
		s.cfg.Logger.Warn("devicesync: salvage reference ownership failed", "err", err)
	}
}

func (op *syncOperation) salvageOwned(ctx context.Context, scope *spoolReferenceScope, src *sourceRow, g *genRow, why string) {
	s := op.syncer
	if op.authorization != nil {
		return
	} // retain pending generation; raw repair requires a fresh verified open

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
		if !ok || scope.putChunk(e.Hash, data) != nil {
			keep = e.Ordinal
			break
		}
	}
	tailOK := g.TailAcked || g.Tail.Size == 0 || keep < g.Entries
	if !tailOK {
		if body, ok, err := s.spool.TailVersion(src.ID, g.Gen, g.Tail.Hash); err == nil && ok && int64(len(body)) == g.Tail.Size {
			tailOK = true
		} else if data, ok := readVerified(g.Tail.Offset, g.Tail.Size, g.Tail.Hash); ok && scope.putTailVersion(src.ID, g.Gen, g.Tail.Hash, data) == nil {
			tailOK = true
		}
	}
	if keep < g.Entries || !tailOK {
		s.cfg.Logger.Warn("devicesync: unacknowledged bytes lost before upload",
			"path", src.Spec.Path, "generation", g.Gen, "reason", why,
			"kept_entries", keep, "entries", g.Entries, "tail_bytes", g.Tail.Size, "spool_blocked", s.spool.Blocked())
		if err := s.cutOwned(ctx, scope, src, g, keep); err != nil {
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
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return s.cutOwned(ctx, scope, src, g, keep)
	})
}

func (s *Syncer) cutOwned(ctx context.Context, scope *spoolReferenceScope, src *sourceRow, g *genRow, keep int64) error {
	oldTail := g.Tail
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
	if err := scope.updateGen(ctx, g, nil); err != nil {
		return err
	}
	if g.done() {
		delete(s.stalls, [2]int64{src.ID, g.Gen})
	}
	s.releaseOwned(ctx, scope, src, g, dropped)
	s.releaseTailOwned(ctx, scope, src.ID, g.Gen, oldTail)
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

// exportRead is one export as capture reads it.
type exportRead struct {
	r      io.ReaderAt // the export's raw bytes (from the tail offset on for an append)
	size   int64
	change transcript.Change
	state  []byte
	// appended: data extends the generation's captured bytes at base, and
	// tail is their redacted provisional tail, from the generation's tail
	// offset to base. Otherwise data is the whole export.
	appended bool
	data     []byte
	base     int64
	tail     []byte
}

// watermark is the watermark after capturing the export. A whole export's
// anchor is its hash, so a later whole export that extends it appends
// (decideExport); after an append the anchor is unknown (AnchorLen 0) and
// only the exporter's state continues the generation.
func (e *exportRead) watermark() *transcript.Watermark {
	if e.appended {
		return &transcript.Watermark{Offset: e.size}
	}
	return exportWatermark(e.data)
}

// readExport asks fn for the source's export: an append to the current
// generation when the saved state, the generation and its spooled tail
// allow one, else the whole export.
//
// An append is redacted on its own, after the already redacted tail, so it
// must start a line of a line-redacted source: otherwise a secret across
// the boundary would be split between two redactions and missed. A state
// is therefore kept only while the captured bytes end a line (lineEnd),
// and an append that does not end a line is not used.
func (s *Syncer) readExport(ctx context.Context, src *sourceRow, fn ExportFunc) (*exportRead, error) {
	cur, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil {
		return nil, err
	}
	if src.Watermark != nil && src.ExportState != nil && cur != nil && !cur.Lost && cur.Size == src.Watermark.Offset {
		e, err := fn(ctx, src.ExportState)
		if err != nil {
			return nil, err
		}
		s.exported.Add(int64(len(e.Data)))
		if !e.Append {
			return wholeExport(src, e), nil
		}
		if tail, ok := s.exportTail(src, cur); ok && lineEnd(e.Data) {
			base := src.Watermark.Offset
			ex := &exportRead{size: base + int64(len(e.Data)), state: e.State, appended: true, data: e.Data, base: base, tail: tail,
				change: transcript.Change{Decision: transcript.Append}}
			ex.r = &appendReader{off: cur.Tail.Offset, tail: tail, base: base, rest: bytes.NewReader(e.Data)}
			if len(e.Data) == 0 {
				ex.change = transcript.Change{Decision: transcript.Unchanged}
			}
			return ex, nil
		}
	}
	e, err := fn(ctx, nil)
	if err != nil {
		return nil, err
	}
	s.exported.Add(int64(len(e.Data)))
	if e.Append {
		return nil, fmt.Errorf("devicesync: export %s: an append without a previous state", src.Spec.Path)
	}
	return wholeExport(src, e), nil
}

func wholeExport(src *sourceRow, e Export) *exportRead {
	if e.Data == nil {
		e.Data = []byte{}
	}
	if redact.ModeFor(string(src.Spec.StorageKind), src.Spec.Path) != redact.Lines || !lineEnd(e.Data) {
		e.State = nil // no append may follow (readExport)
	}
	return &exportRead{r: bytes.NewReader(e.Data), size: int64(len(e.Data)), change: decideExport(src.Watermark, e.Data), state: e.State, data: e.Data}
}

// lineEnd reports whether b is empty or ends a line.
func lineEnd(b []byte) bool { return len(b) == 0 || b[len(b)-1] == '\n' }

// exportTail returns the redacted bytes of the generation's provisional
// tail from the spool (keepTail keeps an export's after it is
// acknowledged), when they are there and match the generation.
func (s *Syncer) exportTail(src *sourceRow, g *genRow) ([]byte, bool) {
	if g.Tail.Offset+g.Tail.Size != src.Watermark.Offset {
		return nil, false // a gap cut the tail
	}
	if g.Tail.Size == 0 {
		return []byte{}, true
	}
	data, ok, err := s.spool.TailVersion(src.ID, g.Gen, g.Tail.Hash)
	if err != nil || !ok || int64(len(data)) != g.Tail.Size || syncproto.Sum(data) != g.Tail.Hash {
		return nil, false
	}
	return data, true
}

// keepExport, for an unchanged export, saves the exporter's state when it
// moved (the first capture since the state was dropped), and spools the
// provisional tail again when the spool lost it, so the next change can
// append.
func (s *Syncer) keepExport(ctx context.Context, src *sourceRow, g *genRow, ex *exportRead) error {
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return s.keepExportOwned(ctx, scope, src, g, ex)
	})
}

func (s *Syncer) keepExportOwned(ctx context.Context, scope *spoolReferenceScope, src *sourceRow, g *genRow, ex *exportRead) error {
	if !bytes.Equal(ex.state, src.ExportState) {
		src.ExportState = ex.state
		if err := scope.setWatermark(ctx, src, src.Watermark); err != nil {
			return err
		}
	}
	if ex.appended || g.Tail.Size == 0 || g.Lost || src.ExportState == nil {
		return nil
	}
	if _, ok := s.exportTail(src, g); ok {
		return nil
	}
	data := make([]byte, g.Tail.Size)
	if _, err := src.Spec.redacted(ex.r).ReadAt(data, g.Tail.Offset); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if syncproto.Sum(data) != g.Tail.Hash {
		return nil
	}
	return scope.putTailVersion(src.ID, g.Gen, g.Tail.Hash, data)
}

// reserveCapture only reconciles this source's immutable tail attempts after
// cap pressure. Failed handoff files stay charged until durable reference reads
// succeed. It does not reclaim chunks or coordinate concurrent workers.
func (s *Syncer) reserveCapture(ctx context.Context, sid, bytes int64) error {
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return s.reserveCaptureOwned(ctx, scope, sid, bytes)
	})
}

func (s *Syncer) reserveCaptureOwned(ctx context.Context, scope *spoolReferenceScope, sid, bytes int64) error {
	if err := s.spool.Reserve(bytes); !errors.Is(err, ErrSpoolFull) {
		return err
	}
	if err := scope.reconcileTailVersions(ctx, sid); err != nil {
		return err
	}
	return s.spool.Reserve(bytes)
}

// releaseTail follows a durable commit/acknowledgement. Reference uncertainty
// or cleanup failure leaves bytes charged for startup or later pressure retry.
func (s *Syncer) releaseTail(ctx context.Context, sid, gen int64, old syncproto.Tail) {
	if err := s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return scope.releaseTail(ctx, sid, gen, old)
	}); err != nil {
		s.cfg.Logger.Warn("devicesync: released tail cleanup deferred", "source_id", sid, "generation", gen, "err", err)
	}
}

func (s *Syncer) releaseTailOwned(ctx context.Context, scope *spoolReferenceScope, sid, gen int64, old syncproto.Tail) {
	if err := scope.releaseTail(ctx, sid, gen, old); err != nil {
		s.cfg.Logger.Warn("devicesync: released tail cleanup deferred", "source_id", sid, "generation", gen, "err", err)
	}
}

// setWatermark changes the durable tail-continuation reference outside capture.
func (s *Syncer) setWatermark(ctx context.Context, src *sourceRow, wm *transcript.Watermark) error {
	return s.spool.withReferences(s.store, func(scope *spoolReferenceScope) error {
		return scope.setWatermark(ctx, src, wm)
	})
}

// appendReader serves an appended export from the generation's tail
// offset: the spooled (redacted) tail, then rest from base on.
type appendReader struct {
	off  int64
	tail []byte
	base int64
	rest io.ReaderAt
}

func (a *appendReader) ReadAt(p []byte, off int64) (int, error) {
	if off < a.off {
		return 0, fmt.Errorf("devicesync: read at %d before the export tail at %d", off, a.off)
	}
	n := 0
	if off < a.base {
		n = copy(p, a.tail[off-a.off:])
		if n == len(p) {
			return n, nil
		}
		off += int64(n)
	}
	m, err := a.rest.ReadAt(p[n:], off-a.base)
	return n + m, err
}

// decideExport: an export that extends the previous one appends; any other
// change is a rewrite. The watermark's anchor holds the hash of the whole
// previous export (none after an appended export).
func decideExport(prev *transcript.Watermark, data []byte) transcript.Change {
	switch {
	case prev == nil:
		return transcript.Change{Decision: transcript.Rewrite, Reason: "no watermark"}
	case prev.AnchorLen != prev.Offset || int64(len(data)) < prev.Offset || syncproto.Sum(data[:prev.Offset]) != syncproto.Hash(prev.AnchorSum):

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
