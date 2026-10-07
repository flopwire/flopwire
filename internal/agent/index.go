package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

// indexTranscript brings the index up to date with one JSONL transcript
// (spec §6.2): Decide against the saved watermark, then parse the appended
// bytes from the saved cursor, or re-parse the whole file as a new
// generation and mark rows it no longer has superseded. The watermark and
// cursor state are saved in the transaction of the last batch. It reports
// whether rows were written.
func (a *Agent) indexTranscript(ctx context.Context, t *target) (bool, error) {
	a.captureScopeMu.RLock()
	defer a.captureScopeMu.RUnlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !a.coworkSafe(t) {
		return false, nil
	}
	if t.parser == nil {
		return false, nil // a loaded placeholder not listed by discovery yet
	}
	f, err := a.openNativeEvidence(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // the next full pass retires it
	} else if err != nil {
		return false, err
	}
	defer f.Close() // held while parsing: an unlinked file stays readable
	sampled := time.Now()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	id := transcript.IdentityOf(fi)
	if mode, known := a.modeOf(t); !known && worstMode(a.policy().pol) == pathpolicy.Deny {
		return false, nil // no complete line names the directory yet, and a rule could deny it
	} else if known && mode == pathpolicy.Deny {
		// D18: neither indexed nor uploaded. Rows from before the rule go.
		why, _, _ := a.decisionOf(a.policy(), t)
		a.mu.Lock()
		sid := t.sourceID
		a.mu.Unlock()
		if sid != 0 {
			if err := a.purgeSource(ctx, sid, why); err != nil {
				return false, err
			}
		}
		a.markSeen(t, id, sampled, sid, "")
		a.mu.Lock()
		t.indexedWith = "" // no rows: nothing to re-parse (D16)
		a.mu.Unlock()
		return false, nil
	}
	st, moved, err := a.sourceFor(ctx, t, id)
	if err != nil {
		return false, err
	}
	name := t.parser.Name()
	version := indexingVersion(t.parser)
	applied := transcript.ReparseKey(sourceVersion(st))
	a.mu.Lock()
	failed := t.reparseFailed == version
	a.mu.Unlock()
	var change transcript.Change
	reparse := st.Watermark != nil && (applied != version || (st.Extraction != nil && st.Extraction.Generation != st.Generation)) && !failed
	a.mu.Lock()
	indexedThisRun := t.seenAt >= a.initializedAt.UnixNano()
	a.mu.Unlock()
	if reparse {
		// D16: the source was indexed by another parser version. Re-parse
		// it whole from its bytes as a new generation, but only after a dry
		// run shows the new parser gets through it: a parser that fails on
		// the file must leave the rows it has alone.
		if err := a.dryRun(ctx, t, f, id); err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			a.mu.Lock()
			t.reparseFailed = version
			a.mu.Unlock()
			return false, fmt.Errorf("re-parse with %s failed, keeping rows from %s: %w", name, st.Source.Parser, err)
		}
		change = transcript.Change{Decision: transcript.Rewrite, Reason: fmt.Sprintf("parser %s -> %s", applied, version)}
	} else if change, err = transcript.Decide(st.Watermark, id, f); err != nil {
		return false, err
	}
	if change.Decision == transcript.Append && st.Generation == 0 {
		change = transcript.Change{Decision: transcript.Rewrite, Reason: "no generation"}
	}
	if id.Size > 0 && change.Decision != transcript.Unchanged {
		if err := a.taintCoworkEvidence(ctx, t); err != nil {
			return false, err
		}
		// Historical provenance can tighten an initially local capture to deny.
		// Recheck after its durable write, before parsing any of these bytes.
		if mode, known := a.modeOf(t); known && mode == pathpolicy.Deny {
			if err := a.purgeDenied(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	gen, cur := st.Generation, transcript.Cursor{}
	switch change.Decision {
	case transcript.Unchanged:
		a.stats.Unchanged.Add(1)
		if change.Verified {
			wm := *st.Watermark
			wm.Identity, wm.SampledAt = id, sampled.UnixNano()
			if err := a.store.SaveWatermark(ctx, st.ID, wm, st.CursorState); err != nil {
				return false, err
			}
		}
		a.markSeen(t, id, sampled, st.ID, applied)
		if moved {
			a.notify(t)
		}
		return false, nil
	case transcript.Append:
		a.stats.Appends.Add(1)
		cur = transcript.Cursor{Offset: st.Watermark.Offset, LineNo: st.Watermark.LineNo, State: st.CursorState}
	case transcript.Rewrite:
		gen++
		reason := change.Reason
		if gen == 1 {
			reason = "initial"
		} else {
			a.stats.Rewrites.Add(1)
			a.log.Info("agent: rewrite", "path", t.path, "generation", gen, "reason", change.Reason)
		}
		g := transcript.Generation{Generation: gen, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: sampled, Complete: true}
		if err := a.store.StartGeneration(ctx, st.ID, g, reason); err != nil {
			return false, err
		}
	}
	src := t.src
	src.FileID = id.ID
	sink := &cwdSink{Sink: a.store.NewSink(ctx, st.ID, gen), reads: readSightings{agent: t.src.Agent}}
	sink.BatchSize = a.cfg.BatchRows
	// The watermark hashes the bytes the parser consumed (P2), so a
	// rewrite during the parse shows up at the next Decide.
	rec, err := transcript.NewReadRecorder(f, cur.Offset)
	if err != nil {
		return false, err
	}
	result, err := transcript.Extract(ctx, t.parser, transcript.Input{Source: &src, R: rec, Size: id.Size}, cur, sink)
	if err != nil {
		return false, err
	}
	// A reporting parser can reject its saved state and restart at zero.
	// Give that replacement extraction a new generation so absent rows retire.
	if result.Report != nil && result.FromOffset == 0 && cur.Offset > 0 {
		gen++
		g := transcript.Generation{Generation: gen, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: sampled, Complete: true}
		if err := a.store.StartGeneration(ctx, st.ID, g, "cursor state reset"); err != nil {
			return false, err
		}
		change.Decision = transcript.Rewrite
		sink = &cwdSink{Sink: a.store.NewSink(ctx, st.ID, gen), reads: readSightings{agent: t.src.Agent}}
		sink.BatchSize = a.cfg.BatchRows
		rec, err = transcript.NewReadRecorder(f, 0)
		if err != nil {
			return false, err
		}
		result, err = transcript.Extract(ctx, t.parser, transcript.Input{Source: &src, R: rec, Size: id.Size}, transcript.Cursor{}, sink)
		if err != nil {
			return false, err
		}
	}
	next := result.Cursor
	sink.Extraction, err = transcript.FinalizeExtraction(st.Extraction, gen, transcript.ParserContract(t.parser), result)
	if err != nil {
		return false, err
	}
	sink.AppliedParser = name
	wm, err := rec.Watermark(id, sampled, next)
	if err != nil {
		return false, err
	}
	indexedWith := version
	if change.Decision == transcript.Rewrite {
		// In the final batch's transaction, before the watermark: a crash
		// cannot save the new watermark and leave absent rows live (L5).
		// Older identities at the path are retired on every rewrite, not
		// only the first: a crash during a replaced file's first parse
		// resumes at generation 2.
		sink.SupersedeAbsent = gen > 1
		if sink.RetireSources, err = a.replacedSources(ctx, t, id); err != nil {
			return false, err
		}
	}
	// Directories the new lines named join the session's placement set
	// before the watermark is saved: the next parse starts past the lines
	// that named them and would not name them again, and sync reads no
	// further than the watermark while rules are in force (uploadBound).
	tt, err := a.recordCwds(ctx, t, sink.cwds)
	if err != nil {
		return false, err
	}
	if err := sink.Flush(&wm, next.State); err != nil {
		return false, err
	}
	if testHookAfterFlush != nil {
		if err := testHookAfterFlush(); err != nil {
			return true, err
		}
	}

	a.stats.Indexed.Add(1)
	denied, err := a.tighten(ctx, t, st.ID, tt)
	if err != nil {
		return true, err
	}
	if !denied {
		// A transcript the rules now deny gives nothing, a read neither (D18).
		a.markRead(ctx, &sink.reads)
	}
	// Decide verifies content even when a rewrite preserves size and mtime.
	// Initial catch-up and extraction migrations do not create live activity.
	// A ctime-only rewrite can also be chmod: compare persisted byte evidence
	// rather than treating every conservative Rewrite verdict as activity.
	prior := st.Watermark
	byteEvidenceChanged := prior != nil && (wm.Offset != prior.Offset || wm.HeadHash != prior.HeadHash || wm.AnchorSum != prior.AnchorSum || id.Size != prior.Identity.Size || id.ID != prior.Identity.ID)
	if indexedThisRun && !reparse && byteEvidenceChanged && change.Decision != transcript.Unchanged {
		a.mu.Lock()
		t.notice.Kind = devicesync.NoticeChanged
		a.mu.Unlock()
	}
	a.markSeen(t, id, sampled, st.ID, indexedWith)
	a.mu.Lock()
	t.scanned = next.Offset
	if denied {
		t.indexedWith = "" // no rows: nothing to re-parse (D16)
	}
	a.mu.Unlock()
	a.notify(t)
	return true, nil
}

// testHookAfterFlush runs right after a parse saved its final batch and
// watermark; an error stops the index there, as a crash would.
var testHookAfterFlush func() error

// cwdSink is the index sink, noting every working directory the parsed
// conversations name (transcript.Conversation.Cwd and OtherCwds), and the
// Flopwire messages its hook context shows (read receipts).
type cwdSink struct {
	*localindex.Sink
	cwds  []string
	seen  map[string]bool
	reads readSightings
}

func (s *cwdSink) Message(m *transcript.Message) error {
	s.reads.note(m)
	return s.Sink.Message(m)
}

func (s *cwdSink) Conversation(c *transcript.Conversation) error {
	for _, d := range append([]string{c.Cwd}, c.OtherCwds...) {
		if d != "" && !s.seen[d] {
			if s.seen == nil {
				s.seen = map[string]bool{}
			}
			s.seen[d] = true
			s.cwds = append(s.cwds, d)
		}
	}
	return s.Sink.Conversation(c)
}

// sourceFor returns the source row for the file at t.path with identity id.
// A file that moved (same agent, session key and identity as a source whose
// path no longer exists, e.g. a Codex rollout archived to
// archived_sessions/) keeps its source: the row is re-pathed and moved is
// true. Otherwise a new identity at the path is a new source.
func (a *Agent) sourceFor(ctx context.Context, t *target, id transcript.Identity) (localindex.SourceState, bool, error) {
	if t.sourceID != 0 {
		st, err := a.store.Source(ctx, t.sourceID)
		if err == nil && st.Source.FileID == id.ID && st.Source.Path == t.path {
			return st, false, nil
		}
	}
	all, err := a.store.ListSources(ctx, t.src.Agent, id.ID.String())
	if err != nil {
		return localindex.SourceState{}, false, err
	}
	for _, st := range all {
		if st.Source.Path == t.path {
			full, err := a.store.Source(ctx, st.ID)
			return full, false, err
		}
	}
	for _, st := range all {
		if t.src.SessionKey == "" || st.Source.SessionKey != t.src.SessionKey {
			continue
		}
		if _, err := fsprobe.Stat(st.Source.Path); !errors.Is(err, os.ErrNotExist) {
			continue // the old path still exists: not a move
		}
		if err := a.store.MoveSource(ctx, st.ID, t.path); err != nil {
			return st, false, err
		}
		a.stats.Renames.Add(1)
		a.log.Info("agent: source moved", "from", st.Source.Path, "to", t.path)
		st, err := a.store.Source(ctx, st.ID)
		return st, true, err
	}
	src := t.src
	src.FileID, src.Parser = id.ID, t.parser.Name()
	st, err := a.store.EnsureSource(ctx, src)
	return st, false, err
}

// discardSink drops a dry run's output.
type discardSink struct{}

func (discardSink) Conversation(*transcript.Conversation) error     { return nil }
func (discardSink) Message(*transcript.Message) error               { return nil }
func (discardSink) SupersedeSession(transcript.Agent, string) error { return nil }

var _ transcript.SessionSuperseder = discardSink{}

// dryRun parses the whole file into nothing, to learn whether the parser
// gets through it.
func (a *Agent) dryRun(ctx context.Context, t *target, f *os.File, id transcript.Identity) error {
	src := t.src
	src.FileID = id.ID
	_, err := t.parser.Parse(ctx, transcript.Input{Source: &src, R: f, Size: id.Size}, transcript.Cursor{}, discardSink{})
	return err
}

// replacedSources returns the other sources recorded at t.path (Claude
// replaced about 2% of transcripts with a new inode mid-session). Rows the
// new file re-emitted moved to the new source by native id; what is left
// on the old sources is absent from the file, and the final batch retires
// it (Batch.RetireSources).
func (a *Agent) replacedSources(ctx context.Context, t *target, id transcript.Identity) ([]int64, error) {
	olds, err := a.store.SourcesByPath(ctx, t.path)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, old := range olds {
		if old.Source.FileID != id.ID {
			out = append(out, old.ID)
		}
	}
	return out, nil
}

func (a *Agent) markSeen(t *target, id transcript.Identity, sampled time.Time, sourceID int64, parser string) {
	a.mu.Lock()
	t.seen, t.seenAt, t.sourceID = id, sampled.UnixNano(), sourceID
	if parser != "" {
		t.indexedWith = parser
	}
	a.mu.Unlock()
}

// notify hands t to sync when the path rules allow its upload. It
// returns false while the rules cannot decide yet (the session is not
// placed), so the caller asks again later.
func (a *Agent) notify(t *target) bool {
	if a.cfg.Sync == nil {
		return true
	}
	m, known := a.modeOf(t)
	if known && m == pathpolicy.Allow {
		sp := a.specOf(t)
		a.mu.Lock()
		notice := t.notice
		t.notice.Kind = devicesync.NoticeHistorical
		a.mu.Unlock()
		a.cfg.Sync.NotifyWithNotice(sp, notice)
	}
	return known
}

// indexCompanion records a Claude companion file (tool-results/*,
// agent-*.meta.json, workflow files) under its conversation and hands it
// to sync with its parent transcript's path. Text tool results reach the
// index through the parser, which reads them when a transcript line
// references them.
func (a *Agent) indexCompanion(ctx context.Context, t *target) error {
	a.captureScopeMu.RLock()
	defer a.captureScopeMu.RUnlock()
	if !a.coworkSafe(t) {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id, err := transcript.StatIdentity(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	sampled := time.Now()
	if mode, known := a.modeOf(t); !known && worstMode(a.policy().pol) == pathpolicy.Deny {
		return nil // its session is not placed yet; asked again at the next change or sweep
	} else if known && mode == pathpolicy.Deny {
		a.markSeen(t, id, sampled, 0, "")
		return nil
	}
	a.mu.Lock()
	var parentID int64
	if p := a.targets[t.parent]; p != nil {
		parentID = p.sourceID
	}
	owner, role := t.owner, t.role // a discovery pass may update them (merge)
	newEvidence := id != t.seen
	a.mu.Unlock()
	var digest []byte
	if _, scoped := a.coworkMode(a.policy(), t); scoped || a.desktopCodeScoped(t.path) {
		file, openErr := a.openNativeEvidence(t.path)
		err = openErr
		if err != nil {
			return err
		}
		h := sha256.New()
		fi, statErr := file.Stat()
		if statErr == nil {
			_, err = io.Copy(h, io.LimitReader(file, fi.Size()))
		} else {
			err = statErr
		}
		file.Close()
		if err != nil {
			return err
		}
		id = transcript.IdentityOf(fi)
		digest = h.Sum(nil)
		prior, err := a.store.CompanionDigest(ctx, t.path)
		if err != nil {
			return err
		}
		// Gate invalidation clears the in-memory sample, but does not erase
		// mapping proof for exactly the bytes already captured.
		newEvidence = !bytes.Equal(prior, digest)
	}
	if id.Size > 0 && newEvidence {
		if err := a.taintCoworkEvidence(ctx, t); err != nil {
			return err
		}
		if mode, known := a.modeOf(t); known && mode == pathpolicy.Deny {
			return a.purgeDenied(ctx)
		}
	}
	if err := a.store.UpsertCompanion(ctx, localindex.Companion{SessionID: owner, Agent: transcript.AgentClaude,
		SourceID: parentID, Path: t.path, Kind: string(role), Size: id.Size, ContentSHA: digest}); err != nil {
		return err
	}
	a.stats.Companions.Add(1)
	a.markSeen(t, id, sampled, 0, "")
	a.notify(t)
	return nil
}

// applyStubs records a conversation for each orphaned Claude session (its
// transcript was cleaned up, its tool-results/ remain), once per process.
func (a *Agent) applyStubs(ctx context.Context, f *found) {
	pv := a.policy()
	var todo []*transcript.Conversation
	for _, c := range f.stubs {
		a.mu.Lock()
		done := a.stubbed[c.SessionID]
		a.mu.Unlock()
		if done {
			continue
		}
		if d, ok := a.coworkMode(pv, &target{path: f.stubAt[c.SessionID], src: transcript.Source{Agent: transcript.AgentClaude, SessionKey: c.SessionID}}); ok && d.Mode == pathpolicy.Deny {
			continue
		}
		if !pv.pol.Empty() {
			// Placed like its companions: the stored placement, else the
			// project folder.
			p, known := a.placeOf(placeKey{transcript.AgentClaude, c.SessionID}, f.stubAt[c.SessionID])
			known = known && settled(pv.pol, p)
			if !known && worstMode(pv.pol) == pathpolicy.Deny {
				continue // not placed yet; the next pass asks again
			}
			if known && a.decide(pv.pol, p).Mode == pathpolicy.Deny {
				continue
			}
		}
		a.mu.Lock()
		a.stubbed[c.SessionID] = true
		a.mu.Unlock()
		todo = append(todo, c)
	}
	a.mu.Lock()
	src := a.orphanSrc
	a.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	if src == 0 {
		st, err := a.store.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentClaude, Path: a.cfg.ClaudeProjects + "#orphans",
			StorageKind: transcript.StorageCompanion, Parser: claude.ParserName})
		if err != nil {
			a.log.Warn("agent: orphan source", "err", err)
			return
		}
		src = st.ID
		a.mu.Lock()
		a.orphanSrc = src
		a.mu.Unlock()
	}
	// A stub never overrides a conversation the session's transcripts
	// already recorded: the upsert only fills empty columns and extra.
	if _, err := a.store.ApplyBatch(ctx, localindex.Batch{SourceID: src, Generation: 1, Conversations: todo}); err != nil {
		a.log.Warn("agent: orphan stubs", "err", err)
	}
}

func sourceVersion(st localindex.SourceState) string {
	if st.Extraction != nil && st.Extraction.Report.Validate() == nil && st.Watermark != nil && st.Extraction.Generation == st.Generation && st.Extraction.Offset == st.Watermark.Offset && st.Extraction.LineNo == st.Watermark.LineNo {
		return st.Extraction.Contract
	}
	return st.Source.Parser
}
