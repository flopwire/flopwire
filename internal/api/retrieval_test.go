package api_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/store"
)

// blockingRetrieval holds every search until released.
type blockingRetrieval struct {
	api.Retrieval
	started chan struct{}
	release chan struct{}
}

func (b *blockingRetrieval) Search(context.Context, format.SearchQuery, format.Filters) (*format.Page, error) {
	b.started <- struct{}{}
	<-b.release
	return nil, nil
}

// Retrieval endpoints have a concurrency limit: requests past it are
// refused with 429, not queued.
func TestRetrievalConcurrencyLimit(t *testing.T) {
	s := store.NewMemory()
	b := &blockingRetrieval{started: make(chan struct{}, 64), release: make(chan struct{})}
	srv := newServer(t, s, api.Config{Retrieval: b})
	admin := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(admin))["token"].(string)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := request(t, "GET", srv.URL+"/v1/search?q=x", nil, bearer(device))
			res.Body.Close()
		}()
	}
	for range 16 {
		<-b.started
	}
	res := request(t, "GET", srv.URL+"/v1/search?q=x", nil, bearer(device))
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" {
		t.Fatalf("status %d", res.StatusCode)
	}
	res.Body.Close()
	close(b.release)
	wg.Wait()
	res = request(t, "GET", srv.URL+"/v1/search?q=x", nil, bearer(device))
	if res.StatusCode != 200 {
		t.Fatalf("after release: %d %s", res.StatusCode, read(res))
	}
	res.Body.Close()
}

// deadlineRetrieval reports the budget each call's context carries.
type deadlineRetrieval struct {
	api.Retrieval
	left chan time.Duration
}

func (d *deadlineRetrieval) note(ctx context.Context) error {
	dl, ok := ctx.Deadline()
	if !ok {
		d.left <- -1
	} else {
		d.left <- time.Until(dl)
	}
	return fmt.Errorf("timed out after 2s: %w", context.DeadlineExceeded)
}

func (d *deadlineRetrieval) Sessions(ctx context.Context, _ string, _ int, _ format.Filters) (*format.Sessions, error) {
	return nil, d.note(ctx)
}

func (d *deadlineRetrieval) Read(ctx context.Context, _ string, _ format.ReadQuery, _ format.Filters) (*format.Context, error) {
	return nil, d.note(ctx)
}

func (d *deadlineRetrieval) RawAt(ctx context.Context, _, _ string) ([]byte, format.Attribution, error) {
	return nil, format.Attribution{}, d.note(ctx)
}

func (d *deadlineRetrieval) RawByPath(ctx context.Context, _, _, _ string, _, _, _ int64) ([]byte, format.Attribution, error) {
	return nil, format.Attribution{}, d.note(ctx)
}

func (d *deadlineRetrieval) Raw(ctx context.Context, _ string, _, _, _ int64) ([]byte, format.Attribution, error) {
	return nil, format.Attribution{}, d.note(ctx)
}

// sessions, read and raw run under the request budget, as grep and search
// do: the default, or ?timeout= up to the maximum; running out is a 504
// that says so, not a 500.
func TestRetrievalRoutesHaveBudget(t *testing.T) {
	s := store.NewMemory()
	d := &deadlineRetrieval{left: make(chan time.Duration, 1)}
	srv := newServer(t, s, api.Config{Retrieval: d})
	admin := seedAdmin(t, s)
	device := postJSON(t, srv.URL+"/v1/devices", map[string]string{"name": "mac", "platform": "darwin"}, bearer(admin))["token"].(string)
	for _, path := range []string{
		"/v1/sessions", "/v1/read?address=sess/1", "/v1/raw?address=sess/1",
		"/v1/raw?path=/x.jsonl&offset=0&length=10", "/v1/raw?source_id=" + uuid.NewString() + "&generation=0&offset=0&length=10",
	} {
		for _, c := range []struct {
			query string
			want  time.Duration
		}{{"", api.DefaultBudget}, {"timeout=2s", 2 * time.Second}, {"timeout=1h", api.MaxBudget}} {
			url := srv.URL + path
			if c.query != "" {
				url += map[bool]string{true: "&", false: "?"}[strings.Contains(path, "?")] + c.query
			}
			res := request(t, "GET", url, nil, bearer(device))
			body := read(res)
			res.Body.Close()
			var left time.Duration
			select {
			case left = <-d.left:
			default:
				t.Fatalf("%s: not called (%d %s)", url, res.StatusCode, body)
			}
			if left <= 0 || left > c.want || left < c.want-5*time.Second {
				t.Errorf("%s: budget %s, want %s", url, left, c.want)
			}
			if res.StatusCode != 504 || !strings.Contains(body, "timed out") {
				t.Errorf("%s: %d %s", url, res.StatusCode, body)
			}
		}
	}
}
