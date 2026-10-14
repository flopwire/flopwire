package devicebus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
	"github.com/flopwire/flopwire/internal/client"
)

type referenceTestServer struct {
	*fakeServer
	read  func(context.Context, string) (string, error)
	calls atomic.Int64
}

func (s *referenceTestServer) ReadReference(ctx context.Context, ref string) (string, error) {
	s.calls.Add(1)
	return s.read(ctx, ref)
}

type referenceTestFixture struct {
	bus        *Bus
	server     *referenceTestServer
	clock      atomic.Int64
	credential atomic.Value
	path       string
	cfg        Config
}

func newReferenceTestFixture(t *testing.T) *referenceTestFixture {
	t.Helper()
	f := &referenceTestFixture{path: filepath.Join(t.TempDir(), "bus.db")}
	f.clock.Store(time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC).UnixNano())
	f.credential.Store("credential-one")
	f.server = &referenceTestServer{fakeServer: newFakeServer(), read: func(context.Context, string) (string, error) { return "user: synthetic authorized fact", nil }}
	f.cfg = Config{Logger: quiet(), Now: func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }, Connect: func() (Server, string) { return f.server, f.credential.Load().(string) }}
	var err error
	f.bus, err = Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if f.bus != nil {
			f.bus.Close()
		}
	})
	return f
}
func (f *referenceTestFixture) message(id string) busproto.Envelope {
	now := f.cfg.Now()
	return busproto.Envelope{ID: id, ThreadID: id, From: "synthetic-sender", FromAgent: "claude", User: "sender@example.test", Sender: busproto.SenderTeammate, Intent: busproto.IntentInform, Body: "synthetic message", Refs: []string{"abcdefgh/0"}, Sent: now, ExpiresAt: now.Add(time.Hour), ToSession: "synthetic-recipient", ToAgent: "claude", Addressed: "session", Seq: 1}
}
func referenceTestWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("reference test barrier timed out")
	}
}

func TestReferenceInflightFences(t *testing.T) {
	for _, mode := range []string{"invalidate", "rotate", "expire"} {
		t.Run(mode, func(t *testing.T) {
			f := newReferenceTestFixture(t)
			m := f.message("synthetic-fenced")
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			f.server.read = func(context.Context, string) (string, error) {
				close(entered)
				<-release
				return "user: stale synthetic text", nil
			}
			go func() { defer close(done); f.bus.prepareReferences(ctx, []busproto.Envelope{m}) }()
			referenceTestWait(t, entered)
			switch mode {
			case "invalidate":
				f.bus.InvalidateReferences()
			case "rotate":
				f.credential.Store("credential-two")
			case "expire":
				f.clock.Add(int64(referenceAge))
			}
			close(release)
			referenceTestWait(t, done)
			if len(f.bus.references) != 0 {
				t.Fatalf("fenced result entered cache before getter: %v", f.bus.references)
			}
			if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
				t.Fatalf("fenced result survived: %v", got)
			}
		})
	}
}

func TestReferenceSnapshotExpiryAndEnvelopeScope(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-scoped")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); got[m.Refs[0]] != "user: synthetic authorized fact" {
		t.Fatalf("prepared snapshot: %v", got)
	}
	other := m
	other.ID = "synthetic-other-envelope"
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{other}); len(got) != 0 {
		t.Fatalf("text leaked between envelopes: %v", got)
	}
	f.clock.Add(int64(referenceAge))
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
		t.Fatalf("expired snapshot: %v", got)
	}
	if len(f.bus.references) != 0 {
		t.Fatalf("expired entries retained: %d", len(f.bus.references))
	}
	if got := f.server.calls.Load(); got != 1 {
		t.Fatalf("hook made network calls: %d", got)
	}
}

func TestReferenceSnapshotCredentialRotation(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-rotation")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	f.credential.Store("credential-two")
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
		t.Fatalf("old credential snapshot: %v", got)
	}
	if f.server.calls.Load() != 1 {
		t.Fatal("hook called reference reader after rotation")
	}
}

func TestReferenceBatchAndCacheBounds(t *testing.T) {
	f := newReferenceTestFixture(t)
	var msgs []busproto.Envelope
	for i := 0; i < referenceEntries+20; i++ {
		m := f.message(fmt.Sprintf("synthetic-%d", i))
		m.Refs = []string{fmt.Sprintf("abcdefgh/%d", i)}
		msgs = append(msgs, m)
	}
	f.bus.prepareReferences(ctx, msgs)
	if got := f.server.calls.Load(); got != referenceEntries {
		t.Fatalf("batch reads %d, want %d", got, referenceEntries)
	}
	if got := len(f.bus.references); got != referenceEntries {
		t.Fatalf("cache size %d", got)
	}
	f.clock.Add(int64(time.Second))
	next := f.message("synthetic-next")
	next.Refs = []string{"abcdefgh/999"}
	f.bus.prepareReferences(ctx, []busproto.Envelope{next})
	if got := len(f.bus.references); got != referenceEntries {
		t.Fatalf("cache grew to %d", got)
	}
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{next}); got[next.Refs[0]] == "" {
		t.Fatal("newest entry discarded")
	}
}

func TestReferenceExpiredSnapshotRefetchMissingLeavesNoText(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-denied")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	f.clock.Add(int64(referenceAge))
	f.server.read = func(context.Context, string) (string, error) {
		return "", &client.APIError{StatusCode: http.StatusNotFound}
	}
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
		t.Fatalf("denied refresh retained text: %v", got)
	}
	if f.server.calls.Load() != 2 {
		t.Fatal("hook called reference reader")
	}
}

func TestReferenceReadFailuresPreserveDelivery(t *testing.T) {
	for _, mode := range []string{"failed", "timeout-reader", "canceled-reader"} {
		t.Run(mode, func(t *testing.T) {
			f := newReferenceTestFixture(t)
			m := f.message("synthetic-delivery")
			f.server.read = func(readCtx context.Context, _ string) (string, error) {
				if mode == "failed" {
					return "", errors.New("synthetic unavailable")
				}
				if mode == "timeout-reader" {
					return "", context.DeadlineExceeded
				}
				canceled, cancel := context.WithCancel(readCtx)
				cancel()
				<-canceled.Done()
				return "", canceled.Err()
			}
			f.bus.prepareReferences(ctx, []busproto.Envelope{m})
			if err := f.bus.st.reconcile(ctx, []busproto.Envelope{m}, nil, f.cfg.Now()); err != nil {
				t.Fatal(err)
			}
			got, err := f.bus.Take(ctx, m.ToSession, m.ToAgent, Limit{})
			if err != nil || len(got) != 1 {
				t.Fatalf("message lost: %v %v", got, err)
			}
			if ex := f.bus.ReferenceExcerpts(got); len(ex) != 0 {
				t.Fatalf("failed read excerpt: %v", ex)
			}
			if f.server.calls.Load() != 1 {
				t.Fatal("hook retried network read")
			}
			if err := f.bus.st.untake(ctx, []string{m.ID}); err != nil {
				t.Fatal(err)
			}
			got, err = f.bus.Take(ctx, m.ToSession, m.ToAgent, Limit{})
			if err != nil || len(got) != 1 {
				t.Fatalf("requeue lost message: %v %v", got, err)
			}
			if err := f.bus.Confirm(ctx, m.ToSession, []string{m.ID}); err != nil {
				t.Fatal(err)
			}
			owed, err := f.bus.st.owed(ctx, "owed", 10)
			if err != nil || len(owed) != 1 || owed[0] != m.ID {
				t.Fatalf("receipt lost: %v %v", owed, err)
			}
		})
	}
}

func TestReferenceRestartDropsTextKeepsInbox(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-restart")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if err := f.bus.st.reconcile(ctx, []busproto.Envelope{m}, nil, f.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.bus.Close(); err != nil {
		t.Fatal(err)
	}
	f.bus = nil
	restarted, err := Open(f.path, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.bus = restarted
	got, err := restarted.Take(ctx, m.ToSession, m.ToAgent, Limit{})
	if err != nil || len(got) != 1 {
		t.Fatalf("persisted inbox lost: %v %v", got, err)
	}
	if ex := restarted.ReferenceExcerpts(got); len(ex) != 0 {
		t.Fatalf("text persisted across restart: %v", ex)
	}
	if f.server.calls.Load() != 1 {
		t.Fatal("restart hook triggered network read")
	}
}

func TestReferenceBlockedReadLeavesQueuedMessageDeliverable(t *testing.T) {
	f := newReferenceTestFixture(t)
	queued := f.message("synthetic-already-queued")
	if err := f.bus.st.reconcile(ctx, []busproto.Envelope{queued}, nil, f.cfg.Now()); err != nil {
		t.Fatal(err)
	}
	entered, done := make(chan struct{}), make(chan struct{})
	f.server.read = func(readCtx context.Context, _ string) (string, error) {
		if _, ok := readCtx.Deadline(); !ok {
			t.Error("batch read lacks deadline")
		}
		close(entered)
		<-readCtx.Done()
		return "", readCtx.Err()
	}
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := f.message("synthetic-incoming")
	go func() { defer close(done); f.bus.prepareReferences(readCtx, []busproto.Envelope{incoming}) }()
	referenceTestWait(t, entered)
	got, err := f.bus.Take(ctx, queued.ToSession, queued.ToAgent, Limit{})
	if err != nil || len(got) != 1 || got[0].ID != queued.ID {
		t.Fatalf("blocked read held queued delivery: %v %v", got, err)
	}
	if ex := f.bus.ReferenceExcerpts(got); len(ex) != 0 {
		t.Fatalf("unexpected snapshot: %v", ex)
	}
	if f.server.calls.Load() != 1 {
		t.Fatal("hook triggered another network read")
	}
	cancel()
	referenceTestWait(t, done)
	if ex := f.bus.ReferenceExcerpts([]busproto.Envelope{incoming}); len(ex) != 0 {
		t.Fatalf("canceled read cached text: %v", ex)
	}
}

func TestReferenceExpiredSnapshotUnavailableDuringRefetch(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-expired-refetch")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	f.clock.Add(int64(referenceAge))
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.server.read = func(context.Context, string) (string, error) {
		close(entered)
		<-release
		return "user: synthetic replacement", nil
	}
	go func() { defer close(done); f.bus.prepareReferences(ctx, []busproto.Envelope{m}) }()
	referenceTestWait(t, entered)
	if during := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(during) != 0 {
		t.Errorf("expired text served while refetch blocked: %v", during)
	}
	if f.server.calls.Load() != 2 {
		t.Error("hook made extra network read")
	}
	close(release)
	referenceTestWait(t, done)
	if after := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); after[m.Refs[0]] != "user: synthetic replacement" {
		t.Fatalf("refetch failed: %v", after)
	}
}

func TestReferenceUnavailableDeviceClearsSnapshots(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-unavailable")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if len(f.bus.ReferenceExcerpts([]busproto.Envelope{m})) != 1 {
		t.Fatal("missing initial snapshot")
	}
	f.bus.cfg.NoDevice = func() string { return "credential revoked" }
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
		t.Fatalf("disabled device retained text: %v", got)
	}
	f.bus.cfg.NoDevice = nil
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{m}); len(got) != 0 {
		t.Fatalf("reenabling revived an invalidated snapshot: %v", got)
	}
}

func TestReferenceAuthorizationFailureInvalidatesBatchAndExistingSnapshots(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newReferenceTestFixture(t)
			old := f.message("synthetic-old-authorization")
			old.Refs = []string{"abcdefgh/9"}
			f.bus.prepareReferences(ctx, []busproto.Envelope{old})
			incoming := f.message("synthetic-batch-authorization")
			incoming.Refs = []string{"abcdefgh/0", "abcdefgh/1", "abcdefgh/2"}
			f.server.read = func(_ context.Context, ref string) (string, error) {
				if ref == "abcdefgh/1" {
					return "", fmt.Errorf("synthetic wrapped read: %w", &client.APIError{StatusCode: status})
				}
				return "user: synthetic batch success", nil
			}
			f.bus.prepareReferences(ctx, []busproto.Envelope{incoming})
			got := f.bus.ReferenceExcerpts([]busproto.Envelope{old, incoming})
			if status == http.StatusNotFound {
				if len(got) != 3 || got[old.Refs[0]] != "user: synthetic authorized fact" || got[incoming.Refs[0]] != "user: synthetic batch success" || got[incoming.Refs[2]] != "user: synthetic batch success" {
					t.Fatalf("404 invalidated unrelated snapshots: %v", got)
				}
				if f.server.calls.Load() != 4 {
					t.Fatalf("404 aborted batch: calls %d", f.server.calls.Load())
				}
			} else {
				if len(got) != 0 || len(f.bus.references) != 0 {
					t.Fatalf("authorization failure retained old or partial batch snapshots: %v", got)
				}
				if f.server.calls.Load() != 3 {
					t.Fatalf("authorization failure did not stop reads immediately: %d", f.server.calls.Load())
				}
			}
		})
	}
}

func TestReferenceFreshSnapshotSkippedWithoutExpiryExtension(t *testing.T) {
	f := newReferenceTestFixture(t)
	m := f.message("synthetic-fresh")
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	key := referenceKey{m.ID, m.Refs[0]}
	expiry := f.bus.references[key].expires
	f.clock.Add(int64(referenceAge - time.Nanosecond))
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if f.server.calls.Load() != 1 {
		t.Fatal("fresh snapshot was fetched again")
	}
	if got := f.bus.references[key].expires; !got.Equal(expiry) {
		t.Fatalf("skip extended expiry: %v -> %v", expiry, got)
	}
	f.clock.Add(int64(time.Nanosecond))
	f.bus.prepareReferences(ctx, []busproto.Envelope{m})
	if f.server.calls.Load() != 2 {
		t.Fatal("snapshot not refetched at expiry")
	}
	if got := f.bus.references[key].expires; !got.Equal(expiry.Add(referenceAge)) {
		t.Fatalf("replacement expiry = %v", got)
	}
}

func TestReferenceTransientFailureKeepsOtherValidSnapshotExpiry(t *testing.T) {
	for _, failure := range []error{errors.New("synthetic unavailable"), context.DeadlineExceeded, &client.APIError{StatusCode: http.StatusServiceUnavailable}} {
		t.Run(failure.Error(), func(t *testing.T) {
			f := newReferenceTestFixture(t)
			m := f.message("synthetic-transient")
			f.bus.prepareReferences(ctx, []busproto.Envelope{m})
			key := referenceKey{m.ID, m.Refs[0]}
			before := f.bus.references[key]
			f.clock.Add(int64(26 * time.Second))
			missing := f.message("synthetic-transient-missing")
			missing.Refs = []string{"abcdefgh/8"}
			f.server.read = func(context.Context, string) (string, error) { return "", failure }
			f.bus.prepareReferences(ctx, []busproto.Envelope{m, missing})
			if after := f.bus.references[key]; after != before {
				t.Fatalf("transient failure changed valid snapshot: before %+v after %+v", before, after)
			}
			if f.server.calls.Load() != 2 {
				t.Fatal("valid snapshot refetched before expiry")
			}
			if got := f.bus.ReferenceExcerpts([]busproto.Envelope{missing}); len(got) != 0 {
				t.Fatalf("transient failure created text: %v", got)
			}
		})
	}
}

func TestReferenceCanceledBatchKeepsUnattemptedSnapshot(t *testing.T) {
	f := newReferenceTestFixture(t)
	old := f.message("synthetic-unattempted")
	f.bus.prepareReferences(ctx, []busproto.Envelope{old})
	key := referenceKey{old.ID, old.Refs[0]}
	before := f.bus.references[key]
	f.clock.Add(int64(referenceAge / 2))
	incoming := f.message("synthetic-missing-before-refresh")
	incoming.Refs = []string{"abcdefgh/8"}
	entered, done := make(chan struct{}), make(chan struct{})
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.server.read = func(c context.Context, ref string) (string, error) {
		if ref != incoming.Refs[0] {
			t.Errorf("refresh preceded missing ref: %s", ref)
		}
		close(entered)
		<-c.Done()
		return "", c.Err()
	}
	go func() { defer close(done); f.bus.prepareReferences(readCtx, []busproto.Envelope{old, incoming}) }()
	referenceTestWait(t, entered)
	cancel()
	referenceTestWait(t, done)
	if after := f.bus.references[key]; after != before {
		t.Fatalf("unattempted ref lost original snapshot: before %+v after %+v", before, after)
	}
	if f.server.calls.Load() != 2 {
		t.Fatalf("batch continued after cancellation: calls %d", f.server.calls.Load())
	}
}

func TestReferenceValidSnapshotsLetLaterMissingEnvelopeProgress(t *testing.T) {
	f := newReferenceTestFixture(t)
	first, second, third := f.message("synthetic-m1"), f.message("synthetic-m2"), f.message("synthetic-m3")
	second.Refs = []string{"abcdefgh/1"}
	third.Refs = []string{"abcdefgh/2"}
	f.bus.prepareReferences(ctx, []busproto.Envelope{first, second})
	f.clock.Add(int64(26 * time.Second))
	var order []string
	f.server.read = func(_ context.Context, ref string) (string, error) {
		order = append(order, ref)
		return "user: synthetic refreshed fact", nil
	}
	f.bus.prepareReferences(ctx, []busproto.Envelope{first, second, third})
	if len(order) == 0 || order[0] != third.Refs[0] {
		t.Fatalf("valid snapshots prevented missing third envelope progress: %v", order)
	}
	if got := f.bus.ReferenceExcerpts([]busproto.Envelope{third}); got[third.Refs[0]] == "" {
		t.Fatal("missing envelope did not make progress")
	}
}
