package ingest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
)

// Barrier sits after the actual MinIO Put. It deliberately ignores canceled
// context until released: admission must follow real work rather than signals.
type twoFlushObjects struct {
	Objects
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (o *twoFlushObjects) Put(ctx context.Context, key string, data []byte) error {
	if err := o.Objects.Put(ctx, key, data); err != nil {
		return err
	}
	o.entered <- struct{}{}
	<-o.release
	return nil
}
func (o *twoFlushObjects) finish() { o.once.Do(func() { close(o.release) }) }

func twoFlushRequest(path string, parts ...[]byte) *syncproto.FlushRequest {
	h := syncproto.FlushHeader{Version: syncproto.Version, CapturedAt: time.Now(), Source: syncproto.Source{Path: path, FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"}}
	readers := make([]io.Reader, 0, len(parts))
	var offset int64
	for i, p := range parts {
		h.Entries = append(h.Entries, syncproto.Entry{Ordinal: int64(i), Hash: syncproto.Sum(p), Offset: offset, Size: int64(len(p))})
		h.Bodies = append(h.Bodies, bodyOf(p)...)
		readers = append(readers, zpayload(p))
		offset += int64(len(p))
	}
	return &syncproto.FlushRequest{Header: h, Payload: io.MultiReader(readers...)}
}

func twoFlushServer(t *testing.T, e *env, objects Objects) (*syncproto.Client, *FlushAdmission) {
	t.Helper()
	a, err := NewFlushAdmissionPerDevice(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Pool: e.pool, Objects: objects, Admission: a}
	httpServer := httptest.NewServer(api.New(e.store, api.Config{Sync: s}).Handler(nil))
	t.Cleanup(httpServer.Close)
	return &syncproto.Client{Server: httpServer.URL, Token: e.token, HTTP: httpServer.Client()}, a
}

func twoFlushWait(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("real MinIO request did not reach overlap barrier")
	}
}
func twoFlushIdle(t *testing.T, a *FlushAdmission) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if admissionCount(a) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("finished handlers retained admission")
}
func twoFlushRefusal(t *testing.T, err error, code string) {
	t.Helper()
	var he *syncproto.HTTPError
	if !errors.As(err, &he) || he.Status != 429 || he.Body.Code != code {
		t.Fatalf("want429 %s got%v", code, err)
	}
}

type twoFlushResult struct {
	response *syncproto.FlushResponse
	err      error
}

func TestTwoFlushAuthenticatedOverlapPathAndCancellation(t *testing.T) {
	e := newEnv(t)
	o := &twoFlushObjects{Objects: e.objects, entered: make(chan struct{}, 8), release: make(chan struct{})}
	c, a := twoFlushServer(t, e, o)
	defer o.finish()
	got, err := c.UploadConcurrency(e.ctx, 2)
	if err != nil || got != 2 {
		t.Fatalf("authenticated negotiation %d %v", got, err)
	}
	policy := devicesync.PolicyClient{Server: c.Server, Token: c.Token, HTTP: c.HTTP}
	if cap, err := policy.Capabilities(e.ctx); err != nil || cap.MaxConcurrentFlushes != 1 {
		t.Fatalf("legacy policy contract changed: %+v %v", cap, err)
	}
	anon := *c
	anon.Token = "invalid"
	if got, err = anon.UploadConcurrency(e.ctx, 2); err == nil || got != 0 {
		t.Fatal("unauthenticated capabilities enabled upload workers")
	}
	first := []byte("{\"marker\":\"first\"}\n")
	second := []byte("{\"marker\":\"second\"}\n")
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	one := make(chan twoFlushResult, 1)
	two := make(chan twoFlushResult, 1)
	go func() { r, err := c.Flush(ctx, twoFlushRequest("/two/first", first)); one <- twoFlushResult{r, err} }()
	twoFlushWait(t, o.entered)
	duplicate := twoFlushRequest("/two/first", second)
	duplicate.Header.Source.FileID = "replacement"
	duplicate.Header.Generation = 9
	_, err = c.Flush(e.ctx, duplicate)
	twoFlushRefusal(t, err, "source_busy")
	go func() {
		r, err := c.Flush(e.ctx, twoFlushRequest("/two/second", second))
		two <- twoFlushResult{r, err}
	}()
	twoFlushWait(t, o.entered)
	if admissionCount(a) != 2 {
		t.Fatal("two same-device requests did not actually overlap")
	}
	_, err = c.Flush(e.ctx, twoFlushRequest("/two/third", first))
	twoFlushRefusal(t, err, "device_busy")
	cancel()
	select {
	case result := <-one:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancel: %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client cancellation did not finish")
	}
	if admissionCount(a) != 2 {
		t.Fatal("cancellation released still-running server request")
	}
	o.finish()
	select {
	case result := <-two:
		if result.err != nil || result.response.AckedEntries != 1 {
			t.Fatalf("second request: %+v %v", result.response, result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second request did not complete")
	}
	twoFlushIdle(t, a)
	response, err := c.Flush(e.ctx, twoFlushRequest("/two/first", first))
	if err != nil || response.AckedEntries != 1 {
		t.Fatalf("canceled source retry %+v %v", response, err)
	}
	for _, p := range [][]byte{first, second} {
		got, err := GetChunk(e.ctx, e.objects, Chunk{Hash: syncproto.Sum(p), Size: int64(len(p)), Key: ChunkKey(syncproto.Sum(p))})
		if err != nil || !bytes.Equal(got, p) {
			t.Fatal("actual MinIO bytes lost")
		}
	}
	if e.count(`SELECT count(*) FROM sources WHERE device_id=$1`, e.deviceID) != 2 || e.count(`SELECT count(*) FROM manifest_entries`) != 2 {
		t.Fatal("refused/canceled request created duplicate or lost manifest")
	}
}

func TestTwoFlushSharedChunksOppositeOrderPartialRetry(t *testing.T) {
	e := newEnv(t)
	o := &twoFlushObjects{Objects: e.objects, entered: make(chan struct{}, 8), release: make(chan struct{})}
	c, a := twoFlushServer(t, e, o)
	defer o.finish()
	h := []byte("{\"shared\":\"H\"}\n")
	j := []byte("{\"shared\":\"J\"}\n")
	results := make(chan twoFlushResult, 2)
	go func() {
		r, err := c.Flush(e.ctx, twoFlushRequest("/opposite/one", h, j))
		results <- twoFlushResult{r, err}
	}()
	twoFlushWait(t, o.entered)
	go func() {
		r, err := c.Flush(e.ctx, twoFlushRequest("/opposite/two", j, h))
		results <- twoFlushResult{r, err}
	}()
	twoFlushWait(t, o.entered)
	o.finish()
	// At least the first completing request must acknowledge a partial prefix:
	// each initially owns its first hash and cannot wait on the other's lock.
	partial := false
	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.response.Status == syncproto.StatusPartial {
				partial = true
				if result.response.AckedEntries != 1 || len(result.response.Missing) != 1 {
					t.Fatalf("partial prefix %+v", result.response)
				}
			}
		case <-time.After(10 * time.Second):
			t.Fatal("shared chunks deadlocked")
		}
	}
	if !partial {
		t.Fatal("fixture did not exercise partial shared-chunk progress")
	}
	twoFlushIdle(t, a)
	for _, pair := range []struct {
		path  string
		parts [][]byte
	}{{"/opposite/one", [][]byte{h, j}}, {"/opposite/two", [][]byte{j, h}}} {
		r, err := c.Flush(e.ctx, twoFlushRequest(pair.path, pair.parts...))
		if err != nil || r.Status != syncproto.StatusOK || r.AckedEntries != 2 {
			t.Fatalf("shared chunk retry %+v %v", r, err)
		}
	}
	if e.count(`SELECT count(*) FROM manifest_entries`) != 4 || e.count(`SELECT count(*) FROM chunks WHERE state='committed'`) != 2 {
		t.Fatal("shared chunk retry lost or duplicated content")
	}
}
