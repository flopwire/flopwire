package devicesync

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Primitive overlap tests establish descriptor lifetime only. Production
// Syncer operations remain serialized by Syncer.mu; these tests do not grant
// concurrent source-generation or permission mutation.
func descriptorTestFile(t *testing.T, path string, body []byte) *os.File {
	t.Helper()
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
func descriptorTestBytes(t *testing.T, f *os.File, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	n, err := f.ReadAt(got, 0)
	if n != len(want) || err != nil && !errors.Is(err, io.EOF) || !bytes.Equal(got, want) {
		t.Fatalf("descriptor bytes %q n=%d err=%v want=%q", got, n, err, want)
	}
}
func descriptorTestClosed(t *testing.T, f *os.File) {
	t.Helper()
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor should be physically closed: %v", err)
	}
}
func descriptorTestSlots(t *testing.T, owner *descriptorOwner, slots, current int) {
	t.Helper()
	owner.mu.Lock()
	defer owner.mu.Unlock()
	if owner.slots != slots || len(owner.current) != current {
		t.Fatalf("retained slots=%d current=%d want=%d/%d", owner.slots, len(owner.current), slots, current)
	}
}
func descriptorTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("descriptor barrier not reached")
	}
}

func TestDescriptorOwnerBorrowKeepsOldInodeAcrossReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source")
	oldBody := []byte("old retained inode, independently expected bytes")
	newBody := []byte("new replacement inode, separately expected bytes")
	old := descriptorTestFile(t, path, oldBody)
	owner := newDescriptorOwner(2)
	t.Cleanup(owner.close)
	if !owner.retain(1, old) {
		t.Fatal("initial retention rejected")
	}
	borrowed := owner.borrow(1)
	if borrowed == nil {
		t.Fatal("missing initial borrow")
	}
	readNow, releaseNow := make(chan struct{}), make(chan struct{})
	var readOnce, releaseOnce sync.Once
	var wg sync.WaitGroup
	result := make(chan struct {
		body []byte
		err  error
	}, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		readOnce.Do(func() { close(readNow) })
		releaseOnce.Do(func() { close(releaseNow) })
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		<-readNow
		b := make([]byte, len(oldBody))
		_, err := borrowed.file().ReadAt(b, 0)
		result <- struct {
			body []byte
			err  error
		}{b, err}
		<-releaseNow
		borrowed.release()
	}()
	replacementPath := filepath.Join(dir, "replacement")
	replacement := descriptorTestFile(t, replacementPath, newBody)
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatal(err)
	}
	if !owner.canRetain(1) {
		t.Fatal("available physical replacement slot denied before open")
	}
	if !owner.retain(1, replacement) {
		t.Fatal("replacement rejected with free physical slot")
	}
	descriptorTestSlots(t, owner, 2, 1)
	latest := owner.borrow(1)
	if latest == nil {
		t.Fatal("replacement lookup missing")
	}
	t.Cleanup(latest.release)
	if latest.file() == borrowed.file() {
		t.Fatal("replacement redirected old borrowed entry")
	}
	descriptorTestBytes(t, latest.file(), newBody)
	readOnce.Do(func() { close(readNow) })
	select {
	case got := <-result:
		if got.err != nil || !bytes.Equal(got.body, oldBody) {
			t.Fatalf("retired reader %q %v", got.body, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retired reader did not finish raw read")
	}
	if _, err := old.Stat(); err != nil {
		t.Fatal("retired borrowed inode closed before release", err)
	}
	releaseOnce.Do(func() { close(releaseNow) })
	descriptorTestSignal(t, done)
	descriptorTestClosed(t, old)
	descriptorTestSlots(t, owner, 1, 1)
	descriptorTestBytes(t, replacement, newBody)
	latest.release()
	owner.retire(1)
	descriptorTestClosed(t, replacement)
	descriptorTestSlots(t, owner, 0, 0)
}

func TestDescriptorOwnerCapacityChargesRetiredBorrowAndKeepsCallerCustody(t *testing.T) {
	dir := t.TempDir()
	oldBody := []byte("borrowed old bytes at cap one")
	old := descriptorTestFile(t, filepath.Join(dir, "old"), oldBody)
	replacement := descriptorTestFile(t, filepath.Join(dir, "replacement"), []byte("caller owns rejected replacement"))
	other := descriptorTestFile(t, filepath.Join(dir, "other"), []byte("different source must not evict"))
	owner := newDescriptorOwner(1)
	t.Cleanup(owner.close)
	if !owner.retain(1, old) {
		t.Fatal("initial retention rejected")
	}
	if !owner.canRetain(1) {
		t.Fatal("unborrowed same-source replacement cannot reuse its slot")
	}
	borrowed := owner.borrow(1)
	if borrowed == nil {
		t.Fatal("borrow missing")
	}
	if owner.canRetain(1) || owner.canRetain(2) {
		t.Fatal("pre-open admission ignored physically charged borrowed entry")
	}
	t.Cleanup(borrowed.release)
	if owner.retain(2, other) {
		t.Fatal("different source evicted an occupied slot")
	}
	descriptorTestSlots(t, owner, 1, 1)
	descriptorTestBytes(t, borrowed.file(), oldBody)
	if _, err := other.Stat(); err != nil {
		t.Fatal("owner took custody of rejected different-source FD", err)
	}
	if owner.retain(1, replacement) {
		t.Fatal("borrowed retired slot went uncharged during replacement")
	}
	descriptorTestSlots(t, owner, 1, 0)
	if owner.borrow(1) != nil {
		t.Fatal("rejected replacement left a current lookup")
	}
	descriptorTestBytes(t, borrowed.file(), oldBody)
	if _, err := replacement.Stat(); err != nil {
		t.Fatal("owner closed caller-owned rejected replacement", err)
	}
	if owner.canRetain(2) || owner.retain(2, other) {
		t.Fatal("retired borrowed entry did not retain capacity charge")
	}
	borrowed.release()
	borrowed.release()
	descriptorTestClosed(t, old)
	descriptorTestSlots(t, owner, 0, 0)
	if !owner.canRetain(2) {
		t.Fatal("released physical slot remained unavailable")
	}
	if !owner.retain(2, other) {
		t.Fatal("released physical slot could not be reused")
	}
	descriptorTestSlots(t, owner, 1, 1)
	owner.retire(2)
	descriptorTestClosed(t, other)
	descriptorTestSlots(t, owner, 0, 0)
}

func TestDescriptorOwnerCloseDeniesNewWorkUntilFinalBorrowRelease(t *testing.T) {
	dir := t.TempDir()
	body := []byte("two independent readers retain this inode")
	f := descriptorTestFile(t, filepath.Join(dir, "retained"), body)
	rejected := descriptorTestFile(t, filepath.Join(dir, "rejected"), []byte("caller retains this FD after close"))
	owner := newDescriptorOwner(1)
	t.Cleanup(owner.close)
	if !owner.retain(1, f) || !owner.retain(1, f) {
		t.Fatal("same concrete retained file was not idempotent")
	}
	descriptorTestSlots(t, owner, 1, 1)
	first, second := owner.borrow(1), owner.borrow(1)
	if first == nil || second == nil {
		t.Fatal("multiple readers unavailable")
	}
	t.Cleanup(first.release)
	t.Cleanup(second.release)
	owner.close()
	owner.close()
	descriptorTestSlots(t, owner, 1, 0)
	if owner.borrow(1) != nil || owner.canRetain(2) || owner.retain(2, rejected) {
		t.Fatal("closed owner accepted new work")
	}
	if _, err := rejected.Stat(); err != nil {
		t.Fatal("closed owner took rejected file custody", err)
	}
	descriptorTestBytes(t, first.file(), body)
	descriptorTestBytes(t, second.file(), body)
	// All duplicate releases refer to the first concrete borrow. They cannot
	// consume the second reader's charge or close its file.
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); first.release() }()
	}
	wg.Wait()
	descriptorTestSlots(t, owner, 1, 0)
	descriptorTestBytes(t, second.file(), body)
	second.release()
	second.release()
	descriptorTestClosed(t, f)
	descriptorTestSlots(t, owner, 0, 0)
}

func TestDescriptorOwnerPayloadBorrowHasFixedOldInodeCustody(t *testing.T) {
	e := newEnv(t, Config{MaxHeldFiles: 2, SealAfter: -1}, 1<<20)
	sp := e.spec("payload-old.jsonl", transcript.StorageJSONLAppend)
	oldBody := []byte("{\"record\":\"old payload inode remains readable\"}\n")
	newBody := []byte("{\"record\":\"replacement inode must remain open\"}\n")
	appendFile(t, sp.Path, oldBody)
	e.srv.SetDown(true)
	if err := e.sy.Sync(t.Context(), sp); err == nil {
		t.Fatal("pending capture outage missing")
	}
	src := tailCaptureSource(t, e.store, sp)
	gen := lifecycleGeneration(t, e, src)
	e.sy.mu.Lock()
	defer e.sy.mu.Unlock()
	p := &payload{ctx: t.Context(), op: e.sy.ordinaryOperation(), src: src, g: gen, failed: -1}
	t.Cleanup(p.close)
	prefix := make([]byte, 12)
	if err := p.readSource(prefix, 0); err != nil || !bytes.Equal(prefix, oldBody[:12]) {
		t.Fatalf("initial payload read %q %v", prefix, err)
	}
	oldFD := p.f
	replacementPath := filepath.Join(e.dir, "payload-new.tmp")
	replacement := descriptorTestFile(t, replacementPath, newBody)
	if err := os.Rename(replacementPath, sp.Path); err != nil {
		t.Fatal(err)
	}
	if !e.sy.descriptors.retain(src.ID, replacement) {
		t.Fatal("payload replacement rejected")
	}
	descriptorTestSlots(t, e.sy.descriptors, 2, 1)
	// The redactor may cache this small body. Read directly through the
	// acquired payload FD to prove uncached reads survive replacement too.
	descriptorTestBytes(t, oldFD, oldBody)
	all := make([]byte, len(oldBody))
	if err := p.readSource(all, 0); err != nil || !bytes.Equal(all, oldBody) {
		t.Fatalf("payload changed inode %q %v", all, err)
	}
	p.close()
	p.close()
	descriptorTestClosed(t, oldFD)
	descriptorTestSlots(t, e.sy.descriptors, 1, 1)
	descriptorTestBytes(t, replacement, newBody)
	// The payload copied the exact captured bytes, rather than borrowing an
	// authorization receipt or reading the new live pathname.
	if syncproto.Sum(all) != gen.Tail.Hash {
		t.Fatal("raw payload differs from durable captured tail hash")
	}
}

func TestDescriptorOwnerPayloadFreshOpenClosesOnlyOwnFD(t *testing.T) {
	e := newEnv(t, Config{MaxHeldFiles: 1, SealAfter: -1}, 1<<20)
	sp := e.spec("payload-fresh.jsonl", transcript.StorageJSONLAppend)
	body := []byte("{\"record\":\"distinct descriptors to one inode\"}\n")
	appendFile(t, sp.Path, body)
	src := tailCaptureSource(t, e.store, sp)
	e.sy.mu.Lock()
	defer e.sy.mu.Unlock()
	p := &payload{ctx: t.Context(), op: e.sy.ordinaryOperation(), src: src, g: &genRow{SourceID: src.ID, Gen: src.Gen}, failed: -1}
	t.Cleanup(p.close)
	got := make([]byte, len(body))
	if err := p.readSource(got, 0); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("fresh payload read %q %v", got, err)
	}
	ownFD := p.f
	retained, err := os.Open(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { retained.Close() })
	ownInfo, err := ownFD.Stat()
	if err != nil {
		t.Fatal(err)
	}
	retainedInfo, err := retained.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if ownFD == retained || !os.SameFile(ownInfo, retainedInfo) {
		t.Fatal("fixture needs separate descriptors to the same inode")
	}
	if !e.sy.descriptors.retain(src.ID, retained) {
		t.Fatal("separately opened retention failed")
	}
	p.close()
	p.close()
	descriptorTestClosed(t, ownFD)
	descriptorTestBytes(t, retained, body)
	descriptorTestSlots(t, e.sy.descriptors, 1, 1)
	e.sy.descriptors.retire(src.ID)
	descriptorTestClosed(t, retained)
}

func TestDescriptorOwnerSnapshotRefusesBorrowedCapBeforeOpen(t *testing.T) {
	e := newEnv(t, Config{MaxHeldFiles: 1, SealAfter: -1}, 1<<20)
	sp := e.spec("snapshot-original.jsonl", transcript.StorageJSONLAppend)
	oldBody := []byte("{\"record\":\"old staged evidence\"}\n")
	appendFile(t, sp.Path, oldBody)
	oldIdentity, err := transcript.StatIdentity(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	oldSnapshot := filepath.Join(e.dir, "staged-old.jsonl")
	if err := os.WriteFile(oldSnapshot, oldBody, 0400); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	if err := e.sy.SyncSnapshot(t.Context(), sp, oldSnapshot, oldIdentity); err == nil {
		t.Fatal("pending snapshot outage missing")
	}
	src := tailCaptureSource(t, e.store, sp)
	borrowed := e.sy.descriptors.borrow(src.ID)
	if borrowed == nil {
		t.Fatal("pending staged descriptor missing")
	}
	t.Cleanup(borrowed.release)
	appended := []byte("{\"record\":\"new staged extension\"}\n")
	appendFile(t, sp.Path, appended)
	newBody := append(bytes.Clone(oldBody), appended...)
	newIdentity, err := transcript.StatIdentity(sp.Path)
	if err != nil {
		t.Fatal(err)
	}
	newSnapshot := filepath.Join(e.dir, "staged-new.jsonl")
	if err := os.WriteFile(newSnapshot, newBody, 0400); err != nil {
		t.Fatal(err)
	}
	before := e.flushes()
	// The missing alternative proves refusal precedes opening the proposed
	// staged pathname, rather than falling through to an original-path read.
	for _, candidate := range []string{newSnapshot, filepath.Join(e.dir, "missing-staged.jsonl")} {
		if err := e.sy.SyncSnapshot(t.Context(), sp, candidate, newIdentity); err == nil || !strings.Contains(err.Error(), "snapshot descriptor limit") {
			t.Fatalf("borrowed snapshot admission=%v", err)
		}
		var generation, wmOffset, size, entries, tailSize, acked int64
		var hash []byte
		if err := e.store.db.QueryRow(`SELECT s.generation,json_extract(s.watermark,'$.Offset'),g.size,g.entries,g.tail_size,g.acked,g.tail_hash FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id AND g.generation=s.generation WHERE s.id=?`, src.ID).Scan(&generation, &wmOffset, &size, &entries, &tailSize, &acked, &hash); err != nil {
			t.Fatal(err)
		}
		want := syncproto.Sum(oldBody)
		if generation != 0 || wmOffset != int64(len(oldBody)) || size != int64(len(oldBody)) || entries != 0 || tailSize != int64(len(oldBody)) || acked != 0 || !bytes.Equal(hash, want[:]) {
			t.Fatal("snapshot refusal mutated committed capture")
		}
		if e.flushes() != before {
			t.Fatal("snapshot refusal entered transport")
		}
		current := e.sy.descriptors.borrow(src.ID)
		if current == nil {
			t.Fatal("pre-open refusal removed old staged lookup")
		}
		if current.file() != borrowed.file() {
			current.release()
			t.Fatal("pre-open refusal replaced staged descriptor")
		}
		current.release()
		descriptorTestBytes(t, borrowed.file(), oldBody)
		descriptorTestSlots(t, e.sy.descriptors, 1, 1)
	}
	borrowed.release()
	e.srv.SetDown(false)
	if err := e.sy.SyncSnapshot(t.Context(), sp, newSnapshot, newIdentity); err != nil {
		t.Fatal("snapshot did not progress after borrower release", err)
	}
	e.requireServerHas(sp.Path, newIdentity.ID.String(), 0, newBody)
	descriptorTestSlots(t, e.sy.descriptors, 0, 0)
}
