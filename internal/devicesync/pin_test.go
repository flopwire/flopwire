package devicesync

import (
	"context"
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/transcript"
)

// A TLS pin mismatch is permanent: the scheduler stops after the first
// handshake instead of retrying, keeps the source queued, and Status says
// why. A hook flush does not restart it.
func TestSchedulerStopsOnPinMismatch(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	var handshakes atomic.Int32
	ts := httptest.NewUnstartedServer(e.srv)
	ts.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		handshakes.Add(1)
		return nil, nil
	}}
	ts.StartTLS()
	defer ts.Close()
	e.client.Server, e.client.HTTP = ts.URL, client.NewHTTPClient(client.FingerprintPrefix+strings.Repeat("11", 32))

	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Millisecond, time.Millisecond}, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	appendFile(t, sp.Path, jsonlLines(70, 20, 100))
	sc.Notify(sp)
	waitFor(t, "sync stopped", func() bool { return sc.Status().Stopped != "" })
	sc.Flush(sp)
	time.Sleep(200 * time.Millisecond)
	st := sc.Status()
	if n := handshakes.Load(); n != 1 {
		t.Fatalf("%d handshakes, want 1 (no retry)", n)
	}
	if !strings.Contains(st.Stopped, "pinned fingerprint") || st.Queued != 1 || st.ServerDown || len(st.Failing) != 0 {
		t.Fatalf("status %+v", st)
	}
}

// A re-pin (`flopwire login --fingerprint`) reaches a stopped scheduler
// without a restart: while Repin reports no new pin sync stays stopped and
// Status says so; once it installs one, a Recheck resumes sync and the
// queued source uploads.
func TestSchedulerResumesAfterRepin(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	ts := httptest.NewUnstartedServer(e.srv)
	ts.StartTLS()
	defer ts.Close()
	e.client.Server, e.client.HTTP = ts.URL, client.NewHTTPClient(client.FingerprintPrefix+strings.Repeat("11", 32))

	var repinned, checks atomic.Int32
	repin := func() bool {
		checks.Add(1)
		if repinned.Load() == 0 {
			return false
		}
		// Runs on the scheduler goroutine, with no flush in progress.
		e.client.HTTP = client.NewHTTPClient(client.Fingerprint(ts.Certificate().Raw))
		return true
	}
	sc := NewScheduler(e.sy, SchedulerConfig{Append: Cadence{time.Millisecond, time.Millisecond}, BackoffMin: time.Millisecond,
		BackoffMax: time.Millisecond, Repin: repin, RepinEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sc.Run(ctx)
	sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
	data := jsonlLines(71, 20, 100)
	appendFile(t, sp.Path, data)
	sc.Notify(sp)
	waitFor(t, "sync stopped", func() bool { return sc.Status().Stopped != "" })
	sc.Recheck()
	waitFor(t, "a pin check", func() bool { return checks.Load() > 0 })
	if st := sc.Status(); st.Stopped == "" {
		t.Fatalf("resumed without a new pin: %+v", st)
	}
	repinned.Store(1)
	sc.Recheck()
	waitFor(t, "upload after re-pin", func() bool { _, tail := e.srv.Manifest(sp.Path, fileIDOf(t, sp.Path), 0); return tail != nil })
	e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
	if st := sc.Status(); st.Stopped != "" || st.LastError != "" {
		t.Fatalf("status after re-pin %+v", st)
	}
}
