package devin

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// countingSink tallies rows without keeping them.
type countingSink struct {
	convs, msgs, superseded int
	byKind                  map[transcript.Kind]int
	onPath                  map[string]int
	keys                    map[string]int
}

func newCountingSink() *countingSink {
	return &countingSink{byKind: map[transcript.Kind]int{}, onPath: map[string]int{}, keys: map[string]int{}}
}

func (s *countingSink) Conversation(*transcript.Conversation) error { s.convs++; return nil }
func (s *countingSink) SupersedeSession(transcript.Agent, string) error {
	s.superseded++
	return nil
}
func (s *countingSink) Message(m *transcript.Message) error {
	s.msgs++
	s.byKind[m.Kind]++
	s.onPath[onPath(m)]++
	s.keys[rowKey(m)]++
	return nil
}

// copyStore copies sessions.db and its -wal/-shm into dir. Copying a live
// store is not atomic; the parse still opens the copy read-only.
func copyStore(t *testing.T, src, dir string) string {
	t.Helper()
	dst := filepath.Join(dir, "sessions.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		in, err := os.Open(src + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(dst + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	return dst
}

// TestCorpus parses the real Devin store on this machine, read-only.
// Run: FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/devin/
// With FLOPWIRE_DEVIN_DB set it parses that file in place (point it at a
// copy); otherwise it copies the default store to a temp dir first.
func TestCorpus(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") != "1" {
		t.Skip("set FLOPWIRE_CORPUS=1 to parse the real Devin store")
	}
	path := os.Getenv(EnvDB)
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		src := DefaultPath(home)
		if _, err := os.Stat(src); err != nil {
			t.Skipf("no Devin store: %v", err)
		}
		path = copyStore(t, src, t.TempDir())
	}
	ctx := context.Background()
	p := &Parser{}
	src := &transcript.Source{Agent: transcript.AgentDevin, Path: path, StorageKind: transcript.StorageSQLite, Parser: p.Name()}

	sink := newCountingSink()
	start := time.Now()
	cur, err := p.Parse(ctx, transcript.Input{Source: src}, transcript.Cursor{}, sink)
	if err != nil {
		t.Fatal(err)
	}
	full := time.Since(start)

	idle := newCountingSink()
	start = time.Now()
	if _, err := p.Parse(ctx, transcript.Input{Source: src}, cur, idle); err != nil {
		t.Fatal(err)
	}
	idleWall := time.Since(start)

	// Graph facts, straight from the store.
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := snapshot(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	sessions, err := loadSessions(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	var nodes, keys, chainNodes, offCopies, offOnly, hidden, twinsOnChain int
	for id, s := range sessions {
		if s.hidden {
			hidden++
		}
		g, err := loadGraph(ctx, tx, id, 0, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		v := g.view(s.mainChain, 1<<62)
		chain := g.chain(s.mainChain, 1<<62)
		nodes += len(g.nodes)
		keys += len(v)
		chainNodes += len(chain)
		onKeys := map[string]int{}
		for _, n := range g.nodes {
			if chain[n.nodeID] {
				onKeys[n.key]++
			}
		}
		for _, n := range g.nodes {
			switch {
			case chain[n.nodeID]:
			case onKeys[n.key] > 0:
				offCopies++ // off-chain node whose message_id is on the chain
			default:
				offOnly++ // compacted-away, branches, summarizer trees
			}
		}
		for _, c := range onKeys {
			if c > 1 {
				twinsOnChain += c - 1
			}
		}
	}

	dups := 0
	for _, n := range sink.keys {
		if n > 1 {
			dups++
		}
	}
	t.Logf("store: %s", path)
	t.Logf("sessions %d (hidden %d), nodes %d, distinct message_ids %d (collapsed copies %d)", len(sessions), hidden, nodes, keys, nodes-keys)
	t.Logf("main-chain nodes %d, off-chain copies of on-chain ids %d, off-chain only %d, repeated ids within a chain %d", chainNodes, offCopies, offOnly, twinsOnChain)
	t.Logf("rows %d: user %d assistant %d thinking %d tool_call %d tool_result %d system %d",
		sink.msgs, sink.byKind[transcript.KindUser], sink.byKind[transcript.KindAssistant], sink.byKind[transcript.KindThinking],
		sink.byKind[transcript.KindToolCall], sink.byKind[transcript.KindToolResult], sink.byKind[transcript.KindSystem])
	t.Logf("on_active_path true %d, false %d, null %d", sink.onPath["true"], sink.onPath["false"], sink.onPath["nil"])
	t.Logf("full parse %v, idle re-parse %v (%d rows), cursor offset %d, state %d bytes", full, idleWall, idle.msgs, cur.Offset, len(cur.State))

	if sink.convs != len(sessions) {
		t.Errorf("conversations %d, sessions %d", sink.convs, len(sessions))
	}
	if dups != 0 {
		t.Errorf("%d row keys emitted more than once in one parse", dups)
	}
	if idle.msgs+idle.convs+idle.superseded != 0 {
		t.Errorf("idle re-parse emitted %d rows, %d conversations, %d supersedes", idle.msgs, idle.convs, idle.superseded)
	}
	if sink.msgs < keys || sink.onPath["true"] == 0 || sink.onPath["false"] == 0 {
		t.Errorf("implausible counts: %d rows for %d message ids", sink.msgs, keys)
	}
}
