package devicesync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Drive a real protocol rejection through capture, upload, scheduler, hook,
// and retry. Advance the scheduler deadline directly rather than sleeping.
func TestSchedulerHookRespectsServerCooldown(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newEnv(t, Config{}, 1<<20)
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
				w.Write([]byte(`{"code":"flush_in_progress"}`))
			}))
			defer srv.Close()
			e.client.Server, e.client.HTTP = srv.URL, srv.Client()
			sc := NewScheduler(e.sy, SchedulerConfig{})
			sp := e.spec("live.jsonl", transcript.StorageJSONLAppend)
			data := jsonlLines(77, 10, 100)
			appendFile(t, sp.Path, data)
			sc.Flush(sp)
			sc.runOnce(context.Background())
			st := sc.Status()
			if st.ServerDown || !st.ServerBusy || st.RetryAt.Before(time.Now().Add(59*time.Second)) {
				t.Fatalf("bad pressure status: %+v", st)
			}
			sc.Flush(sp)
			sc.runOnce(context.Background())
			if requests != 1 || !sc.Status().RetryAt.Equal(st.RetryAt) {
				t.Fatal("hook bypassed cooldown")
			}
			// A later retry succeeds against the real in-memory sync server.
			e.client.Server, e.client.HTTP = e.http.URL, e.http.Client()
			sc.mu.Lock()
			sc.retryAt = time.Time{}
			sc.mu.Unlock()
			sc.runOnce(context.Background())
			if st := sc.Status(); st.ServerBusy || st.ServerDown || st.Queued != 0 {
				t.Fatalf("did not recover: %+v", st)
			}
			e.requireServerHas(sp.Path, fileIDOf(t, sp.Path), 0, data)
		})
	}
}
