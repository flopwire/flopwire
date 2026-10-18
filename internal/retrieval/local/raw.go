package local

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/transcript"
)

// ErrNotLocal is returned (wrapped) when the transcript file can no longer
// serve the requested indexed bytes with verified identity.
var ErrNotLocal = errors.New("the transcript file no longer holds these bytes")

// Raw returns bytes [offset, offset+length) of a local source generation.
// Reads verify a persisted content hash, or require an unchanged non-racy
// indexed file tuple before and after the read.
// Archive generations are independent of local generations, so a path lookup
// cannot safely substitute archived bytes for a local byte range.
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
	data, why := readLocal(st.Source.Path, st.Source.FileID, st.Watermark, current, offset, length)
	if why == "" {
		return data, nil
	}
	return nil, fmt.Errorf("%w (%s: %s); read archived evidence using a server address", ErrNotLocal, st.Source.Path, why)
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
func readLocal(path string, want transcript.FileID, wm *transcript.Watermark, currentGen bool, offset, length int64) ([]byte, string) {
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
	before := transcript.IdentityOf(fi)
	if wm == nil {
		return nil, "source has no indexed watermark"
	}
	if before.ID.IsZero() || before.ID != wm.Identity.ID {
		return nil, "indexed file identity is unavailable or changed"
	}
	if offset >= wm.Offset || length > wm.Offset-offset {
		return nil, "range extends beyond indexed bytes"
	}
	if offset >= fi.Size() {
		return nil, "file was truncated"
	}
	if before.CTime == 0 || before != wm.Identity || wm.Racy() {
		// Changed or untrustworthy metadata cannot establish identity.
		// Return bytes from the buffer whose indexed digest we verify.
		base, size, sum := int64(0), wm.HeadLen, wm.HeadHash
		if offset+length > size {
			base, size, sum = wm.Offset-wm.AnchorLen, wm.AnchorLen, wm.AnchorSum
		}
		if offset < base || offset+length > base+size {
			return nil, "no indexed content hash covers this range; wait for a stable index sweep"
		}
		region := make([]byte, size)
		if _, err := f.ReadAt(region, base); err != nil || sha256.Sum256(region) != sum {
			return nil, "indexed bytes changed"
		}
		return region[offset-base : offset-base+length], ""
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil {
		return nil, err.Error()
	}
	after, err := f.Stat()
	if err != nil || transcript.IdentityOf(after) != before {
		return nil, "file changed during the read"
	}
	return buf, ""
}
