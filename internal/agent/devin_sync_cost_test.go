package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// A live Devin session gains a node every second or two. Each flush of its
// export must export and redact what the session gained, not the session:
// a 30k-node, 255MB session was exported, redacted and chunked whole about
// every 10s and held the agent at 80-110% CPU (#150).
func TestLiveDevinSessionSyncCostsTheChange(t *testing.T) {
	path, db := buildDevin(t)
	const big, nodes = "live-big", 1500
	body := strings.Repeat("lorem ipsum dolor sit amet ", 40) // ~1KB a node
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions (id, working_directory, backend_type, model, agent_mode, created_at, last_activity_at, main_chain_id) VALUES (?, '/tmp/big', 'cli', 'swe-1-7', 'normal', 1790160000, 1790160000, ?)`, big, nodes); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= nodes; i++ {
		var parent any
		if i > 1 {
			parent = i - 1
		}
		if _, err := tx.Exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`,
			big, i, parent, fmt.Sprintf(`{"message_id":"big-%d","role":"assistant","content":"step %d %s"}`, i, i, body), 1790160000+i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	whole, err := devin.Export(context.Background(), path, big)
	if err != nil {
		t.Fatal(err)
	}

	srv := synctest.New("tok")
	h := httptest.NewServer(srv)
	defer h.Close()
	st, err := devicesync.OpenStore(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	spool, err := devicesync.OpenSpool(filepath.Join(t.TempDir(), "spool"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	chunk := devicesync.ChunkParams{Min: 4 << 10, Avg: 16 << 10, Max: 64 << 10}
	sy, err := devicesync.NewSyncer(devicesync.Config{Chunk: chunk, SealAfter: -1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		st, spool, &syncproto.Client{Server: h.URL, Token: "tok", HTTP: h.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	sched := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{Document: devicesync.Cadence{Debounce: 5 * time.Millisecond, MaxWait: 20 * time.Millisecond}})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sched.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
	}()

	f := newFixture(t, path)
	f.cfg.Sync = sched
	f.a = New(f.store, f.cfg)
	f.once() // indexes the store and hands every session to sync
	settled := func(after int64) func() bool {
		return func() bool { return sy.CaptureStats().Captures > after && sched.Status().Queued == 0 }
	}
	waitFor(t, settled(0))

	for i := range 5 {
		before := sy.CaptureStats()
		n := nodes + 1 + i
		exec := func(q string, args ...any) {
			if _, err := db.Exec(q, args...); err != nil {
				t.Fatal(err)
			}
		}
		exec(`INSERT INTO message_nodes (session_id, node_id, parent_node_id, chat_message, created_at) VALUES (?, ?, ?, ?, ?)`,
			big, n, n-1, fmt.Sprintf(`{"message_id":"big-%d","role":"assistant","content":"live %d %s"}`, n, n, body), 1790160000+n)
		exec(`UPDATE sessions SET main_chain_id = ?, last_activity_at = ? WHERE id = ?`, n, 1790160000+n, big)
		f.a.devin.last = time.Time{}
		f.a.pollDevin(ctx, true, true)
		waitFor(t, settled(before.Captures))
		after := sy.CaptureStats()
		exported, scanned := after.Exported-before.Exported, after.Scanned-before.Scanned
		t.Logf("change %d: exported %d bytes, redacted and chunked %d bytes (whole export %d)", i, exported, scanned, len(whole))
		// One node and a session record, plus at most a provisional tail
		// (shorter than a chunk) before them.
		if exported > 8<<10 {
			t.Errorf("change %d: exported %d bytes for one ~1KB node; the session's export is %d", i, exported, len(whole))
		}
		if scanned > int64(chunk.Max)+8<<10 {
			t.Errorf("change %d: redacted and chunked %d bytes for one ~1KB node; the session's export is %d", i, scanned, len(whole))
		}
	}
}
