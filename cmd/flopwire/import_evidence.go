package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/transcript"
	"golang.org/x/sys/unix"
)

type recoverySnapshot struct {
	entry    cassimport.Entry
	path     string
	identity transcript.Identity
}

// withRecoverySnapshots verifies every source into private disk-backed copies
// before invoking upload. Original paths may change afterwards; upload receives
// only the checked copies. One copy buffer and at most two descriptors are live.
func withRecoverySnapshots(ctx context.Context, root string, m cassimport.Manifest, upload func([]recoverySnapshot) error) error {
	stage, err := os.MkdirTemp("", "flopwire-cass-verified-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	seen := map[string]bool{}
	snapshots := make([]recoverySnapshot, 0, len(m.Entries))
	for _, e := range m.Entries {
		if filepath.Base(e.File) != e.File || filepath.Ext(e.File) != ".jsonl" || seen[e.File] {
			return fmt.Errorf("invalid recovery filename")
		}
		seen[e.File] = true
		path := filepath.Join(stage, e.File)
		id, err := snapshotRecovery(ctx, filepath.Join(root, e.File), path, e.SHA256)
		if err != nil {
			return err
		}
		snapshots = append(snapshots, recoverySnapshot{entry: e, path: path, identity: id})
	}
	return upload(snapshots)
}

func snapshotRecovery(ctx context.Context, original, snapshot, checksum string) (transcript.Identity, error) {
	var zero transcript.Identity
	// Check first for a useful error, then enforce it on the actual opened
	// descriptor. NOFOLLOW closes the check/open race; NONBLOCK prevents a
	// replacement FIFO from blocking before Fstat rejects it.
	info, err := os.Lstat(original)
	if err != nil {
		return zero, err
	}
	if !info.Mode().IsRegular() {
		return zero, fmt.Errorf("recovery evidence is not a regular file")
	}
	fd, err := unix.Open(original, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return zero, err
	}
	f := os.NewFile(uintptr(fd), original)
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return zero, err
	}
	if !info.Mode().IsRegular() {
		return zero, fmt.Errorf("recovery evidence is not a regular file")
	}
	id := transcript.IdentityOf(info)
	out, err := os.OpenFile(snapshot, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return zero, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), recoveryContextReader{ctx: ctx, r: f})
	closeErr := out.Close()
	if copyErr != nil {
		return zero, copyErr
	}
	if closeErr != nil {
		return zero, closeErr
	}
	if n != id.Size || hex.EncodeToString(h.Sum(nil)) != checksum {
		return zero, fmt.Errorf("recovery evidence checksum mismatch")
	}
	if err := os.Chmod(snapshot, 0400); err != nil {
		return zero, err
	}
	return id, nil
}

type recoveryContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r recoveryContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}
