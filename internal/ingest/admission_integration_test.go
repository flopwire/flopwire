package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// The first chunk reaches real MinIO under a database reservation before the
// second payload is interrupted. Both the admission slot and the connection /
// chunk locks must be reusable by a complete retry of the same source.
func TestServeSyncAdmissionInterruptedPayloadAndSuccessfulRetry(t *testing.T) {
	e := newEnv(t)
	a := admissionOwner(t, 1)
	s := &Server{Pool: e.pool, Objects: e.objects, Admission: a}
	first := []byte("{\"marker\":\"admission-first\"}\n")
	second := []byte("{\"marker\":\"admission-second\"}\n")
	h1, h2 := syncproto.Sum(first), syncproto.Sum(second)
	wire := &syncproto.FlushRequest{Header: syncproto.FlushHeader{
		Version: syncproto.Version, CapturedAt: time.Now(),
		Source:  syncproto.Source{Path: "/synthetic/admission.jsonl", FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"},
		Entries: []syncproto.Entry{{Ordinal: 0, Hash: h1, Size: int64(len(first))}, {Ordinal: 1, Hash: h2, Offset: int64(len(first)), Size: int64(len(second))}},
		Bodies:  append(bodyOf(first), bodyOf(second)...),
	}, Payload: io.MultiReader(zpayload(first), zpayload(second))}
	var frame bytes.Buffer
	if err := syncproto.EncodeFlush(&frame, wire); err != nil {
		t.Fatal(err)
	}
	full := frame.Bytes()
	w := httptest.NewRecorder()
	s.ServeSync(w, admissionRequest(e.ctx, bytes.NewReader(full[:len(full)-1])), e.deviceID)
	if w.Code != http.StatusBadRequest || admissionCount(a) != 0 {
		t.Fatalf("interrupted payload status=%d active=%d", w.Code, admissionCount(a))
	}
	if e.count(`SELECT count(*) FROM generations`) != 0 {
		t.Fatal("interrupted payload committed a generation")
	}
	got, err := GetChunk(context.Background(), e.objects, Chunk{Hash: h1, Size: int64(len(first)), Key: ChunkKey(h1)})
	if err != nil || !bytes.Equal(got, first) {
		t.Fatal("test did not reach real object storage before payload interruption")
	}
	// A leaked connection or advisory lock cannot be hidden by an unlimited
	// wait. Retry on the same server, owner, device and source under a budget.
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	w = httptest.NewRecorder()
	s.ServeSync(w, admissionRequest(ctx, bytes.NewReader(full)), e.deviceID)
	var response syncproto.FlushResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || response.AckedEntries != 2 || admissionCount(a) != 0 {
		t.Fatalf("retry status=%d acked=%d active=%d", w.Code, response.AckedEntries, admissionCount(a))
	}
	if e.count(`SELECT count(*) FROM generations`) != 1 || e.count(`SELECT count(*) FROM manifest_entries`) != 2 {
		t.Fatal("complete retry did not commit exactly one two-chunk generation")
	}
	got, err = GetChunk(ctx, e.objects, Chunk{Hash: h2, Size: int64(len(second)), Key: ChunkKey(h2)})
	if err != nil || !bytes.Equal(got, second) {
		t.Fatal("complete retry did not preserve the second chunk")
	}
}
