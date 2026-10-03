package ingest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
	"github.com/flopwire/flopwire/internal/transcript/devin"
	"github.com/flopwire/flopwire/internal/transcript/opencode"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

// fixture is a copy of testdata/oracle/home plus a Devin store built from
// the oracle seed, and the device specs that sync it.
type fixture struct {
	home, devinDB string
	transcripts   []devicesync.SourceSpec // Claude and Codex transcripts
	companions    []devicesync.SourceSpec
	devinSessions []string

	opencodeDB       string
	opencodeSessions []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{home: t.TempDir()}
	copyTree(t, "../../testdata/oracle/home", f.home)
	sessions, err := claude.Discover(filepath.Join(f.home, ".claude", "projects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		for _, src := range s.Sources() {
			f.transcripts = append(f.transcripts, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentClaude,
				StorageKind: transcript.StorageJSONLAppend, SessionKey: src.SessionKey, Parser: claude.ParserName})
		}
		main := filepath.Join(s.ProjectDir, s.SessionID+".jsonl")
		for _, c := range s.Companions {
			parent := main
			if c.Role == claude.CompanionMeta {
				parent = strings.TrimSuffix(c.Path, ".meta.json") + ".jsonl"
			}
			f.companions = append(f.companions, devicesync.SourceSpec{Path: c.Path, Agent: transcript.AgentClaude,
				StorageKind: transcript.StorageCompanion, Parent: parent})
		}
	}
	codexSrcs, err := codex.Discover(filepath.Join(f.home, ".codex"))
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range codexSrcs {
		f.transcripts = append(f.transcripts, devicesync.SourceSpec{Path: src.Path, Agent: transcript.AgentCodex,
			StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name})
	}
	seed, err := os.ReadFile("../../testdata/oracle/seeds/devin.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.devinDB = filepath.Join(f.home, "sessions.db")
	db, err := sql.Open("sqlite", "file:"+f.devinDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(seed)); err != nil {
		t.Fatal(err)
	}
	if f.devinSessions, err = devin.ListSessions(context.Background(), f.devinDB); err != nil {
		t.Fatal(err)
	}
	f.opencodeDB = seedOpencode(t, f.home)
	if f.opencodeSessions, err = opencode.ListSessions(context.Background(), f.opencodeDB); err != nil {
		t.Fatal(err)
	}
	if len(f.transcripts) < 10 || len(f.companions) < 4 || len(f.devinSessions) < 2 {
		t.Fatalf("fixture: %d transcripts, %d companions, %d devin sessions", len(f.transcripts), len(f.companions), len(f.devinSessions))
	}
	return f
}

// seedOpencode writes a synthetic opencode store under home: a session
// with a tool call that spawned a subagent session.
func seedOpencode(t *testing.T, home string) string {
	oc := opencodetest.New(t, home)
	const parent, child = "ses_synthetic0000000000000P", "ses_synthetic0000000000000C"
	t0 := opencodetest.T0
	oc.Session(parent, "", "/work/oc", "Map the widget", t0)
	oc.Prompt(parent, t0, "map the widget package")
	msg := opencodetest.ID("msg", t0+1000, 1)
	oc.Message(msg, parent, t0+1000, `{"role":"assistant","path":{"cwd":"/work/oc","root":"/work/oc"}}`)
	oc.Part(opencodetest.ID("prt", t0+1001, 1), msg, parent, t0+1001, `{"type":"text","text":"Delegating the exploration."}`)
	oc.Part(opencodetest.ID("prt", t0+1002, 1), msg, parent, t0+1002, `{"type":"tool","tool":"task","callID":"call_t","state":{"status":"completed","input":{"prompt":"explore"},"output":"widget has 3 files","metadata":{"sessionId":"`+child+`"}}}`)
	oc.Session(child, parent, "/work/oc", "Explore widget (@explore subagent)", t0+1003)
	oc.Prompt(child, t0+1003, "explore the widget package")
	return oc.Path
}

func opencodeSpec(db, session string) devicesync.SourceSpec {
	return devicesync.SourceSpec{Path: opencode.ExportPath(db, session), Agent: transcript.AgentOpencode,
		StorageKind: transcript.StorageSQLite, Parser: opencode.ExportFormat, Export: true}
}

func devinSpec(db, session string) devicesync.SourceSpec {
	return devicesync.SourceSpec{Path: devin.ExportPath(db, session), Agent: transcript.AgentDevin,
		StorageKind: transcript.StorageSQLite, Parser: devin.ExportFormat, Export: true}
}

// syncAll ships every source: transcripts first, companions after, so the
// server must re-parse what a late companion feeds.
func (f *fixture) syncAll(t *testing.T, sy *devicesync.Syncer) {
	t.Helper()
	ctx := context.Background()
	for _, sp := range append(slices.Clone(f.transcripts), f.companions...) {
		if err := sy.Sync(ctx, sp); err != nil {
			t.Fatalf("sync %s: %v", sp.Path, err)
		}
	}
	for _, id := range f.devinSessions {
		data, err := devin.Export(ctx, f.devinDB, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := sy.SyncExport(ctx, devinSpec(f.devinDB, id), data); err != nil {
			t.Fatalf("sync devin %s: %v", id, err)
		}
	}
	for _, id := range f.opencodeSessions {
		data, err := opencode.Export(ctx, f.opencodeDB, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := sy.SyncExport(ctx, opencodeSpec(f.opencodeDB, id), data); err != nil {
			t.Fatalf("sync opencode %s: %v", id, err)
		}
	}
}

// want parses the fixture locally, as the device's own index would: the
// server's live rows must be exactly these.
func (f *fixture) want(t *testing.T) map[string][]string {
	t.Helper()
	ctx := context.Background()
	out := map[string][]string{}
	add := func(agent string, c *transcript.Collector) {
		last := map[string]*transcript.Message{}
		for _, m := range c.Messages {
			if !m.Superseded {
				last[m.SessionID+"|"+m.NativeID+"|"+strconv.Itoa(m.Part)+"|"+locator(m)] = m
			}
		}
		for _, m := range last {
			out[agent+"|"+m.SessionID] = append(out[agent+"|"+m.SessionID], rowString(m.NativeID, m.Part, m.Kind.String(), clean(m.Text)))
		}
	}
	for _, sp := range f.transcripts {
		fh, err := os.Open(sp.Path)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := fh.Stat()
		var p transcript.Parser = &claude.Parser{Caps: uncapped}
		if sp.Agent == transcript.AgentCodex {
			p = &codex.Parser{Caps: uncapped}
		}
		c := &transcript.Collector{}
		in := transcript.Input{Source: &transcript.Source{Agent: sp.Agent, Path: sp.Path}, R: fh, Size: st.Size()}
		if _, err := p.Parse(ctx, in, transcript.Cursor{}, c); err != nil {
			t.Fatal(err)
		}
		fh.Close()
		add(string(sp.Agent), c)
	}
	c := &transcript.Collector{}
	if _, err := (&devin.Parser{Caps: uncapped}).Parse(ctx, transcript.Input{Source: &transcript.Source{Path: f.devinDB}}, transcript.Cursor{}, c); err != nil {
		t.Fatal(err)
	}
	add("devin", c)
	c = &transcript.Collector{}
	if _, err := (&opencode.Parser{Caps: uncapped}).Parse(ctx, transcript.Input{Source: &transcript.Source{Path: f.opencodeDB}}, transcript.Cursor{}, c); err != nil {
		t.Fatal(err)
	}
	add("opencode", c)
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func rowString(native string, part int, kind, text string) string {
	return fmt.Sprintf("%s#%d %s: %s", native, part, kind, text)
}

// live returns the server's live rows per agent|session.
func (e *env) live() map[string][]string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT c.agent,c.session_id,COALESCE(m.native_id,''),m.part,m.kind,m.text
		FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE NOT m.superseded`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var agent, session, native, kind, text string
		var part int
		if err := rows.Scan(&agent, &session, &native, &part, &kind, &text); err != nil {
			e.t.Fatal(err)
		}
		out[agent+"|"+session] = append(out[agent+"|"+session], rowString(native, part, kind, text))
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}

func sameRows(t *testing.T, got, want map[string][]string) {
	t.Helper()
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s: server has %d rows, local parse %d\nserver: %q\nlocal:  %q", k, len(got[k]), len(w), got[k], w)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected session %s", k)
		}
	}
}

// A device syncs the oracle fixtures (Claude with subagents and companion
// files, Codex, Devin exports) through devicesync to the real server; the
// parsed rows equal a local parse of the same files.
func TestDeviceSyncEndToEnd(t *testing.T) {
	e := newEnv(t)
	f := newFixture(t)
	sy := e.syncer(devicesync.Config{SealAfter: -1}) // tails stay provisional
	f.syncAll(t, sy)
	e.drain()
	want := f.want(t)
	sameRows(t, e.live(), want)

	// Tails are in Postgres, never in object storage.
	if n := e.count(`SELECT count(*) FROM provisional_tails`); n == 0 {
		t.Fatal("expected provisional tails")
	}
	// Subagent links resolved late by native id.
	if n := e.count(`SELECT count(*) FROM conversations WHERE parent_native_session_id IS NOT NULL AND parent_conversation_id IS NULL`); n != 0 {
		t.Fatalf("%d unresolved subagent parents", n)
	}
	if n := e.count(`SELECT count(*) FROM conversations WHERE agent='claude' AND depth>0 AND spawned_by_message_id IS NOT NULL`); n < 3 {
		t.Fatalf("claude subagents with a spawning call: %d", n)
	}
	// Companions link to their parents; the persisted output replaced its
	// preview (the preview row is kept, superseded).
	if n := e.count(`SELECT count(*) FROM sources WHERE storage_kind='companion' AND parent_source_id IS NULL AND path NOT LIKE '%0000000f%'`); n != 0 {
		t.Fatalf("%d companions without a parent", n)
	}
	if n := e.count(`SELECT count(*) FROM messages WHERE NOT superseded AND enrichment ? 'persisted_output'`); n == 0 {
		t.Fatal("no row filled from tool-results/")
	}
	// opencode: the subagent links to its parent's task call.
	if n := e.count(`SELECT count(*) FROM conversations WHERE agent='opencode' AND depth>0 AND spawned_by_message_id IS NOT NULL`); n != 1 {
		t.Fatalf("opencode subagents with a spawning call: %d", n)
	}
	// Devin: on_active_path from main_chain_id.
	if n := e.count(`SELECT count(*) FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.agent='devin' AND m.on_active_path = false`); n == 0 {
		t.Fatal("no off-path devin rows")
	}

	// Re-syncing and re-parsing changes nothing.
	f.syncAll(t, sy)
	e.exec(`UPDATE source_parse_state SET requested_seq=requested_seq+1`)
	e.drain()
	sameRows(t, e.live(), want)
	if n := e.count(`SELECT count(*) FROM messages WHERE version>1 AND enrichment ? 'persisted_output' IS NOT TRUE`); n != 0 {
		t.Fatalf("%d spurious versions after a no-op re-parse", n)
	}
}
