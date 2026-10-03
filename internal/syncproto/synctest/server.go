// Package synctest is an in-memory server for the sync protocol, for tests
// of the device side. It implements every rule of the syncproto package
// documentation and is the reference behaviour for the real server (B3).
package synctest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/syncproto"
)

type chunk struct {
	size int64
	data []byte // nil in Discard mode
}

type generation struct {
	entries  []syncproto.Entry
	tail     *syncproto.Tail
	tailData []byte
}

func (g *generation) end() int64 {
	if n := len(g.entries); n > 0 {
		return g.entries[n-1].End()
	}
	return 0
}

// Source is the server's view of one source.
type Source struct {
	Desc   syncproto.Source
	Latest int64
	Gens   map[int64]*generation
	// Redaction is the record the last flush carried.
	Redaction *syncproto.Redaction
}

// Server is an in-memory protocol server. The zero value is not usable;
// call New.
type Server struct {
	Token string
	// Discard keeps only chunk hashes and sizes, not bytes (corpus dry runs).
	Discard bool

	mu      sync.Mutex
	down    bool
	ackCap  int  // >0: apply at most this many new entries per request
	dropAck bool // commit, then answer 503 (lost acknowledgement)
	chunks  map[syncproto.Hash]chunk
	sources map[[2]string]*Source

	// Stats.
	Requests      int
	FlushRequests int
	BodyBytes     int64 // chunk bytes received, uncompressed (including duplicates)
	WireBytes     int64 // the same bodies' compressed bytes on the wire
	TailBytes     int64
	FrameBytes    []int64 // per flush request: bodies + tail bytes
}

func New(token string) *Server {
	return &Server{Token: token, chunks: map[syncproto.Hash]chunk{}, sources: map[[2]string]*Source{}}
}

// Lock and Unlock guard the exported stats fields.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// SetDown makes every request fail with 503 (a sleeping laptop).
func (s *Server) SetDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

// SetAckCap limits how many new entries one request may commit (0: none).
func (s *Server) SetAckCap(n int) { s.mu.Lock(); s.ackCap = n; s.mu.Unlock() }

// SetDropAck commits the next requests but answers 503, as if the answer
// were lost after the commit.
func (s *Server) SetDropAck(v bool) { s.mu.Lock(); s.dropAck = v; s.mu.Unlock() }

// DeleteChunk forgets a stored chunk, as if the server lost it.
func (s *Server) DeleteChunk(h syncproto.Hash) { s.mu.Lock(); delete(s.chunks, h); s.mu.Unlock() }

// UniqueChunks returns the number and total size of distinct chunks stored.
func (s *Server) UniqueChunks() (n int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chunks {
		bytes += c.size
	}
	return len(s.chunks), bytes
}

// Source returns a copy of the server state for (path, fileID).
func (s *Server) Source(path, fileID string) (desc syncproto.Source, latest int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[[2]string{path, fileID}]
	if src == nil {
		return desc, 0, false
	}
	return src.Desc, src.Latest, true
}

// Redaction returns the redaction record of the last flush of a source.
func (s *Server) Redaction(path, fileID string) *syncproto.Redaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	if src := s.sources[[2]string{path, fileID}]; src != nil {
		return src.Redaction
	}
	return nil
}

// Manifest returns the committed entries and the tail of a generation.
func (s *Server) Manifest(path, fileID string, gen int64) ([]syncproto.Entry, *syncproto.Tail) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[[2]string{path, fileID}]
	if src == nil || src.Gens[gen] == nil {
		return nil, nil
	}
	g := src.Gens[gen]
	return append([]syncproto.Entry(nil), g.entries...), g.tail
}

// Reconstruct concatenates a generation's chunks and tail. It fails in
// Discard mode.
func (s *Server) Reconstruct(path, fileID string, gen int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.sources[[2]string{path, fileID}]
	if src == nil || src.Gens[gen] == nil {
		return nil, errors.New("synctest: no such generation")
	}
	g := src.Gens[gen]
	var out []byte
	for _, e := range g.entries {
		c := s.chunks[e.Hash]
		if c.data == nil && e.Size > 0 {
			return nil, errors.New("synctest: chunk bytes not kept")
		}
		out = append(out, c.data...)
	}
	return append(out, g.tailData...), nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests++
	down := s.down
	s.mu.Unlock()
	switch {
	case down:
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "server down")
		return
	case r.Method != http.MethodPost:
		writeErr(w, http.StatusMethodNotAllowed, "bad_request", "POST only")
		return
	case r.Header.Get("Authorization") != "Bearer "+s.Token:
		writeErr(w, http.StatusUnauthorized, "unauthorized", "bad device credential")
		return
	case r.Header.Get(syncproto.HeaderVersion) != strconv.Itoa(syncproto.Version):
		writeErr(w, http.StatusBadRequest, "unsupported_version", "want version 1")
		return
	}
	switch r.URL.Path {
	case syncproto.PathHas:
		s.has(w, r)
	case syncproto.PathFlush:
		s.flush(w, r)
	default:
		writeErr(w, http.StatusNotFound, "not_found", r.URL.Path)
	}
}

func (s *Server) has(w http.ResponseWriter, r *http.Request) {
	var req syncproto.HasRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Hashes) > syncproto.MaxHasHashes {
		writeErr(w, http.StatusBadRequest, "bad_request", "bad has request")
		return
	}
	out := syncproto.HasResponse{Version: syncproto.Version, Missing: []syncproto.Hash{}}
	s.mu.Lock()
	for _, h := range req.Hashes {
		if _, ok := s.chunks[h]; !ok {
			out.Missing = append(out.Missing, h)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) flush(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), syncproto.FlushContentType) {
		writeErr(w, http.StatusBadRequest, "bad_request", "content type")
		return
	}
	h, pr, err := syncproto.DecodeFlush(r.Body)
	if err != nil {
		code := "bad_request"
		if strings.Contains(err.Error(), "unsupported version") {
			code = "unsupported_version"
		}
		writeErr(w, http.StatusBadRequest, code, err.Error())
		return
	}
	// Step 1: read and verify every body before touching state.
	bodies := map[syncproto.Hash][]byte{}
	bodySizes := map[syncproto.Hash]int64{}
	var bodyBytes, wireBytes int64
	for {
		b, data, _, err := pr.NextBody(nil, nil)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		bodyBytes += b.Size
		wireBytes += b.ZSize
		if s.Discard {
			data = nil
		}
		bodies[b.Hash], bodySizes[b.Hash] = data, b.Size
	}
	tailData, err := pr.ReadTail(nil)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.FlushRequests++
	s.BodyBytes += bodyBytes
	s.WireBytes += wireBytes
	s.TailBytes += int64(len(tailData))
	s.FrameBytes = append(s.FrameBytes, h.PayloadSize())
	defer func() {
		if src := s.sources[[2]string{h.Source.Path, h.Source.FileID}]; src != nil {
			src.Redaction = h.Redaction
		}
	}()

	key := [2]string{h.Source.Path, h.Source.FileID}
	src := s.sources[key]
	if src == nil {
		src = &Source{Desc: h.Source, Latest: -1, Gens: map[int64]*generation{}}
		s.sources[key] = src
	}
	// The repository follows every flush, as on the real server.
	src.Desc.Checkout, src.Desc.Remote = h.Source.Checkout, h.Source.Remote
	resp := syncproto.FlushResponse{Version: syncproto.Version, Status: syncproto.StatusOK}
	// Step 2: generation.
	if h.Generation < src.Latest {
		resp.Status, resp.Generation = syncproto.StatusStaleGeneration, src.Latest
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if h.Generation > src.Latest {
		src.Latest = h.Generation
		src.Gens[h.Generation] = &generation{}
	}
	src.Desc = h.Source
	g := src.Gens[h.Generation]
	resp.Generation = src.Latest

	// Step 3: entries.
	applied, all := 0, true
	for _, e := range h.Entries {
		n := int64(len(g.entries))
		if e.Ordinal < n {
			if c := g.entries[e.Ordinal]; c.Hash != e.Hash || c.Offset != e.Offset {
				resp.Status = syncproto.StatusNewGeneration
				all = false
				break
			}
			continue
		}
		if e.Ordinal > n || (s.ackCap > 0 && applied >= s.ackCap) {
			all = false
			break
		}
		c, held := s.chunks[e.Hash]
		if !held {
			data, ok := bodies[e.Hash]
			if !ok {
				resp.Missing = append(resp.Missing, e.Hash)
				all = false
				break
			}
			c = chunk{size: bodySizes[e.Hash], data: data}
		}
		if e.Offset != g.end() {
			writeErr(w, http.StatusBadRequest, "bad_request", "entry offset does not continue the manifest")
			return
		}
		if c.size != e.Size {
			writeErr(w, http.StatusBadRequest, "bad_request", "entry size does not match the chunk")
			return
		}
		s.chunks[e.Hash] = c
		g.entries = append(g.entries, e)
		applied++
	}
	// Step 4: tail.
	if g.tail != nil && g.tail.Offset < g.end() {
		g.tail, g.tailData = nil, nil
	}
	if all {
		switch t := h.Tail; {
		case t == nil:
			resp.TailAcked = true
		case t.Offset == g.end():
			var stored []byte
			if g.tail != nil && g.tail.Offset == t.Offset {
				stored = g.tailData
			}
			whole, err := t.Apply(stored, tailData)
			if errors.Is(err, syncproto.ErrTailMismatch) {
				resp.Status = syncproto.StatusNewGeneration
				break
			}
			if s.Discard && err != nil && g.tail != nil && g.tail.Offset == t.Offset && g.tail.Size >= t.From {
				err = nil // bytes not kept: trust the base
			}
			if err != nil {
				break // base missing: TailOffset/TailSize tell the device what to resend
			}
			if g.tail == nil || g.tail.Offset != t.Offset || g.tail.Size <= t.Size {
				if s.Discard {
					whole = nil
				}
				tt := *t
				tt.From = 0
				g.tail, g.tailData = &tt, whole
			}
			resp.TailAcked = true
		}
	}
	if g.tail != nil {
		resp.TailOffset, resp.TailSize = g.tail.Offset, g.tail.Size
	}
	if resp.Status == syncproto.StatusOK && !(all && resp.TailAcked) {
		resp.Status = syncproto.StatusPartial
	}
	resp.AckedEntries, resp.AckedOffset = int64(len(g.entries)), g.end()
	if s.dropAck {
		writeErr(w, http.StatusServiceUnavailable, "unavailable", "acknowledgement lost")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, syncproto.ErrorResponse{Code: code, Message: msg})
}
