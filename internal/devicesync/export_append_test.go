package devicesync

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/flopwire/flopwire/internal/redact"
	"github.com/flopwire/flopwire/internal/transcript"
)

// growingExport is an exporter whose export only grows: its state is the
// length exported, and an append is what was added since.
type growingExport struct{ log []byte }

func (g *growingExport) fn(_ context.Context, prev []byte) (Export, error) {
	state := []byte(strconv.Itoa(len(g.log)))
	if prev == nil {
		return Export{Data: g.log, State: state}, nil
	}
	n, err := strconv.Atoi(string(prev))
	if err != nil {
		return Export{}, err
	}
	return Export{Data: g.log[n:], Append: true, State: state}, nil
}

// An appending exporter's sync costs the append and the provisional tail
// before it, across acknowledgements and a restart; the server holds the
// export redacted exactly as a whole capture would redact it. A lost tail
// starts a new generation from the whole export.
func TestAppendingExport(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 4<<20)
	ctx := context.Background()
	sp := SourceSpec{Path: "devin:sessions.db#live", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", Export: true}
	g := &growingExport{log: jsonlLines(41, 300, 150)}
	planted := bytes.SplitAfter(fixture(t, "devin-export.jsonl", 1), []byte("\n"))
	redacted := func() []byte {
		out := make([]byte, len(g.log))
		if _, err := redact.NewReaderAt(bytes.NewReader(g.log), redact.ModeFor(string(sp.StorageKind), sp.Path)).ReadAt(out, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		return out
	}
	sync := func(name string, wantExported int) {
		t.Helper()
		before := e.sy.CaptureStats()
		if err := e.sy.SyncExportFunc(ctx, sp, g.fn); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, e.logs)
		}
		after := e.sy.CaptureStats()
		if got := after.Exported - before.Exported; got != int64(wantExported) {
			t.Errorf("%s: exported %d bytes, want %d", name, got, wantExported)
		}
		if scanned := after.Scanned - before.Scanned; wantExported < len(g.log) && scanned > int64(small.Max+wantExported) {
			t.Errorf("%s: redacted and chunked %d bytes for a %d-byte append", name, scanned, wantExported)
		}
	}
	add := func(b []byte) int { g.log = append(g.log, b...); return len(b) }

	sync("first", len(g.log))
	for i, line := range planted {
		if len(line) > 0 {
			sync("append "+strconv.Itoa(i), add(line))
		}
	}
	e.requireServerHas(sp.Path, "", 0, redacted())
	e.requireRedacted(sp.Path, "", g.log, true)
	if e.spool.Used() > int64(small.Max) {
		t.Fatalf("spool holds %d bytes after every ack; only the tail is kept", e.spool.Used())
	}

	// A restarted agent appends from the saved state and the kept tail.
	e.sy.Close()
	sy, err := NewSyncer(Config{Chunk: small, SealAfter: -1, Logger: slog.New(slog.NewTextHandler(e.logs, nil))}, e.store, e.spool, e.client)
	if err != nil {
		t.Fatal(err)
	}
	e.sy = sy
	sync("after restart", add(jsonlLines(42, 3, 150)))
	sync("unchanged", 0)
	e.requireServerHas(sp.Path, "", 0, redacted())

	// The server is down for an append: it uploads later from the spool.
	e.srv.SetDown(true)
	add(jsonlLines(43, 2, 150))
	if err := e.sy.SyncExportFunc(ctx, sp, g.fn); err == nil {
		t.Fatal("want an error while the server is down")
	}
	e.srv.SetDown(false)
	sync("after outage", add(jsonlLines(46, 1, 150)))
	e.requireServerHas(sp.Path, "", 0, redacted())

	// The tail is gone from the spool: the whole export starts generation 1.
	src, err := e.store.source(ctx, sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	tailGen, err := e.store.gen(ctx, src.ID, src.Gen)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.spool.DropTailVersion(src.ID, src.Gen, tailGen.Tail.Hash); err != nil {
		t.Fatal(err)
	}
	n := add(jsonlLines(44, 1, 150))
	sync("lost tail", n+len(g.log)) // the append it could not use, then the whole export
	e.requireServerHas(sp.Path, "", 1, redacted())
	sync("append to generation 1", add(jsonlLines(45, 2, 150)))
	e.requireServerHas(sp.Path, "", 1, redacted())
}

// An append redacts on its own only from a line start. An exporter whose
// append ends inside a line (here a token split across two appends) must
// still have the token redacted on the server, as a whole capture would.
func TestAppendingExportSplitLineRedacted(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 4<<20)
	ctx := context.Background()
	sp := SourceSpec{Path: "devin:sessions.db#split", Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", Export: true}
	g := &growingExport{log: jsonlLines(47, 20, 150)}
	token := "ghp_" + "0123456789abcdefghijABCDEFGHIJ012345"
	line := []byte(`{"t":"node","text":"token ` + token + ` here"}` + "\n")
	cut := bytes.Index(line, []byte(token)) + 10
	for _, part := range [][]byte{nil, line[:cut], line[cut:]} {
		g.log = append(g.log, part...)
		if err := e.sy.SyncExportFunc(ctx, sp, g.fn); err != nil {
			t.Fatalf("%v\n%s", err, e.logs)
		}
	}
	src, err := e.store.source(ctx, sp.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.srv.Reconstruct(sp.Path, "", src.Gen)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(token)) {
		t.Fatalf("a token split across two appends reached the server unredacted")
	}
}
