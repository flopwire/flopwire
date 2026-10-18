package devicesync

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// tailFixture is an opened store with one committed tail per added source and
// an isolated spool, for tests that decide tail retention from committed state.
type tailFixture struct {
	dbPath string
	store  *Store
	spool  *Spool
}

func newTailFixture(t *testing.T) tailFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	spool := publicationSpool(t, 4096)
	if _, err := store.db.Exec(`CREATE TABLE untouched(value TEXT); INSERT INTO untouched VALUES ('literal retained state')`); err != nil {
		t.Fatal(err)
	}
	return tailFixture{path, store, spool}
}

func (f tailFixture) add(t *testing.T, kind transcript.StorageKind, export bool, body []byte, state []byte, acked bool) (int64, syncproto.Hash) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "synthetic-source")
	src, err := f.store.source(ctx, path, &SourceSpec{Path: path, StorageKind: kind, Agent: transcript.AgentClaude, SessionKey: "literal-fixture", Parser: "claude:v1", Export: export})
	if err != nil {
		t.Fatal(err)
	}
	hash := syncproto.Sum(body)
	g := &genRow{SourceID: src.ID, Gen: 0, FileID: "literal-inode", Size: int64(len(body)), Tail: syncproto.Tail{Size: int64(len(body)), Hash: hash}, TailAcked: acked}
	wm := &transcript.Watermark{Offset: int64(len(body))}
	if err := f.store.saveCapture(ctx, src, g, nil, wm, state); err != nil {
		t.Fatal(err)
	}
	return src.ID, hash
}

func readOnlyIndexDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	u.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}
