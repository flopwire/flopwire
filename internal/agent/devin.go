package agent

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// devinState is the poll gate for Devin's sessions.db: the identity of the
// database file and of its -wal at the last parse. The CLI commits to the
// WAL, so a change shows in one of the two stats.
type devinState struct {
	mu       sync.Mutex // serializes polls
	path     string
	db, wal  transcript.Identity
	sourceID int64
	last     time.Time
	synced   bool // every session handed to sync since start

	indexedWith   string // parser version the store was indexed with
	reparseFailed string // parser version whose full re-parse failed
	// dirty forces the next poll to parse (path rules purged sessions and
	// reset the cursor, or changed which sessions are indexed).
	dirty atomic.Bool
	// reparsing is set while a background re-parse (D16) holds mu.
	reparsing atomic.Bool
}

// minDevinPoll bounds how often a busy store is re-parsed.
const minDevinPoll = time.Second

// pollDevin brings the index up to date with Devin's store when its stat
// tuple moved (or force), through the parser's incremental cursor: per
// session watermarks in the cursor state, deleted sessions superseded
// through the sink. Changed sessions are handed to sync as per-session
// exports (devin-export@1), a deleted one as its "gone" record. With wait
// it waits for a poll already running instead of skipping.
func (a *Agent) pollDevin(ctx context.Context, force, wait bool) {
	d := &a.devin
	if d.path == "" {
		return
	}
	if !d.mu.TryLock() {
		if !wait {
			return // a poll (or a background re-parse) is running
		}
		// Wait for the running poll, then poll again: what it read may
		// predate the caller's request. A background re-parse is not
		// waited for (D16: a pass never waits for one).
		for !d.mu.TryLock() {
			if d.reparsing.Load() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	unlock := true
	defer func() {
		if unlock {
			d.mu.Unlock()
		}
	}()
	if !force && time.Since(d.last) < minDevinPoll {
		return
	}
	db, err := transcript.StatIdentity(d.path)
	if err != nil {
		return // no Devin on this device
	}
	wal, _ := transcript.StatIdentity(d.path + "-wal")
	name := (&devin.Parser{}).Name()
	a.mu.Lock()
	bg := a.bgOn
	a.mu.Unlock()
	if bg && d.indexedWith != "" && transcript.ReparseKey(d.indexedWith) != transcript.ReparseKey(name) && d.reparseFailed != name {
		// D16: indexed by another parser version. Re-parse the whole store
		// in the background; polls wait (TryLock) until it is done.
		unlock = false
		d.reparsing.Store(true)
		a.bgWG.Add(1)
		go func() {
			defer a.bgWG.Done()
			defer d.mu.Unlock()
			defer d.reparsing.Store(false)
			d.last = time.Now()
			if err := a.parseDevin(ctx, db, true); err != nil {
				if ctx.Err() == nil {
					d.reparseFailed = name
					a.stats.Errors.Add(1)
					a.log.Warn("agent: devin re-parse failed; keeping rows", "path", d.path, "parser", name, "err", err)
				}
				return
			}
			d.db, d.wal = db, wal
		}()
		return
	}
	if d.dirty.Swap(false) {
		force = true
	}
	if !force && db == d.db && wal == d.wal && d.sourceID != 0 {
		return
	}
	d.last = time.Now()
	if err := a.parseDevin(ctx, db, false); err != nil {
		if ctx.Err() == nil {
			a.stats.Errors.Add(1)
			a.log.Warn("agent: devin", "path", d.path, "err", err)
		}
		return
	}
	d.db, d.wal = db, wal
}

// devinSink records which sessions a parse touched, and drops what the
// path rules deny (D18).
type devinSink struct {
	*localindex.Sink
	touched map[string]bool
	deny    func(session, cwd string) bool // nil: no rules
}

func (s *devinSink) denied(session, cwd string) bool { return s.deny != nil && s.deny(session, cwd) }

func (s *devinSink) Conversation(c *transcript.Conversation) error {
	if s.denied(c.SessionID, c.Cwd) {
		return nil
	}
	s.touched[c.SessionID] = true
	return s.Sink.Conversation(c)
}

func (s *devinSink) Message(m *transcript.Message) error {
	if s.denied(m.SessionID, "") {
		return nil
	}
	s.touched[m.SessionID] = true
	return s.Sink.Message(m)
}

func (s *devinSink) SupersedeSession(agent transcript.Agent, id string) error {
	if s.denied(id, "") {
		return nil
	}
	s.touched[id] = true
	return s.Sink.SupersedeSession(agent, id)
}

var _ transcript.SessionSuperseder = (*devinSink)(nil)

// parseDevin parses what changed in Devin's store. full re-parses the whole
// store as a new generation (a parser version change, D16): a dry run
// first, then every row again, then rows the new parse did not emit are
// superseded and the source records the new parser version.
func (a *Agent) parseDevin(ctx context.Context, id transcript.Identity, full bool) error {
	d := &a.devin
	p := &devin.Parser{}
	sampled := time.Now()
	var st localindex.SourceState
	var err error
	if d.sourceID != 0 {
		st, err = a.store.Source(ctx, d.sourceID)
	} else {
		// One source per store path; the file identity is informational
		// (the parser reads rows, not bytes).
		st, err = a.store.EnsureSource(ctx, transcript.Source{Agent: transcript.AgentDevin, Path: d.path, FileID: id.ID,
			StorageKind: transcript.StorageSQLite, Parser: p.Name()})
	}
	if err != nil {
		return err
	}
	d.sourceID = st.ID
	if full {
		src := st.Source
		if _, err := p.Parse(ctx, transcript.Input{Source: &src}, transcript.Cursor{}, discardSink{}); err != nil {
			return err
		}
		st.Generation++
		st.CursorState, st.Watermark = nil, nil
		g := transcript.Generation{Generation: st.Generation, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: sampled, Complete: true}
		if err := a.store.StartGeneration(ctx, st.ID, g, fmt.Sprintf("parser %s -> %s", st.Source.Parser, p.Name())); err != nil {
			return err
		}
	}
	if st.Generation == 0 {
		st.Generation = 1
		g := transcript.Generation{Generation: 1, Size: id.Size, ChangeTime: time.Unix(0, id.CTime), CapturedAt: sampled, Complete: true}
		if err := a.store.StartGeneration(ctx, st.ID, g, "initial"); err != nil {
			return err
		}
	}
	cur := transcript.Cursor{State: st.CursorState}
	if st.Watermark != nil {
		cur.Offset = st.Watermark.Offset
	}
	sink := &devinSink{Sink: a.store.NewSink(ctx, st.ID, st.Generation), touched: map[string]bool{}}
	if pv := a.policy(); !pv.pol.Empty() {
		a.loadDevinModes(ctx, pv)
		sink.deny = func(session, cwd string) bool {
			a.mu.Lock()
			m, ok := a.devinModes[session]
			a.mu.Unlock()
			if !ok { // a session newer than the directory listing
				m = a.devinDecide(pv, session, cwd).Mode
			}
			return m == pathpolicy.Deny
		}
	}
	sink.BatchSize = a.cfg.BatchRows
	src := st.Source
	next, err := p.Parse(ctx, transcript.Input{Source: &src}, cur, sink)
	if err != nil {
		return err
	}
	wm := transcript.Watermark{Identity: id, SampledAt: sampled.UnixNano(), Offset: next.Offset}
	if err := sink.Flush(&wm, next.State); err != nil {
		return err
	}
	if full {
		if _, err := a.store.SupersedeAbsent(ctx, st.ID, st.Generation); err != nil {
			return err
		}
		src.Parser = p.Name()
		if _, err := a.store.EnsureSource(ctx, src); err != nil {
			return err
		}
		d.indexedWith = p.Name()
	} else if d.indexedWith == "" {
		d.indexedWith = st.Source.Parser
	}
	a.stats.DevinPolls.Add(1)
	if a.cfg.Sync == nil {
		return nil
	}
	sessions := make([]string, 0, len(sink.touched))
	for s := range sink.touched {
		sessions = append(sessions, s)
	}
	if !d.synced {
		// First poll since start: hand every session over once, so a server
		// configured after indexing still receives them.
		all, err := devin.ListSessions(ctx, d.path)
		if err != nil {
			return err
		}
		sessions = append(sessions, all...)
		d.synced = true
	}
	sort.Strings(sessions)
	for i, s := range sessions {
		if i > 0 && sessions[i-1] == s || a.devinMode(ctx, s) != pathpolicy.Allow {
			continue
		}
		a.cfg.Sync.NotifyExportFunc(devinSpec(d.path, s), a.devinExport(d.path, s))
	}
	return nil
}

// devinCwds returns every Devin session's working directory, reading the
// store read-only.
func devinCwds(ctx context.Context, path string) (map[string]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, ifnull(working_directory, '') FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, cwd string
		if err := rows.Scan(&id, &cwd); err != nil {
			return nil, err
		}
		out[id] = cwd
	}
	return out, rows.Err()
}

func devinSpec(db, session string) devicesync.SourceSpec {
	return devicesync.SourceSpec{Path: devin.ExportPath(db, session), Agent: transcript.AgentDevin,
		StorageKind: transcript.StorageSQLite, SessionKey: session, Parser: devin.ExportFormat, Export: true}
}

// devinExport reads the session's export when the flush runs; a deleted
// session exports its "gone" record.
func (a *Agent) devinExport(db, session string) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) { return devin.Export(ctx, db, session) }
}
