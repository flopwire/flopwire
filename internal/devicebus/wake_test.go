package devicebus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestWakeIsExplicitBatchedAndNeverAutomaticallyRetried(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "submitted", true: "failed"}[fail], func(t *testing.T) {
			lb := newLocalBus(t)
			var mu sync.Mutex
			calls := 0
			lb.SetWaker(func(s Session) bool { return s.Agent == "claude" }, func(_ context.Context, s Session, text string) error {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if s.SessionID != "bbbb3333" || text == "" {
					t.Errorf("wrong native input: %+v %q", s, text)
				}
				if fail {
					return errors.New("ambiguous transport failure")
				}
				return nil
			})
			lb.advance(time.Second)
			if _, err := lb.send(t, "aaaa1111", "bbbb", "ordinary"); err != nil {
				t.Fatal(err)
			}
			if err := lb.wakePending(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatal("ordinary send woke a session")
			}
			for _, body := range []string{"first", "second"} {
				r, err := lb.send(t, "aaaa1111", "bbbb", body, func(r *busproto.SendRequest) { r.Wake = true })
				if err != nil || !r.WakeRequested || !r.To.WakeSupported {
					t.Fatalf("wake receipt: %+v %v", r, err)
				}
			}
			if err := lb.wakePending(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("batch submitted %d native inputs", calls)
			}
			if err := lb.wakePending(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("wake was retried")
			}
			var n int
			if err := lb.st.db.QueryRow(`SELECT count(*) FROM devbus_wakes WHERE outcome=?`, map[bool]string{false: "submitted", true: "failed"}[fail]).Scan(&n); err != nil || n != 2 {
				t.Fatalf("durable wake attempts: %d %v", n, err)
			}
			// A new process opening the same inbox must not repeat a wake.
			var seq int
			var name, path string
			if err := lb.st.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, lb.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.wakePending(ctx); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("restart repeated wake")
			}
			// Hook delivery still owns the messages, including after transport failure.
			if messages, _ := deliver(lb.Bus, "bbbb3333", "", Limit{}); len(messages) != 3 {
				t.Fatalf("lost messages: %+v", messages)
			}
		})
	}
}

func TestWakeRejectsUnsupportedTargetsWithoutSending(t *testing.T) {
	lb := newLocalBus(t)
	for _, to := range []string{"bbbb", "@gary"} {
		if _, err := lb.send(t, "aaaa1111", to, "wake", func(r *busproto.SendRequest) { r.Wake = true }); err == nil {
			t.Fatalf("accepted unsupported target %s", to)
		}
	}
	var n int
	if err := lb.st.db.QueryRow(`SELECT count(*) FROM devbus_messages`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("failed wake stored a message: %d %v", n, err)
	}
}

func TestWakeSkipsEndedRecipient(t *testing.T) {
	lb := newLocalBus(t)
	calls := 0
	lb.SetWaker(func(Session) bool { return true }, func(context.Context, Session, string) error { calls++; return nil })
	lb.advance(time.Second)
	if _, err := lb.send(t, "aaaa1111", "bbbb", "wake", func(r *busproto.SendRequest) { r.Wake = true }); err != nil {
		t.Fatal(err)
	}
	lb.p.set(sess("aaaa1111", "claude", "/src/api", true))
	lb.advance(time.Second)
	if err := lb.wakePending(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("ended session was woken")
	}
}

func TestWakeWorkerStartsNativeInputFromQueuedRequest(t *testing.T) {
	lb := newLocalBus(t)
	called := make(chan struct{}, 1)
	lb.SetWaker(func(Session) bool { return true }, func(context.Context, Session, string) error { called <- struct{}{}; return nil })
	lb.cfg.PresenceEvery = 5 * time.Millisecond
	lb.advance(time.Second)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- lb.Run(runCtx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if _, err := lb.send(t, "aaaa1111", "bbbb", "scheduled wake", func(r *busproto.SendRequest) { r.Wake = true }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("queued wake did not reach native input worker")
	}
}
