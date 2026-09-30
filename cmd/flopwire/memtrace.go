package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// memTrace appends one line per interval to path while ctx lives: seconds
// since start, process RSS (ps), the Go heap in use, the Go memory the OS
// still backs (Sys minus released), and the bytes SQLite's allocator has
// mapped. The last is 0 unless the binary is built with -tags
// memory.counters. `flopwire bench acceptance` and the memory tuning in A4
// read it; FLOPWIRE_MEMTRACE=<file> turns it on.
func memTrace(ctx context.Context, path string, every time.Duration) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, "t_s\trss_mb\tgo_heap_inuse_mb\tgo_backed_mb\tsqlite_mapped_mb\tsqlite_used_mb")
	tls := libc.NewTLS()
	start := time.Now()
	tick := time.NewTicker(every)
	defer tick.Stop()
	pid := strconv.Itoa(os.Getpid())
	for {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		rss := 0.0
		if out, err := exec.Command("ps", "-o", "rss=", "-p", pid).Output(); err == nil {
			kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
			rss = kb / 1024
		}
		fmt.Fprintf(f, "%.1f\t%.0f\t%d\t%d\t%d\t%d\n", time.Since(start).Seconds(), rss, ms.HeapInuse>>20,
			(ms.Sys-ms.HeapReleased)>>20, libc.MemStat().Bytes>>20, sqlite3.Xsqlite3_memory_used(tls)>>20)
		if hp := os.Getenv("FLOPWIRE_MEMTRACE_HEAP"); hp != "" && time.Since(start) > 60*time.Second {
			if hf, err := os.Create(hp); err == nil {
				pprof.WriteHeapProfile(hf)
				hf.Close()
			}
			os.Unsetenv("FLOPWIRE_MEMTRACE_HEAP")
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// enableSQLiteMemStatus turns on SQLite's memory accounting, which the
// modernc build leaves off; it must run before the first connection opens.
func enableSQLiteMemStatus() {
	tls := libc.NewTLS()
	defer tls.Close()
	va := libc.NewVaList(int32(1))
	defer libc.Xfree(tls, va)
	sqlite3.Xsqlite3_config(tls, sqlite3.SQLITE_CONFIG_MEMSTATUS, va)
}
