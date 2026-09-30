package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
)

// TestCorpusSyncOnlySize runs one pass of a sync-only agent over this
// device's real transcripts (read-only; the index goes to a scratch
// directory) and reports the size of the bookkeeping-only index. With
// FLOPWIRE_CORPUS_FULL=1 it also builds a full index of the same corpus for
// comparison (much slower; =only builds just that one). FLOPWIRE_CORPUS_DIR
// keeps the index files there instead of a temp dir, so a full build that
// ran out of time resumes where it stopped.
//
//	FLOPWIRE_CORPUS=1 go test -run TestCorpusSyncOnlySize -v -timeout 30m ./internal/agent/
func TestCorpusSyncOnlySize(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	dir := os.Getenv("FLOPWIRE_CORPUS_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	modes := []bool{true}
	switch os.Getenv("FLOPWIRE_CORPUS_FULL") {
	case "":
	case "only": // resume a full build in FLOPWIRE_CORPUS_DIR that ran out of time
		modes = []bool{false}
	default:
		modes = append(modes, false)
	}
	for _, syncOnly := range modes {
		name := "full"
		if syncOnly {
			name = "sync-only"
		}
		db := filepath.Join(dir, name+".db")
		store, err := localindex.Open(db, localindex.Options{DeferCommit: true, ReadConns: 2, SyncOnly: syncOnly})
		if err != nil {
			t.Fatal(err)
		}
		a := New(store, Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		t0, cpu0 := time.Now(), cpuTime()
		if err := a.Once(ctx); err != nil {
			t.Fatal(err)
		}
		wall, cpu := time.Since(t0), cpuTime()-cpu0
		var srcs, convs, msgs int
		store.DB().QueryRow(`SELECT (SELECT count(*) FROM sources), (SELECT count(*) FROM conversations), (SELECT count(*) FROM messages)`).Scan(&srcs, &convs, &msgs)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		var total int64
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), name+".db") {
				if fi, err := e.Info(); err == nil {
					total += fi.Size()
				}
			}
		}
		var ru syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		rss := ru.Maxrss
		if runtime.GOOS == "linux" {
			rss *= 1024
		}
		t.Logf("%s: %d sources, %d conversations, %d messages; index files %.1fMB; pass %s wall, %s CPU; process peak RSS so far %.0fMB",
			name, srcs, convs, msgs, float64(total)/1e6, wall.Round(time.Second), cpu.Round(time.Second), float64(rss)/1e6)
		if syncOnly && msgs != 0 {
			t.Errorf("sync-only index holds %d messages", msgs)
		}
	}
}
