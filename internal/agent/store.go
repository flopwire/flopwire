package agent

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
)

// storeHarness is a harness whose sessions all live in one SQLite store
// (Devin's sessions.db, opencode's opencode.db): read through a parser
// with per-session watermarks, and synced as one export per session.
type storeHarness struct {
	agent     transcript.Agent
	newParser func() transcript.Parser
	list      func(ctx context.Context, db string) ([]string, error)
	cwds      func(ctx context.Context, db string) (map[string]string, error)
	// export returns a session's export, appended to the one prev
	// describes when it can (devicesync.ExportFunc).
	export       func(ctx context.Context, db, session string, prev []byte) (devicesync.Export, error)
	exportPath   func(db, session string) string
	exportFormat string
}

var devinHarness = storeHarness{
	agent:        transcript.AgentDevin,
	newParser:    func() transcript.Parser { return &devin.Parser{} },
	list:         devin.ListSessions,
	cwds:         devinCwds,
	export:       devinExport,
	exportPath:   devin.ExportPath,
	exportFormat: devin.ExportFormat,
}

var opencodeHarness = storeHarness{
	agent:        transcript.AgentOpencode,
	newParser:    func() transcript.Parser { return &opencode.Parser{} },
	list:         opencode.ListSessions,
	cwds:         opencode.Cwds,
	export:       opencodeExport,
	exportPath:   opencode.ExportPath,
	exportFormat: opencode.ExportFormat,
}

// storeState is the poll gate for one harness store: the identity of the
// database file and of its -wal at the last parse. Both harnesses commit
// to the WAL, so a change shows in one of the two stats.
type storeState struct {
	h        storeHarness
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
	// modes caches each session's path rules verdict; guarded by Agent.mu.
	modes map[string]pathpolicy.Mode
	polls atomic.Int64
	// parser is kept across incremental polls (guarded by mu): the Devin
	// parser keeps the graphs of live sessions, so a poll reads only their
	// new rows instead of every row of a session that is still growing.
	parser transcript.Parser
}

// stores are the harness stores this device reads.
func (a *Agent) stores() []*storeState {
	var out []*storeState
	for _, d := range []*storeState{&a.devin, &a.opencode} {
		if d.path != "" {
			out = append(out, d)
		}
	}
	return out
}

// storeOf is the store of a harness, or nil.
func (a *Agent) storeOf(agent transcript.Agent) *storeState {
	for _, d := range a.stores() {
		if d.h.agent == agent {
			return d
		}
	}
	return nil
}

// minStorePoll bounds how often a busy store is re-parsed.
const minStorePoll = time.Second

// pollStores polls every harness store (pollStore).
func (a *Agent) pollStores(ctx context.Context, force, wait bool) {
	for _, d := range a.stores() {
		a.pollStore(ctx, d, force, wait)
	}
}

// pollDevin polls Devin's store.
func (a *Agent) pollDevin(ctx context.Context, force, wait bool) {
	a.pollStore(ctx, &a.devin, force, wait)
}

// pollStore brings the index up to date with a harness store when its stat
// tuple moved (or force), through the parser's incremental cursor: per
// session watermarks in the cursor state, deleted sessions superseded
// through the sink. Changed sessions are handed to sync as per-session
// exports (devin-export@2, opencode-export@1), a deleted one as its "gone"
// record. With wait it waits for a poll already running instead of
// skipping.
func (a *Agent) pollStore(ctx context.Context, d *storeState, force, wait bool) {
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
	if !force && time.Since(d.last) < minStorePoll {
		return
	}
	db, err := transcript.StatIdentity(d.path)
	if err != nil {
		return // the harness is not on this device
	}
	wal, _ := transcript.StatIdentity(d.path + "-wal")
	name := d.h.newParser().Name()
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
			if err := a.parseStore(ctx, d, db, true); err != nil {
				if ctx.Err() == nil {
					d.reparseFailed = name
					a.stats.Errors.Add(1)
					a.log.Warn("agent: store re-parse failed; keeping rows", "agent", d.h.agent, "path", d.path, "parser", name, "err", err)
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
	if err := a.parseStore(ctx, d, db, false); err != nil {
		if ctx.Err() == nil {
			a.stats.Errors.Add(1)
			a.log.Warn("agent: store", "agent", d.h.agent, "path", d.path, "err", err)
		}
		return
	}
	d.db, d.wal = db, wal
}

// storeSink records which sessions a parse touched, and drops what the
// path rules deny (D18).
type storeSink struct {
	*localindex.Sink
	touched map[string]bool
	deny    func(session, cwd string) bool // nil: no rules
	reads   readSightings
}

func (s *storeSink) denied(session, cwd string) bool { return s.deny != nil && s.deny(session, cwd) }

func (s *storeSink) Conversation(c *transcript.Conversation) error {
	if s.denied(c.SessionID, c.Cwd) {
		return nil
	}
	s.touched[c.SessionID] = true
	return s.Sink.Conversation(c)
}

func (s *storeSink) Message(m *transcript.Message) error {
	if s.denied(m.SessionID, "") {
		return nil
	}
	s.touched[m.SessionID] = true
	s.reads.note(m)
	return s.Sink.Message(m)
}

func (s *storeSink) SupersedeSession(agent transcript.Agent, id string) error {
	if s.denied(id, "") {
		return nil
	}
	s.touched[id] = true
	return s.Sink.SupersedeSession(agent, id)
}

var _ transcript.SessionSuperseder = (*storeSink)(nil)

// parseStore parses what changed in a harness store. full re-parses the
// whole store as a new generation (a parser version change, D16): a dry run
// first, then every row again, then rows the new parse did not emit are
// superseded and the source records the new parser version.
func (a *Agent) parseStore(ctx context.Context, d *storeState, id transcript.Identity, full bool) error {
	p := d.parser
	if full || p == nil {
		p = d.h.newParser()
	}
	if !full {
		d.parser = p
	}
	sampled := time.Now()
	var st localindex.SourceState
	var err error
	if d.sourceID != 0 {
		st, err = a.store.Source(ctx, d.sourceID)
	} else {
		// One source per store path; the file identity is informational
		// (the parser reads rows, not bytes).
		st, err = a.store.EnsureSource(ctx, transcript.Source{Agent: d.h.agent, Path: d.path, FileID: id.ID,
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
	sink := &storeSink{Sink: a.store.NewSink(ctx, st.ID, st.Generation), touched: map[string]bool{}, reads: readSightings{agent: d.h.agent}}
	if pv := a.policy(); !pv.pol.Empty() {
		a.loadStoreModes(ctx, d, pv)
		sink.deny = func(session, cwd string) bool {
			a.mu.Lock()
			m, ok := d.modes[session]
			a.mu.Unlock()
			if !ok { // a session newer than the directory listing
				m = a.storeDecide(d, pv, session, cwd).Mode
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
	a.markRead(ctx, &sink.reads)
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
	d.polls.Add(1)
	if d.h.agent == transcript.AgentDevin {
		a.stats.DevinPolls.Add(1)
	}
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
		all, err := d.h.list(ctx, d.path)
		if err != nil {
			return err
		}
		sessions = append(sessions, all...)
		d.synced = true
	}
	sort.Strings(sessions)
	for i, s := range sessions {
		if i > 0 && sessions[i-1] == s || a.storeMode(ctx, d, s) != pathpolicy.Allow {
			continue
		}
		sp := d.spec(s)
		if p, ok := a.storedPlace(placeKey{d.h.agent, s}); ok {
			sp.Checkout, sp.Remote = p.pl.Main, p.pl.Remote
		}
		a.cfg.Sync.NotifyExportFunc(sp, d.exportFn(s))
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
	if _, err := fsprobe.Stat(abs); err != nil {
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

// spec is the sync source of one session's export.
func (d *storeState) spec(session string) devicesync.SourceSpec {
	return devicesync.SourceSpec{Path: d.h.exportPath(d.path, session), Agent: d.h.agent,
		StorageKind: transcript.StorageSQLite, SessionKey: session, Parser: d.h.exportFormat, Export: true}
}

// exportFn reads the session's export when the flush runs; a deleted
// session exports its "gone" record.
func (d *storeState) exportFn(session string) devicesync.ExportFunc {
	return func(ctx context.Context, prev []byte) (devicesync.Export, error) {
		return d.h.export(ctx, d.path, session, prev)
	}
}

// devinExport appends to a Devin session's export (devin-export@2): a
// live session syncs what it gained, not its whole history.
func devinExport(ctx context.Context, db, session string, prev []byte) (devicesync.Export, error) {
	data, appended, state, err := devin.ExportFrom(ctx, db, session, prev)
	return devicesync.Export{Data: data, Append: appended, State: state}, err
}

// opencodeExport is an opencode session's whole export.
func opencodeExport(ctx context.Context, db, session string, _ []byte) (devicesync.Export, error) {
	data, err := opencode.Export(ctx, db, session)
	return devicesync.Export{Data: data}, err
}
