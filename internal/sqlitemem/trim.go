// Package sqlitemem returns memory that modernc.org/sqlite's C allocator
// retains to the operating system.
//
// modernc.org/libc backs SQLite's malloc with a modernc.org/memory
// Allocator. That allocator keeps emptied regions for reuse, up to 4MiB
// plus the high-water mark of its live mapping, and on macOS its
// MADV_DONTNEED decommit does not lower the resident set. During a bulk
// index SQLite's live memory peaked near 120MB while the allocator held
// about 340MB mapped. The Allocator has a Trim method, but libc keeps the
// allocator in an unexported variable, so Trim reaches it by linkname. The
// names are pinned to the libc version in go.mod: if a libc upgrade renames
// them, the build fails at link time rather than misbehaving.
package sqlitemem

import (
	"context"
	"time"
	_ "unsafe" // go:linkname

	"modernc.org/libc"
	"modernc.org/memory"
)

//go:linkname allocator modernc.org/libc.allocator
var allocator memory.Allocator

// Trim unmaps the empty regions SQLite's allocator retains. It holds the
// allocator's lock, so every SQLite allocation waits while it runs (tens of
// microseconds for a few hundred regions).
func Trim() {
	lock()
	defer unlock()
	_ = allocator.Trim()
}

// Mapped reports the bytes the allocator has mapped; 0 unless the binary
// is built with -tags memory.counters.
func Mapped() int64 { return int64(libc.MemStat().Bytes) }

// TrimEvery calls Trim every d until ctx ends.
func TrimEvery(ctx context.Context, d time.Duration) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			Trim()
		}
	}
}
