package synctest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// Target is a protocol server under test: a client holding a valid device
// credential and a way to read back a generation's bytes.
type Target struct {
	Client      *syncproto.Client
	Reconstruct func(path, fileID string, gen int64) ([]byte, error)
}

// Conformance runs the protocol rules of the syncproto package
// documentation against a server. newTarget returns a fresh, empty server.
// The in-memory Server passes it; so must the real one.
func Conformance(t *testing.T, newTarget func(t *testing.T) Target) {
	t.Run("rules", func(t *testing.T) { protocolRules(t, newTarget(t)) })
	t.Run("limits", func(t *testing.T) { limits(t, newTarget(t)) })
	t.Run("auth", func(t *testing.T) {
		c := *newTarget(t).Client
		c.Token = "wrong"
		_, err := c.Has(context.Background(), nil)
		var he *syncproto.HTTPError
		if !errors.As(err, &he) || he.Status != 401 || he.Retryable() || syncproto.Retryable(err) {
			t.Fatalf("want 401 not retryable, got %v", err)
		}
	})
}

type req struct {
	gen     int64
	entries []syncproto.Entry
	bodies  [][]byte
	tail    []byte
	from    int64 // tail delta base
	tailOff int64
}

func entries(chunks ...[]byte) []syncproto.Entry {
	var out []syncproto.Entry
	var off int64
	for i, c := range chunks {
		out = append(out, syncproto.Entry{Ordinal: int64(i), Hash: syncproto.Sum(c), Offset: off, Size: int64(len(c))})
		off += int64(len(c))
	}
	return out
}

func flush(t *testing.T, c *syncproto.Client, r req) *syncproto.FlushResponse {
	t.Helper()
	h := syncproto.FlushHeader{Version: syncproto.Version, Generation: r.gen, Entries: r.entries,
		Source: syncproto.Source{Path: "/s.jsonl", FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"}}
	var payload []byte
	for _, b := range r.bodies {
		body, z := syncproto.EncodeBody(b)
		h.Bodies = append(h.Bodies, body)
		payload = append(payload, z...)
	}
	if r.tail != nil {
		h.Tail = &syncproto.Tail{Offset: r.tailOff, Size: int64(len(r.tail)), Hash: syncproto.Sum(r.tail), From: r.from}
		payload = append(payload, r.tail[r.from:]...)
	}
	resp, err := c.Flush(context.Background(), &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func protocolRules(t *testing.T, s Target) {
	c := s.Client
	a, b, x := []byte("aaaa"), []byte("bbbbbb"), []byte("xxxx")
	all := entries(a, b)

	// Body missing for a chunk the server lacks: partial, Missing lists it.
	r := flush(t, c, req{entries: all, bodies: [][]byte{a}, tail: []byte("t1"), tailOff: 10})
	if r.Status != syncproto.StatusPartial || r.AckedEntries != 1 || len(r.Missing) != 1 || r.Missing[0] != syncproto.Sum(b) || r.TailAcked {
		t.Fatalf("partial: %+v", r)
	}
	// Re-send everything: ok, idempotent on the first entry.
	r = flush(t, c, req{entries: all, bodies: [][]byte{a, b}, tail: []byte("t1"), tailOff: 10})
	if r.Status != syncproto.StatusOK || r.AckedEntries != 2 || r.AckedOffset != 10 || !r.TailAcked || r.TailSize != 2 {
		t.Fatalf("ok: %+v", r)
	}
	// The same request again changes nothing.
	r = flush(t, c, req{entries: all, bodies: [][]byte{a, b}, tail: []byte("t1"), tailOff: 10})
	if r.Status != syncproto.StatusOK || r.AckedEntries != 2 || r.TailSize != 2 {
		t.Fatalf("replay: %+v", r)
	}
	// Tail delta on the stored tail.
	r = flush(t, c, req{tail: []byte("t1t2"), from: 2, tailOff: 10})
	if !r.TailAcked || r.TailSize != 4 {
		t.Fatalf("delta: %+v", r)
	}
	// A late retry of the shorter tail never shrinks it.
	flush(t, c, req{tail: []byte("t1"), tailOff: 10})
	if got, _ := s.Reconstruct("/s.jsonl", "1:1", 0); string(got) != "aaaabbbbbbt1t2" {
		t.Fatalf("reconstruct %q", got)
	}
	// Delta whose base does not hash: new generation required.
	r = flush(t, c, req{tail: []byte("zzt3"), from: 2, tailOff: 10})
	if r.Status != syncproto.StatusNewGeneration {
		t.Fatalf("tail mismatch: %+v", r)
	}
	// Delta without a stored base at that offset: not stored, the answer
	// says what the server holds.
	r = flush(t, c, req{tail: []byte("q1q2"), from: 2, tailOff: 99})
	if r.TailAcked || r.Status != syncproto.StatusPartial || r.TailOffset != 10 || r.TailSize != 4 {
		t.Fatalf("delta without base: %+v", r)
	}
	// Gap: ordinal 3 while the server has 2.
	gap := []syncproto.Entry{{Ordinal: 3, Hash: syncproto.Sum(x), Offset: 14, Size: 4}}
	r = flush(t, c, req{entries: gap, bodies: [][]byte{x}})
	if r.Status != syncproto.StatusPartial || r.AckedEntries != 2 {
		t.Fatalf("gap: %+v", r)
	}
	// Conflict with a committed entry: new generation required.
	r = flush(t, c, req{entries: entries(x), bodies: [][]byte{x}})
	if r.Status != syncproto.StatusNewGeneration {
		t.Fatalf("conflict: %+v", r)
	}
	// Finalizing past the stored tail deletes it.
	t1t2 := []byte("t1t2")
	fin := []syncproto.Entry{{Ordinal: 2, Hash: syncproto.Sum(t1t2), Offset: 10, Size: 4}}
	r = flush(t, c, req{entries: fin, bodies: [][]byte{t1t2}})
	if r.Status != syncproto.StatusOK || r.AckedEntries != 3 || r.AckedOffset != 14 || r.TailSize != 0 {
		t.Fatalf("finalize: %+v", r)
	}
	if got, _ := s.Reconstruct("/s.jsonl", "1:1", 0); string(got) != "aaaabbbbbbt1t2" {
		t.Fatalf("reconstruct after finalize %q", got)
	}
	// Generation 1 starts empty; generation 0 is then stale. A chunk the
	// server holds needs no body.
	r = flush(t, c, req{gen: 1, entries: entries(a)})
	if r.Status != syncproto.StatusOK || r.Generation != 1 || r.TailSize != 0 || r.AckedEntries != 1 {
		t.Fatalf("gen 1: %+v", r)
	}
	r = flush(t, c, req{gen: 0})
	if r.Status != syncproto.StatusStaleGeneration || r.Generation != 1 {
		t.Fatalf("stale: %+v", r)
	}
	if got, _ := s.Reconstruct("/s.jsonl", "1:1", 0); string(got) != "aaaabbbbbbt1t2" {
		t.Fatalf("gen 0 after gen 1: %q", got)
	}
	// Has lists what the server lacks.
	missing, err := c.Has(context.Background(), []syncproto.Hash{syncproto.Sum(a), syncproto.Sum(x)})
	if err != nil || len(missing) != 1 || missing[0] != syncproto.Sum(x) {
		t.Fatalf("has: %v %v", missing, err)
	}
	// A body that does not decode to its announcement is refused whole.
	yz := syncproto.Compress(nil, []byte("yyyy"))
	h := syncproto.FlushHeader{Version: syncproto.Version, Generation: 1, Entries: []syncproto.Entry{{Ordinal: 1, Hash: syncproto.Sum(x), Offset: 4, Size: 4}},
		Bodies: []syncproto.Body{{Hash: syncproto.Sum(x), Size: 4, ZSize: int64(len(yz))}},
		Source: syncproto.Source{Path: "/s.jsonl", FileID: "1:1", Agent: "codex", StorageKind: "jsonl_append"}}
	_, err = c.Flush(context.Background(), &syncproto.FlushRequest{Header: h, Payload: bytes.NewReader(yz)})
	var he *syncproto.HTTPError
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("bad body: %v", err)
	}
	if got, _ := s.Reconstruct("/s.jsonl", "1:1", 1); string(got) != "aaaa" {
		t.Fatalf("gen 1 after bad body: %q", got)
	}
}

// rawFlush posts a hand-built frame (header and payload as given, no
// client-side checks) and returns the HTTP status.
func rawFlush(t *testing.T, c *syncproto.Client, h syncproto.FlushHeader, payload []byte) int {
	t.Helper()
	hdr, err := json.Marshal(&h)
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte{'T', 'M', 'F', syncproto.Version, 0, 0, 0, 0}, hdr...)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(hdr)))
	frame = append(frame, payload...)
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(c.Server, "/")+syncproto.PathFlush, bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", syncproto.FlushContentType)
	req.Header.Set(syncproto.HeaderVersion, strconv.Itoa(syncproto.Version))
	req.Header.Set("Authorization", "Bearer "+c.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// limits: hostile announcements are refused with 400 before they cost
// memory, and entry sizes must match the chunk they name (S2, S7).
func limits(t *testing.T, s Target) {
	c := s.Client
	src := syncproto.Source{Path: "/l.jsonl", FileID: "1:2", Agent: "codex", StorageKind: "jsonl_append"}
	hdr := func() syncproto.FlushHeader { return syncproto.FlushHeader{Version: syncproto.Version, Source: src} }
	// A body or tail one byte past the 16MB part bound, sent in full: a
	// server without the bound accepts it, so the case fails fast against
	// one (never declare gigabytes here: an unbounded server allocates what
	// the header announces before it reads the payload).
	big := bytes.Repeat([]byte("z"), syncproto.MaxPartBytes+1)
	zbig := syncproto.Compress(nil, big)
	for name, tc := range map[string]struct {
		mut     func(*syncproto.FlushHeader)
		payload []byte
	}{
		"huge body": {func(h *syncproto.FlushHeader) {
			h.Bodies = []syncproto.Body{{Hash: syncproto.Sum(big), Size: int64(len(big)), ZSize: int64(len(zbig))}}
		}, zbig},
		// A zip bomb: a small frame announced at the bound that decodes one
		// byte past it.
		"body decodes past the bound": {func(h *syncproto.FlushHeader) {
			h.Bodies = []syncproto.Body{{Hash: syncproto.Sum(big[:syncproto.MaxPartBytes]), Size: syncproto.MaxPartBytes, ZSize: int64(len(zbig))}}
		}, zbig},
		"huge tail": {func(h *syncproto.FlushHeader) { h.Tail = &syncproto.Tail{Offset: 0, Size: int64(len(big))} }, big},
		"entry overflow": {func(h *syncproto.FlushHeader) {
			h.Entries = []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Sum([]byte("z")), Offset: math.MaxInt64 - 1, Size: 4}}
		}, nil},
		"tail overflow": {func(h *syncproto.FlushHeader) { h.Tail = &syncproto.Tail{Offset: math.MaxInt64 - 1, Size: 4} }, nil},
	} {
		h := hdr()
		tc.mut(&h)
		if st := rawFlush(t, c, h, tc.payload); st != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, st)
		}
	}
	a := []byte("aaaa")
	// An entry that lies about its body's size.
	h := hdr()
	h.Entries = []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Sum(a), Offset: 0, Size: 5}}
	ab, az := syncproto.EncodeBody(a)
	h.Bodies = []syncproto.Body{ab}
	if st := rawFlush(t, c, h, az); st != http.StatusBadRequest {
		t.Fatalf("entry size != body size: status %d, want 400", st)
	}
	// Store the chunk honestly, then name it with a wrong size.
	r := flush(t, c, req{entries: entries(a), bodies: [][]byte{a}})
	if r.Status != syncproto.StatusOK {
		t.Fatalf("honest flush: %+v", r)
	}
	h = hdr()
	h.Source.Path = "/l2.jsonl"
	h.Entries = []syncproto.Entry{{Ordinal: 0, Hash: syncproto.Sum(a), Offset: 0, Size: 3}}
	if st := rawFlush(t, c, h, nil); st != http.StatusBadRequest {
		t.Fatalf("entry size != stored chunk size: status %d, want 400", st)
	}
}
