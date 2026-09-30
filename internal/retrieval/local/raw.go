package local

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// ErrNotLocal is returned (wrapped) when the transcript file can no longer
// serve the requested bytes and no server is configured.
var ErrNotLocal = errors.New("the transcript file no longer holds these bytes")

// Raw returns bytes [offset, offset+length) of a local source generation,
// read from the transcript file. The file serves them while it still is the
// indexed file (same device and inode), the generation is the source's
// current one, and the range lies inside it. When the file is gone,
// replaced or truncated, the team server serves its latest copy of the
// source, found by this device's (path, file id), when one is configured.
func (b *Backend) Raw(ctx context.Context, sourceID string, generation, offset, length int64) ([]byte, error) {
	sid, err := strconv.ParseInt(sourceID, 10, 64)
	if err != nil || sid <= 0 {
		return nil, fmt.Errorf("%w: source %q is not a local id", format.ErrNotFound, sourceID)
	}
	if offset < 0 || length <= 0 || length > MaxRaw || generation < 0 {
		return nil, bad("need generation >= 0, offset >= 0 and 0 < length <= %d", MaxRaw)
	}
	st, err := b.Store.Source(ctx, sid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: source %s", format.ErrNotFound, sourceID)
	}
	if err != nil {
		return nil, err
	}
	if st.Source.StorageKind == transcript.StorageSQLite {
		return nil, bad("source %s is a database; its rows have no byte range (use read)", sourceID)
	}
	current := generation == st.Generation
	data, why := readLocal(st.Source.Path, st.Source.FileID, current, offset, length)
	if why == "" {
		return data, nil
	}
	if b.Remote == nil {
		return nil, fmt.Errorf("%w (%s: %s); configure a server to read the archived copy", ErrNotLocal, st.Source.Path, why)
	}
	if !current {
		// The server numbers generations itself; only the latest copy of
		// the file is addressable by path.
		return nil, fmt.Errorf("%w (%s: %s)", ErrNotLocal, st.Source.Path, why)
	}
	fileID := ""
	if !st.Source.FileID.IsZero() {
		fileID = st.Source.FileID.String()
	}
	data, err = b.Remote.RawByPath(ctx, st.Source.Path, fileID, -1, offset, length)
	if err != nil {
		return nil, fmt.Errorf("%s: %s; server: %w", st.Source.Path, why, err)
	}
	return data, nil
}

// rawRow returns the raw record bytes of a row.
func (b *Backend) rawRow(ctx context.Context, r *localindex.Row) ([]byte, error) {
	if r.ByteLen <= 0 {
		return nil, bad("message %d has no byte range (locator %q); read shows its text", r.ID, r.Locator)
	}
	srcs, err := b.Store.RowSources(ctx, []int64{r.ID})
	if err != nil {
		return nil, err
	}
	src := srcs[r.ID]
	return b.Raw(ctx, id(src.SourceID), src.Generation, r.ByteOffset, r.ByteLen)
}

// readLocal reads the range from path, or says why the file cannot serve it.
func readLocal(path string, want transcript.FileID, currentGen bool, offset, length int64) ([]byte, string) {
	if !currentGen {
		return nil, "that generation was rewritten"
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "file is gone"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err.Error()
	}
	if id := transcript.IdentityOf(fi).ID; !want.IsZero() && !id.IsZero() && id != want {
		return nil, "file was replaced"
	}
	if offset >= fi.Size() {
		return nil, "file was truncated"
	}
	buf := make([]byte, min(length, fi.Size()-offset))
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err.Error()
	}
	return buf[:n], ""
}
