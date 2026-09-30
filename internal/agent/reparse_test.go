package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// versioned stands in for a parser version bump that changes native ids
// (as D16's stable Claude ids do): another name, ids tagged with it.
type versioned struct {
	transcript.Parser
	name string
	fail bool
}

func (v *versioned) Name() string { return v.name }

func (v *versioned) Parse(ctx context.Context, in transcript.Input, cur transcript.Cursor, sink transcript.Sink) (transcript.Cursor, error) {
	if v.fail {
		return cur, errors.New("parser bug")
	}
	return v.Parser.Parse(ctx, in, cur, &retag{Sink: sink, tag: v.name})
}

type retag struct {
	transcript.Sink
	tag string
}

func (r *retag) Message(m *transcript.Message) error {
	if m.NativeID != "" {
		m.NativeID += "#" + r.tag
	}
	m.Parser = r.tag
	return r.Sink.Message(m)
}

// exec writes to the index through a connection of its own (the store's
// reader is read-only), as an older agent would have left it.
func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if err := f.store.Sync(ctx); err != nil {
		f.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+f.store.Path()+"?_pragma=busy_timeout(10000)")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

// run starts Run and returns its stop function.
func (f *fixture) run() func() {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	return func() { cancel(); <-done }
}

// D16: a parser version change re-parses unchanged sources from their
// bytes in the background: new rows replace the old ones, which become
// superseded, and the source records the new version. A pass (Once) does
// not wait for it. A source whose file is gone keeps its rows.
func TestParserVersionChangeReparsesInBackground(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	waitRacy()
	f.once() // settles the racy entries, so the next passes see no change
	liveClaude := `SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.agent = 'claude' AND m.superseded = 0`
	before := f.count(liveClaude)
	gone := f.path(codexActive)
	goneRows := f.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.path = ? AND m.superseded = 0`, gone)
	if before == 0 || goneRows == 0 {
		t.Fatal("fixture has no rows")
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	claudeFiles := f.count(`SELECT count(*) FROM sources WHERE agent = 'claude' AND storage_kind = 'jsonl_append' AND wm_offset IS NOT NULL`)

	bump := func() {
		f.restart()
		f.a.claude = &versioned{Parser: f.a.claude, name: "claude@next"}
		f.a.codex = &versioned{Parser: f.a.codex, name: "codex@next"}
	}
	bump()
	f.once()
	if n := f.count(`SELECT count(*) FROM sources WHERE parser LIKE '%@next'`); n != 0 {
		t.Fatalf("a pass re-parsed %d unchanged sources in the foreground", n)
	}

	bump()
	stop := f.run()
	waitFor(t, func() bool {
		return f.count(`SELECT count(*) FROM sources WHERE parser = 'claude@next'`) == claudeFiles
	})
	stop()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(liveClaude + ` AND m.native_id NOT LIKE '%#claude@next'`); n != 0 {
		t.Errorf("%d rows of the old parser still live", n)
	}
	if n := f.count(liveClaude); n != before {
		t.Errorf("live Claude rows %d after re-parse, want %d", n, before)
	}
	if n := f.count(`SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.path = ? AND m.superseded = 0`, gone); n != goneRows {
		t.Errorf("source whose file is gone: %d live rows, want %d", n, goneRows)
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE path = ? AND parser LIKE '%@next'`, gone); n != 0 {
		t.Error("source whose file is gone recorded as re-parsed")
	}
}

// D16: a re-parse that fails leaves the rows and the recorded version as
// they were, and is not retried in a loop.
func TestFailedReparseKeepsRows(t *testing.T) {
	f := newFixture(t, "-")
	f.once()
	live := `SELECT count(*) FROM messages WHERE superseded = 0`
	before := f.count(live)
	claudeFiles := int64(f.count(`SELECT count(*) FROM sources WHERE agent = 'claude' AND storage_kind = 'jsonl_append' AND wm_offset IS NOT NULL`))
	f.restart()
	f.a.claude = &versioned{Parser: f.a.claude, name: "claude@broken", fail: true}
	stop := f.run()
	waitFor(t, func() bool { return f.a.stats.Errors.Load() >= claudeFiles })
	f.a.WaitIdle()
	stop()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.count(live); n != before {
		t.Errorf("live rows %d after a failed re-parse, want %d", n, before)
	}
	if n := f.count(`SELECT count(*) FROM sources WHERE parser = 'claude@broken'`); n != 0 {
		t.Errorf("%d sources recorded the broken version", n)
	}
	if n := f.a.stats.Errors.Load(); n != claudeFiles {
		t.Errorf("errors %d, want one per Claude source (%d)", n, claudeFiles)
	}
}

// D16 for Devin's store: a version change re-parses it whole as a new
// generation in the background.
func TestDevinParserVersionChange(t *testing.T) {
	path, _ := buildDevin(t)
	f := newFixture(t, path)
	f.once()
	live := `SELECT count(*) FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.agent = 'devin' AND m.superseded = 0`
	before := f.count(live)
	f.exec(`UPDATE sources SET parser = 'devin@0' WHERE agent = 'devin'`)
	gen := f.count(`SELECT generation FROM sources WHERE agent = 'devin'`)
	f.restart()
	stop := f.run()
	name := (&devin.Parser{}).Name()
	waitFor(t, func() bool {
		return f.count(`SELECT count(*) FROM sources WHERE agent = 'devin' AND parser = ?`, name) == 1
	})
	stop()
	if err := f.store.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if g := f.count(`SELECT generation FROM sources WHERE agent = 'devin'`); g != gen+1 {
		t.Errorf("generation %d, want %d", g, gen+1)
	}
	if n := f.count(live); n != before {
		t.Errorf("live Devin rows %d, want %d", n, before)
	}
	if !strings.HasPrefix(name, "devin@") {
		t.Fatal(name)
	}
}
