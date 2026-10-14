package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/minio/minio-go/v7"
)

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func TestCopyObjectsBoundAndStableManifest(t *testing.T) {
	entries := []Entry{{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}}
	started := make(chan string, len(entries))
	release := make(chan struct{})
	var active, peak atomic.Int32
	result := make(chan error, 1)
	go func() {
		result <- copyObjects(t.Context(), entries, 2, func(ctx context.Context, e Entry) (string, error) {
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			defer active.Add(-1)
			started <- e.Key
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "sum-" + e.Key, nil
		})
	}()
	receive(t, started)
	receive(t, started)
	if active.Load() != 2 {
		t.Fatalf("active=%d", active.Load())
	}
	select {
	case extra := <-started:
		t.Fatalf("worker bound exceeded: %s", extra)
	default:
	}
	close(release)
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak=%d", peak.Load())
	}
	for i, e := range entries {
		if e.Key != string(rune('a'+i)) || e.SHA256 != "sum-"+e.Key {
			t.Fatalf("entry %d=%+v", i, e)
		}
	}
}

func TestCopyObjectsOutOfOrderCompletionKeepsHashesAtTheirIndexes(t *testing.T) {
	entries := []Entry{{Key: "a"}, {Key: "b"}}
	started := make(chan string, 2)
	releaseA := make(chan struct{})
	completedB := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- copyObjects(t.Context(), entries, 2, func(ctx context.Context, e Entry) (string, error) {
			started <- e.Key
			if e.Key == "a" {
				<-releaseA
			} else {
				close(completedB)
			}
			return "sum-" + e.Key, nil
		})
	}()
	receive(t, started)
	receive(t, started)
	receive(t, completedB)
	close(releaseA)
	if err := receive(t, result); err != nil {
		t.Fatal(err)
	}
	if entries[0].Key != "a" || entries[0].SHA256 != "sum-a" || entries[1].Key != "b" || entries[1].SHA256 != "sum-b" {
		t.Fatalf("entries=%+v", entries)
	}
}

func TestCopyObjectsFailureCancelsAndJoins(t *testing.T) {
	entries := []Entry{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	started := make(chan string, 2)
	fail := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	want := errors.New("injected copy failure")
	go func() {
		result <- copyObjects(t.Context(), entries, 2, func(ctx context.Context, e Entry) (string, error) {
			started <- e.Key
			if e.Key == "a" {
				<-fail
				return "", want
			}
			<-ctx.Done()
			close(canceled)
			<-release
			return "", ctx.Err()
		})
	}()
	receive(t, started)
	receive(t, started)
	close(fail)
	receive(t, canceled)
	select {
	case err := <-result:
		t.Fatalf("returned before sibling stopped: %v", err)
	default:
	}
	close(release)
	if err := receive(t, result); !errors.Is(err, want) {
		t.Fatalf("error=%v", err)
	}
	select {
	case key := <-started:
		t.Fatalf("dispatched after failure: %s", key)
	default:
	}
}

func TestCopyObjectsParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- copyObjects(ctx, []Entry{{Key: "a"}}, 1, func(ctx context.Context, e Entry) (string, error) { close(started); <-ctx.Done(); return "", ctx.Err() })
	}()
	receive(t, started)
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestCopyObjectsRejectsAliasesBeforeCopy(t *testing.T) {
	for _, key := range []string{"../escape", "chunks/../a", "a//b", "./a"} {
		t.Run(key, func(t *testing.T) {
			err := copyObjects(t.Context(), []Entry{{Key: "a"}, {Key: key}}, 2, func(context.Context, Entry) (string, error) {
				t.Error("copy ran before inventory validation")
				return "", nil
			})
			if err == nil {
				t.Fatal("accepted unsafe or aliased key")
			}
		})
	}
}

type fakeDownloader func(context.Context, string, string, string, minio.GetObjectOptions) error

func (f fakeDownloader) FGetObject(ctx context.Context, bucket, key, path string, opts minio.GetObjectOptions) error {
	return f(ctx, bucket, key, path, opts)
}

func TestCopyObjectVerifiesBeforeDurabilityAndReturnsHashAfterSync(t *testing.T) {
	raw := []byte("synthetic backup chunk")
	z := syncproto.Compress(nil, raw)
	entry := Entry{Key: "chunks/ab/test", Size: int64(len(z)), RawSize: int64(len(raw)), BLAKE3: syncproto.Sum(raw).String()}
	download := fakeDownloader(func(_ context.Context, _, _, path string, _ minio.GetObjectOptions) error {
		return os.WriteFile(path, z, 0600)
	})
	for _, tc := range []struct {
		name    string
		mutate  func(*Entry)
		syncErr error
	}{
		{name: "valid"},
		{name: "wrong stored size", mutate: func(e *Entry) { e.Size++ }},
		{name: "wrong content address", mutate: func(e *Entry) { e.BLAKE3 = "bad" }},
		{name: "sync failure", syncErr: errors.New("injected fsync failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := entry
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			called := false
			output := t.TempDir()
			sum, err := copyObject(t.Context(), download, "synthetic", output, e, func(path string) error {
				called = true
				if path != filepath.Join(output, "objects", filepath.FromSlash(e.Key)) {
					return fmt.Errorf("wrong sync target %s", path)
				}
				if tc.syncErr != nil {
					return tc.syncErr
				}
				return syncFileAndDir(path)
			})
			if tc.mutate != nil {
				if err == nil || called || sum != "" {
					t.Fatalf("bad object sync=%v sum=%q err=%v", called, sum, err)
				}
				return
			}
			if !called {
				t.Fatal("durability step skipped")
			}
			if tc.syncErr != nil {
				if !errors.Is(err, tc.syncErr) || sum != "" {
					t.Fatalf("sum=%q error=%v", sum, err)
				}
				return
			}
			hash := sha256.Sum256(z)
			if err != nil || sum != hex.EncodeToString(hash[:]) {
				t.Fatalf("sum=%q error=%v", sum, err)
			}
		})
	}
}

func TestCreateRejectsInvalidWorkerCountBeforeOutputMutation(t *testing.T) {
	for _, workers := range []int{-1, MaxObjectWorkers + 1} {
		output := filepath.Join(t.TempDir(), "backup")
		_, err := Create(t.Context(), "unused", nil, "unused", output, Options{EncryptedDestination: true, ObjectWorkers: workers})
		if err == nil {
			t.Fatalf("accepted workers=%d", workers)
		}
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("output mutated: %v", err)
		}
	}
}
