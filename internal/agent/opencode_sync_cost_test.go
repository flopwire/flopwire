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
	"github.com/flopwire/flopwire/internal/transcript/opencode"
	"github.com/flopwire/flopwire/internal/transcript/opencode/opencodetest"
)

// A streaming opencode part must export and scan only its changed record,
// even when earlier parts hold megabytes of transcript data.
func TestLiveOpencodeSessionSyncCostsTheChange(t *testing.T) {
	oc := opencodetest.New(t, "")
	const big, nodes = "live-opencode", 500
	body := strings.Repeat("lorem ipsum dolor sit amet ", 160)
	oc.Session(big, "", "/work/demo", "Large session", opencodetest.T0)
	for i := range nodes {
		oc.Prompt(big, opencodetest.T0+int64(i)*3000, body)
	}
	at := opencodetest.T0 + nodes*3000
	part := oc.Prompt(big, at, "streaming")
	whole, err := opencode.Export(context.Background(), oc.Path, big)
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

	f := newFixture(t, "-")
	f.cfg.OpencodeDB = oc.Path
	f.cfg.Sync = sched
	f.a = New(f.store, f.cfg)
	f.once() // indexes the store and hands every session to sync
	settled := func(after int64) func() bool {
		return func() bool { return sy.CaptureStats().Captures > after && sched.Status().Queued == 0 }
	}
	waitFor(t, settled(0))

	for i := range 5 {
		before := sy.CaptureStats()
		oc.UpdatePart(part, at+int64(i)+1, fmt.Sprintf(`{"type":"text","text":"stream %d %s"}`, i, body))
		oc.Exec(`UPDATE session SET time_updated=? WHERE id=?`, at+int64(i)+1, big)
		f.a.opencode.last = time.Time{}
		f.a.pollStore(ctx, &f.a.opencode, true, true)
		waitFor(t, settled(before.Captures))
		after := sy.CaptureStats()
		exported, scanned := after.Exported-before.Exported, after.Scanned-before.Scanned
		t.Logf("change %d: exported %d bytes, redacted and chunked %d bytes (whole export %d)", i, exported, scanned, len(whole))
		// One part and a session record, plus at most a provisional tail
		// (shorter than a chunk) before them.
		if exported > 8<<10 {
			t.Errorf("change %d: exported %d bytes for one ~4KB part; the session's export is %d", i, exported, len(whole))
		}
		if scanned > int64(chunk.Max)+8<<10 {
			t.Errorf("change %d: redacted and chunked %d bytes for one ~4KB part; the session's export is %d", i, scanned, len(whole))
		}
	}
}
