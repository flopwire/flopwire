package devicesync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func requireReferenceOwnerHeld(t *testing.T, spool *Spool) {
	t.Helper()
	if spool.referenceMu.TryLock() {
		spool.referenceMu.Unlock()
		t.Fatal("publication/reference transition does not hold the common owner")
	}
}

func awaitReferenceResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("reference operation did not complete")
		return nil
	}
}

func referenceSource(t *testing.T, store *Store) *sourceRow {
	t.Helper()
	path := filepath.Join(t.TempDir(), "literal-export")
	spec := SourceSpec{Path: path, Agent: transcript.AgentCodex, StorageKind: transcript.StorageJSONDoc, Export: true, Parser: "codex@1"}
	src, err := store.source(t.Context(), path, &spec)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func requireOwnedChunk(t *testing.T, spool *Spool, body []byte, present bool) {
	t.Helper()
	got, found, err := spool.Chunk(syncproto.Sum(body))
	if err != nil || found != present || present && !bytes.Equal(got, body) {
		t.Fatalf("chunk present=%v error=%v body=%q", found, err, got)
	}
}

func TestSpoolReferencesPublicationThroughCommitOrAbort(t *testing.T) {
	for _, mode := range []string{"guard-reject", "commit", "commit-then-error"} {
		t.Run(mode, func(t *testing.T) {
			commit := mode != "guard-reject"
			f := newTailPreparationFixture(t)
			src := referenceSource(t, f.store)
			body := []byte("literal publication H\n")
			hash := syncproto.Sum(body)
			g, entries, wm := captureCrashGeneration(src, body)
			published, proceed := make(chan struct{}), make(chan struct{})
			captureResult, cleanupResult := make(chan error, 1), make(chan error, 1)
			defer func() {
				select {
				case <-proceed:
				default:
					close(proceed)
				}
			}()
			rejected := errors.New("reject actual capture transaction")
			go func() {
				captureResult <- f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
					if err := scope.putChunk(hash, body); err != nil {
						close(published)
						return err
					}
					close(published)
					<-proceed // Published H has no committed manifest yet.
					var guards []func() error
					if !commit {
						guards = append(guards, func() error { return rejected })
					}
					if err := scope.saveCapture(t.Context(), src, g, entries, wm, nil, guards...); err != nil {
						return err
					}
					if mode == "commit-then-error" {
						return rejected // Error does not undo the durable reference.
					}
					return nil
				})
			}()
			<-published
			requireOwnedChunk(t, f.spool, body, true)
			if f.spool.Used() != int64(len(body)) {
				t.Fatal("publication is not charged")
			}
			var pending int
			if err := f.store.db.QueryRow(`SELECT count(*) FROM devsync_manifest`).Scan(&pending); err != nil || pending != 0 {
				t.Fatalf("precommit manifest count %d: %v", pending, err)
			}
			requireReferenceOwnerHeld(t, f.spool) // Disabling ownership fails deterministically.
			cleanupAttempt := make(chan struct{})
			go func() {
				close(cleanupAttempt)
				cleanupResult <- f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
					return scope.releaseChunks(t.Context(), []syncproto.Hash{hash})
				})
			}()
			<-cleanupAttempt
			close(proceed)
			captureErr := awaitReferenceResult(t, captureResult)
			if mode == "commit" && captureErr != nil || mode != "commit" && !errors.Is(captureErr, rejected) {
				t.Fatalf("actual capture result: %v", captureErr)
			}
			if err := awaitReferenceResult(t, cleanupResult); err != nil {
				t.Fatal(err)
			}
			requireOwnedChunk(t, f.spool, body, commit)
			want := int64(0)
			if commit {
				want = int64(len(body))
			}
			if f.spool.Used() != want {
				t.Fatalf("charged bytes %d, want %d", f.spool.Used(), want)
			}
			refs, err := f.store.referenced(t.Context(), []syncproto.Hash{hash})
			if err != nil || refs[hash] != commit {
				t.Fatalf("durable chunk reference: %v %v", refs[hash], err)
			}
		})
	}
}

func TestSpoolReferencesTailCancellationAfterWaitingForFileOwner(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal released tail awaiting file owner\n")
	sid, hash := f.add(t, transcript.StorageJSONDoc, false, body, nil, true)
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		return scope.putTailVersion(sid, 0, hash, body)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempted := make(chan struct{})
	result := make(chan error, 1)
	f.spool.mu.Lock()
	go func() {
		result <- f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
			_, needed, err := scope.store.requiredTail(t.Context(), sid, 0)
			close(attempted)
			if err != nil || needed {
				return errors.New("fixture tail is not durably released")
			}
			// The private helper must check cancellation after acquiring the
			// occupied file mutex. No outer cancellation check can mask it.
			return scope.spool.dropTailVersion(ctx, sid, 0, hash)
		})
	}()
	<-attempted
	cancel()
	f.spool.mu.Unlock()
	if err := awaitReferenceResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation while waiting for file ownership: %v", err)
	}
	assertReconcileFile(t, reconcileVersionPath(f.spool, sid, 0, hash), body)
	if f.spool.Used() != int64(len(body)) {
		t.Fatal("canceled tail cleanup changed accounting")
	}
}

func TestSpoolReferencesCleanupBeforeRepublish(t *testing.T) {
	f := newTailPreparationFixture(t)
	src := referenceSource(t, f.store)
	body := []byte("literal reclaimed and republished H\n")
	hash := syncproto.Sum(body)
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.putChunk(hash, body) }); err != nil {
		t.Fatal(err)
	}
	checked, proceed := make(chan struct{}), make(chan struct{})
	cleanupResult, captureResult := make(chan error, 1), make(chan error, 1)
	defer func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	}()
	go func() {
		cleanupResult <- f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
			refs, err := scope.requiredChunks(t.Context(), []syncproto.Hash{hash})
			if err != nil || refs[hash] {
				close(checked)
				return errors.New("fixture unexpectedly referenced")
			}
			close(checked)
			<-proceed
			return scope.releaseChunks(t.Context(), []syncproto.Hash{hash})
		})
	}()
	<-checked
	requireReferenceOwnerHeld(t, f.spool)
	publishAttempt := make(chan struct{})
	go func() {
		close(publishAttempt)
		captureResult <- f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
			_, found, err := f.spool.Chunk(hash)
			if err != nil || found || f.spool.Used() != 0 {
				return errors.New("publisher entered before verified cleanup")
			}
			if err := scope.putChunk(hash, body); err != nil {
				return err
			}
			g, entries, wm := captureCrashGeneration(src, body)
			return scope.saveCapture(t.Context(), src, g, entries, wm, nil)
		})
	}()
	<-publishAttempt
	close(proceed)
	if err := awaitReferenceResult(t, cleanupResult); err != nil {
		t.Fatal(err)
	}
	if err := awaitReferenceResult(t, captureResult); err != nil {
		t.Fatal(err)
	}
	requireOwnedChunk(t, f.spool, body, true)
	refs, err := f.store.referenced(t.Context(), []syncproto.Hash{hash})
	if err != nil || !refs[hash] || f.spool.Used() != int64(len(body)) {
		t.Fatalf("republished committed H: %v %v", refs[hash], err)
	}
}

func TestSpoolReferencesBindingRejectsBeforeCallback(t *testing.T) {
	f, other := newTailPreparationFixture(t), newTailPreparationFixture(t)
	called := false
	fn := func(*spoolReferenceScope) error { called = true; return nil }
	if err := f.spool.withReferences(nil, fn); err == nil || called {
		t.Fatal("nil Store accepted")
	}
	if err := f.spool.withReferences(other.store, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	if err := f.spool.withReferences(f.store, fn); err != nil || !called {
		t.Fatalf("invalid call poisoned first binding: %v", err)
	}
	called = false
	if err := f.spool.withReferences(other.store, fn); err == nil || called {
		t.Fatal("incompatible Store entered callback")
	}
	if f.spool.Used() != 0 || other.spool.Used() != 0 {
		t.Fatal("rejected binding mutated spool")
	}
}

func TestSpoolReferencesUncertainLedgerRetainsChunks(t *testing.T) {
	for _, change := range []string{
		`DELETE FROM devsync_gens`,
		`UPDATE devsync_gens SET acked=-1`,
		`UPDATE devsync_gens SET entries=-1`,
		`UPDATE devsync_gens SET acked=entries+1`,
		`UPDATE devsync_gens SET lost=2`,
		`CREATE TEMP VIEW devsync_gens AS SELECT source_id,generation,NULL AS entries,acked,lost FROM main.devsync_gens`,
		`DROP TABLE devsync_manifest`,
		"canceled",
	} {
		t.Run(change, func(t *testing.T) {
			f := newTailPreparationFixture(t)
			src := referenceSource(t, f.store)
			body := []byte("literal uncertain reference H\n")
			hash := syncproto.Sum(body)
			if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
				if err := scope.putChunk(hash, body); err != nil {
					return err
				}
				g, entries, wm := captureCrashGeneration(src, body)
				return scope.saveCapture(t.Context(), src, g, entries, wm, nil)
			}); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if change == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else if _, err := f.store.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.releaseChunks(ctx, []syncproto.Hash{hash}) }); err == nil {
				t.Fatal("uncertain ledger authorized deletion")
			}
			requireOwnedChunk(t, f.spool, body, true)
			if f.spool.Used() != int64(len(body)) {
				t.Fatal("uncertain lookup changed accounting")
			}
		})
	}
}

func TestSpoolReferencesCutAuditRowsAreNotPending(t *testing.T) {
	f := newTailPreparationFixture(t)
	src := referenceSource(t, f.store)
	body := []byte("literal cut audit H\n")
	hash := syncproto.Sum(body)
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		if err := scope.putChunk(hash, body); err != nil {
			return err
		}
		g, entries, wm := captureCrashGeneration(src, body)
		return scope.saveCapture(t.Context(), src, g, entries, wm, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE devsync_gens SET entries=0`); err != nil {
		t.Fatal(err)
	}
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		return scope.releaseChunks(t.Context(), []syncproto.Hash{hash, hash})
	}); err != nil {
		t.Fatal(err)
	}
	requireOwnedChunk(t, f.spool, body, false)
	if f.spool.Used() != 0 {
		t.Fatal("duplicate release changed accounting twice")
	}
}

func TestSpoolReferencesSameHashTailAndCopiedChunk(t *testing.T) {
	f := newTailPreparationFixture(t)
	body := []byte("literal committed tail\n")
	sid, hash := f.add(t, transcript.StorageJSONDoc, false, body, nil, false)
	tail := syncproto.Tail{Size: int64(len(body)), Hash: hash}
	chunk := []byte("literal copied orphan chunk\n")
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		if err := scope.putTailVersion(sid, 0, hash, body); err != nil {
			return err
		}
		if err := scope.releaseTail(t.Context(), sid, 0, tail); err != nil {
			return err
		}
		return scope.putChunk(syncproto.Sum(chunk), chunk) // Explicit nested scope, no relock.
	}); err != nil {
		t.Fatal(err)
	}
	copy, found, err := f.spool.Chunk(syncproto.Sum(chunk))
	if err != nil || !found {
		t.Fatalf("copied chunk: %v", err)
	}
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
		return scope.releaseChunks(t.Context(), []syncproto.Hash{syncproto.Sum(chunk)})
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(copy, chunk) {
		t.Fatal("deletion changed returned chunk copy")
	}
	got, found, err := f.spool.TailVersion(sid, 0, hash)
	if err != nil || !found || !bytes.Equal(got, body) || f.spool.Used() != int64(len(body)) {
		t.Fatalf("same-hash tail was released: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.releaseTail(ctx, sid, 0, tail) }); !errors.Is(err, context.Canceled) {
		t.Fatalf("tail cancellation: %v", err)
	}
	assertReconcileFile(t, reconcileVersionPath(f.spool, sid, 0, hash), body)
}

func TestSpoolReferencesInvalidFilesRetainCharge(t *testing.T) {
	for _, kind := range []string{"corrupt", "symlink", "directory", "chunk-directory-symlink", "missing"} {
		t.Run(kind, func(t *testing.T) {
			f := newTailPreparationFixture(t)
			body := []byte("literal verified removable H\n")
			hash := syncproto.Sum(body)
			if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.putChunk(hash, body) }); err != nil {
				t.Fatal(err)
			}
			path := f.spool.chunkPath(hash)
			switch kind {
			case "corrupt":
				if err := os.WriteFile(path, []byte("different bytes"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "outside")
				if err := os.WriteFile(target, body, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "chunk-directory-symlink":
				if err := os.Rename(filepath.Join(f.spool.dir, "chunks"), filepath.Join(f.spool.dir, "held-chunks")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.spool.dir, "held-chunks"), filepath.Join(f.spool.dir, "chunks")); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error {
				return scope.releaseChunks(t.Context(), []syncproto.Hash{hash})
			})
			if kind == "missing" && err != nil || kind != "missing" && err == nil {
				t.Fatalf("invalid/missing file cleanup result: %v", err)
			}
			if f.spool.Used() != int64(len(body)) {
				t.Fatal("uncertain/missing file silently changed charged bytes")
			}
			if kind != "missing" {
				if err := f.spool.withReferences(f.store, func(scope *spoolReferenceScope) error { return scope.putChunk(hash, body) }); err == nil {
					t.Fatal("owned publication accepted an invalid existing candidate")
				}
			}
		})
	}
}
