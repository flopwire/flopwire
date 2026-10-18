package devicesync

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
)

func uploadBuffersRequireCleared(t *testing.T, p *payload) {
	t.Helper()
	if p.buffers.parts != nil || p.buffers.tail != nil || p.buffers.tailFrom != 0 || p.cur != nil || p.rr != nil {
		t.Fatal("closed payload retains request buffer or redactor roots")
	}
}
func uploadBuffersRequirePart(t *testing.T, p part, raw []byte) {
	t.Helper()
	if p.e.Offset < 0 || p.e.Size <= 0 || p.e.Offset > int64(len(raw))-p.e.Size {
		t.Fatal("frame entry exceeds independent source bytes")
	}
	want := raw[p.e.Offset : p.e.Offset+p.e.Size]
	got, err := syncproto.Decompress(nil, p.z, p.e.Size, syncproto.Sum(want))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("frame differs from source span: %v", err)
	}
}

func TestUploadBuffersClosePreservesDeferredBackingForNextAuthorizedRequest(t *testing.T) {
	e := newEnv(t, Config{Chunk: ChunkParams{Min: 64, Avg: 128, Max: 256}, MaxRequestBytes: 256, HasThreshold: 1 << 30, SealAfter: -1}, 1<<20)
	sp := authorizedSpec(e, "owned-overflow.jsonl", transcript.StorageJSONLAppend)
	raw := jsonlLines(321, 15, 800)
	appendFile(t, sp.Path, raw)
	a := fileAuthorization(t, sp, int64(len(raw)))
	var firstPayload *payload
	var deferred part
	var firstWire int64
	e.sy.tr = operationTransport{Transport: e.client, flush: func(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
		p, ok := r.Payload.(*payload)
		if !ok || len(p.buffers.parts) == 0 {
			return nil, errors.New("fixture missing owned compressed frame")
		}
		firstPayload = p
		firstWire = p.wire
		deferred = e.sy.serialScratch.deferred
		if len(deferred.z) == 0 {
			return nil, errors.New("fixture failed to create overflow frame")
		}
		return e.client.Flush(ctx, r)
	}}
	outcome, err := e.sy.syncTurn(t.Context(), sp, nil, -1, a, captureSource)
	if err != nil || outcome != uploadPending {
		t.Fatalf("first turn %v %v", outcome, err)
	}
	if firstPayload == nil {
		t.Fatal("first actual request missing")
	}
	uploadBuffersRequireCleared(t, firstPayload)
	if firstPayload.err != nil || firstPayload.failed != -1 || firstPayload.wire != firstWire || firstWire <= 0 {
		t.Fatal("close changed successful request metadata")
	}
	cached := e.sy.serialScratch.deferred
	if len(cached.z) == 0 || &cached.z[0] != &deferred.z[0] || cached.e.Hash != deferred.e.Hash {
		t.Fatal("closing request consumed shared deferred frame")
	}
	uploadBuffersRequirePart(t, cached, raw)
	fresh := fileAuthorization(t, sp, int64(len(raw)))
	fresh.Proof.PolicyRequestDigest = strings.Repeat("f", 64)
	checks := 0
	fresh.Check = func(ctx context.Context) error { checks++; return ctx.Err() }
	var nextPayload *payload
	e.sy.tr = operationTransport{Transport: e.client, flush: func(ctx context.Context, r *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
		p, ok := r.Payload.(*payload)
		if !ok || len(p.buffers.parts) == 0 {
			return nil, errors.New("next actual request missing frame")
		}
		nextPayload = p
		first := p.buffers.parts[0]
		if first.e.Hash != cached.e.Hash || &first.z[0] != &cached.z[0] {
			return nil, errors.New("next authorized request did not reuse exact deferred backing")
		}
		if checks == 0 {
			return nil, errors.New("frame reuse bypassed fresh authorization")
		}
		return e.client.Flush(ctx, r)
	}}
	if _, err := e.sy.syncTurn(t.Context(), sp, nil, -1, fresh, resumeUpload); err != nil {
		t.Fatal(err)
	}
	if nextPayload == nil {
		t.Fatal("continuation missing")
	}
	uploadBuffersRequireCleared(t, nextPayload)
	e.sy.tr = e.client
	if err := e.sy.ResumeAuthorized(t.Context(), sp, fresh); err != nil {
		t.Fatal(err)
	}
	e.requireServerHas(sp.Path, fileID(a.Proof.Identity), 0, raw)
}

func TestUploadBuffersTailSuffixOwnsFullBackingCapacity(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	sp := e.spec("full-tail.jsonl", transcript.StorageJSONLAppend)
	raw := []byte("{\"record\":\"full verified tail backing, one byte transmitted\"}\n")
	appendFile(t, sp.Path, raw)
	src := tailCaptureSource(t, e.store, sp)
	e.sy.mu.Lock()
	defer e.sy.mu.Unlock()
	op := e.sy.ordinaryOperation()
	if err := op.capture(t.Context(), src, nil, -1, nil); err != nil {
		t.Fatal(err)
	}
	g := lifecycleGeneration(t, e, src)
	p := &payload{ctx: t.Context(), op: op, src: src, g: g, failed: -1}
	t.Cleanup(p.close)
	// Force the spool reader's io.ReadAll allocation, whose capacity can
	// exceed the literal body length while ownership still covers its root.
	if err := e.spool.PutTailVersion(src.ID, g.Gen, g.Tail.Hash, raw); err != nil {
		t.Fatal(err)
	}
	tail := g.Tail
	tail.From = int64(len(raw) - 1)
	p.loadTail(&tail)
	if p.err != nil {
		t.Fatal(p.err)
	}
	compressed, full := p.buffers.capacities()
	rootCapacity := int64(cap(p.buffers.tail))
	suffixCapacity := int64(cap(p.buffers.tail[tail.From:]))
	if compressed != 0 || full != rootCapacity || full < int64(len(raw)) || full-suffixCapacity != tail.From || !bytes.Equal(p.buffers.tail, raw) {
		t.Fatalf("owned tail capacity=%d root=%d suffix=%d compressed=%d fullbytes=%q", full, rootCapacity, suffixCapacity, compressed, p.buffers.tail)
	}
	// Request construction freezes the suffix bound with the owned root.
	tail.From = 0
	transmitted, err := io.ReadAll(p)
	if err != nil || !bytes.Equal(transmitted, []byte("\n")) {
		t.Fatalf("tail suffix=%q %v", transmitted, err)
	}
	_, afterRead := p.buffers.capacities()
	if afterRead != full {
		t.Fatal("cursor consumed the full tail owner charge")
	}
	p.close()
	p.close()
	uploadBuffersRequireCleared(t, p)
	if compressed, tail := p.buffers.capacities(); compressed != 0 || tail != 0 {
		t.Fatal("closed request still charges frame/tail roots")
	}
}

func TestUploadBuffersFailedNextBodyPreventsEarlierFlushAndKeepsErrorMetadata(t *testing.T) {
	e := newEnv(t, Config{Chunk: ChunkParams{Min: 64, Avg: 128, Max: 256}, MaxRequestBytes: 256, HasThreshold: 1 << 30, SealAfter: -1}, 1<<20)
	sp := e.spec("failed-next-body.jsonl", transcript.StorageJSONLAppend)
	raw := jsonlLines(322, 5, 800)
	appendFile(t, sp.Path, raw)
	src := tailCaptureSource(t, e.store, sp)
	e.sy.mu.Lock()
	op := e.sy.ordinaryOperation()
	err := op.capture(t.Context(), src, nil, -1, nil)
	e.sy.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	g := lifecycleGeneration(t, e, src)
	entries, err := e.store.entries(t.Context(), src.ID, g.Gen, 0, 2)
	if err != nil || len(entries) != 2 {
		t.Fatalf("fixture entries=%d err=%v", len(entries), err)
	}
	wrong := syncproto.Sum([]byte("independent deliberately incorrect second body digest"))
	if _, err := e.store.db.Exec(`UPDATE devsync_manifest SET hash=? WHERE source_id=? AND generation=? AND ordinal=1`, wrong[:], src.ID, g.Gen); err != nil {
		t.Fatal(err)
	}
	entries[1].Hash = wrong
	e.sy.mu.Lock()
	p := &payload{ctx: t.Context(), op: op, src: src, g: g, failed: -1}
	p.pack(entries, entries)
	e.sy.mu.Unlock()
	t.Cleanup(p.close)
	if !errors.Is(p.err, ErrSourceChanged) || p.failed != 1 || len(p.buffers.parts) != 1 || p.wire <= 0 || p.rr == nil {
		t.Fatalf("failed-next-body err=%v failed=%d parts=%d wire=%d", p.err, p.failed, len(p.buffers.parts), p.wire)
	}
	uploadBuffersRequirePart(t, p.buffers.parts[0], raw)
	if compressed, tail := p.buffers.capacities(); compressed != int64(cap(p.buffers.parts[0].z)) || tail != 0 {
		t.Fatal("owned frame capacity differs from actual compressed root")
	}
	one := make([]byte, 1)
	if n, err := p.Read(one); n != 1 || err != nil || len(p.cur) == 0 {
		t.Fatal("fixture did not establish a frame cursor alias")
	}
	wire := p.wire
	failure := p.err
	p.close()
	p.close()
	uploadBuffersRequireCleared(t, p)
	if p.err != failure || p.failed != 1 || p.wire != wire {
		t.Fatal("close erased failure evidence needed for ACK/gap handling")
	}
	tr := &authTransport{Transport: e.client}
	e.sy.tr = tr
	if err := e.sy.Resume(t.Context(), sp); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("actual continuation failure=%v", err)
	}
	if tr.flush != 0 || e.flushes() != 0 {
		t.Fatal("valid earlier body flushed before later materialization failed")
	}
	var acked int64
	if err := e.store.db.QueryRow(`SELECT acked FROM devsync_gens WHERE source_id=? AND generation=?`, src.ID, g.Gen).Scan(&acked); err != nil {
		t.Fatal(err)
	}
	if acked != 0 {
		t.Fatal("failed materialization advanced durable ACK")
	}
}

type uploadBuffersRoundTrip func(*http.Request) (*http.Response, error)

func (fn uploadBuffersRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type uploadBuffersGatedReader struct {
	payload                 *payload
	want                    []byte
	entered, finish, exited chan struct{}
	once                    sync.Once
	result                  chan error
}

func (r *uploadBuffersGatedReader) Read(b []byte) (int, error) {
	first := false
	r.once.Do(func() { first = true; close(r.entered); <-r.finish })
	if !first {
		return r.payload.Read(b)
	}
	defer close(r.exited)
	if !bytes.Equal(r.payload.buffers.tail, r.want) {
		err := errors.New("request tail root cleared before encoder finished")
		r.result <- err
		return 0, err
	}
	n, err := r.payload.Read(b)
	if n != len(r.want) || !bytes.Equal(b[:n], r.want) {
		r.result <- fmt.Errorf("encoder received %d bytes, expected literal tail", n)
	} else {
		r.result <- nil
	}
	return n, err
}

func TestUploadBuffersActualClientCancellationJoinsEncoderBeforeClose(t *testing.T) {
	e := newEnv(t, Config{SealAfter: -1}, 1<<20)
	sp := e.spec("joined-encoder.jsonl", transcript.StorageJSONLAppend)
	raw := []byte("{\"record\":\"encoder still owns this literal tail\"}\n")
	appendFile(t, sp.Path, raw)
	entered, finish, exited, transportCanceled := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	joined := make(chan struct{})
	t.Cleanup(func() {
		once.Do(func() { close(finish) })
		cancel()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Error("canceled upload did not join before fixture cleanup")
		}
	})
	client := *e.client
	client.HTTP = &http.Client{Transport: uploadBuffersRoundTrip(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var prefix [8]byte
		if _, err := io.ReadFull(r.Body, prefix[:]); err != nil {
			return nil, err
		}
		header := make([]byte, binary.BigEndian.Uint32(prefix[4:]))
		if _, err := io.ReadFull(r.Body, header); err != nil {
			return nil, err
		}
		var h syncproto.FlushHeader
		if err := json.Unmarshal(header, &h); err != nil {
			return nil, err
		}
		if !bytes.Equal(prefix[:4], []byte{'T', 'M', 'F', 1}) || h.Version != syncproto.Version || h.Source.Path != sp.Path || h.Generation != 0 || len(h.Entries) != 0 || len(h.Bodies) != 0 || h.Tail == nil || h.Tail.From != 0 || h.Tail.Size != int64(len(raw)) || h.Tail.Hash != syncproto.Sum(raw) {
			return nil, errors.New("encoded request header differs from literal native tail")
		}
		<-r.Context().Done()
		close(transportCanceled)
		return nil, r.Context().Err()
	})}
	var original *payload
	readerResult := make(chan error, 1)
	e.sy.tr = operationTransport{Transport: &client, flush: func(ctx context.Context, request *syncproto.FlushRequest) (*syncproto.FlushResponse, error) {
		p, ok := request.Payload.(*payload)
		if !ok {
			return nil, errors.New("original payload type missing")
		}
		original = p
		gated := &uploadBuffersGatedReader{payload: p, want: raw, entered: entered, finish: finish, exited: exited, result: readerResult}
		copied := *request
		copied.Payload = gated
		response, err := client.Flush(ctx, &copied)
		select {
		case <-exited:
		default:
			return nil, errors.New("Client.Flush returned before encoder released payload")
		}
		return response, err
	}}
	go func() { defer close(joined); done <- e.sy.Sync(ctx, sp) }()
	operationSignal(t, entered)
	cancel()
	operationSignal(t, transportCanceled)
	if original == nil || !bytes.Equal(original.buffers.tail, raw) || original.rr == nil {
		t.Fatal("request roots disappeared while canceled encoder still borrowed payload")
	}
	once.Do(func() { close(finish) })
	if err := operationResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled actual Flush=%v", err)
	}
	operationSignal(t, exited)
	if err := operationResult(t, readerResult); err != nil {
		t.Fatal(err)
	}
	uploadBuffersRequireCleared(t, original)
	var acked int64
	var tailAcked bool
	if err := e.store.db.QueryRow(`SELECT acked,tail_acked FROM devsync_gens`).Scan(&acked, &tailAcked); err != nil {
		t.Fatal(err)
	}
	if acked != 0 || tailAcked {
		t.Fatal("canceled Flush advanced durable ACK")
	}
}
