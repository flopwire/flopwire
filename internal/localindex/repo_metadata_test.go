package localindex

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestReadRepoDirsWithoutContentIndex(t *testing.T) {
	for _, minimal := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "sync-only"}[minimal], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "index.db")
			s, err := Open(path, Options{SyncOnly: minimal})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := []RepoDir{{Dir: "/deleted/worktree", Main: "/main", Remote: "github.test/team/app"}}
			if err = s.SavePlacement(t.Context(), Placement{Agent: transcript.AgentClaude, SessionID: "synthetic", How: PlacedByWorktree, Placement: pathpolicy.Placement{Worktree: want[0].Dir, Main: want[0].Main, Remote: want[0].Remote}}); err != nil {
				t.Fatal(err)
			}
			// Read alongside an active writer; no second writer lock or shard needed.
			got, err := ReadRepoDirs(t.Context(), path)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, %v", got, err)
			}
			if minimal {
				if r, err := Open(path, Options{ReadOnly: true}); !errors.Is(err, ErrSyncOnly) {
					if r != nil {
						r.Close()
					}
					t.Fatalf("content open: %v", err)
				}
			}
		})
	}
}

func TestReadRepoDirsMissingIndexDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := ReadRepoDirs(t.Context(), path); err == nil {
		t.Fatal("missing index accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata read created index: %v", err)
	}
}

func TestReadRepoDirsRejectsUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path, Options{SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.write(t.Context(), func(w *writeTx) error { _, err := w.exec("PRAGMA user_version=0"); return err }); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRepoDirs(t.Context(), path); err == nil {
		t.Fatal("unknown schema accepted")
	}
}
