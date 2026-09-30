package ingest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

// MaxFlushBytes bounds one flush request body. Devices send at most
// MaxRequestBytes (default max(4MB, the chunker's max chunk)) of chunk
// bodies plus one chunk of tail; the bound leaves room for devices
// configured with a larger cap.
const MaxFlushBytes = 80 << 20

// A sync request's read deadline is syncReadBase plus its declared body
// length at syncMinRate, and its write deadline that plus syncWriteBase:
// the server's fixed ReadTimeout (30s) alone needs about 1.1Mbit/s for a
// 4MB request, so a device on a slower link that the client still accepts
// (client.MinRate, 64KB/s) failed every retry of the same request (S9).
var (
	syncReadBase  = 30 * time.Second
	syncWriteBase = 60 * time.Second
)

var syncMinRate int64 = 64 << 10 // bytes per second; client.MinRate

// syncDeadlineBytes caps the length the read deadline scales with: what a
// device really sends in one request, its default MaxRequestBytes (4MB of
// chunk bodies, the tail included) plus room for the frame header and
// framing. A larger declared length gets no more time (about 110s in
// all), so a device cannot hold a request, and the Postgres connection
// and chunk locks a flush takes, for longer by declaring a large body.
const syncDeadlineBytes = 4<<20 + 1<<20

// ServeSync answers POST /v1/sync/has and /v1/sync/flush for an
// authenticated device. Errors use the syncproto ErrorResponse shape.
func (s *Server) ServeSync(w http.ResponseWriter, r *http.Request, deviceID string) {
	if r.Header.Get(syncproto.HeaderVersion) != strconv.Itoa(syncproto.Version) {
		writeErr(w, &Error{http.StatusBadRequest, "unsupported_version", "want sync version 1"})
		return
	}
	if n := r.ContentLength; n > 0 {
		read := syncReadBase + time.Duration(min(n, syncDeadlineBytes))*time.Second/time.Duration(syncMinRate)
		rc := http.NewResponseController(w)
		now := time.Now()
		_ = rc.SetReadDeadline(now.Add(read))
		_ = rc.SetWriteDeadline(now.Add(read + syncWriteBase))
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFlushBytes)
	switch r.URL.Path {
	case syncproto.PathHas:
		var req syncproto.HasRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, badRequest("bad has request"))
			return
		}
		missing, err := s.Has(r.Context(), deviceID, req.Hashes)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, syncproto.HasResponse{Version: syncproto.Version, Missing: missing})
	case syncproto.PathFlush:
		if !strings.HasPrefix(r.Header.Get("Content-Type"), syncproto.FlushContentType) {
			writeErr(w, badRequest("content type"))
			return
		}
		// One flush per device at a time: a flush holds a pool connection
		// and chunk locks while its body arrives, and a device's agent
		// sends one at a time anyway. A second one is told to retry.
		if _, busy := s.flushing.LoadOrStore(deviceID, struct{}{}); busy {
			writeErr(w, &Error{http.StatusTooManyRequests, "flush_in_progress", "another flush of this device is in progress; retry later"})
			return
		}
		defer s.flushing.Delete(deviceID)
		h, pr, err := syncproto.DecodeFlush(r.Body)
		if err != nil {
			code := "bad_request"
			if strings.Contains(err.Error(), "unsupported version") {
				code = "unsupported_version"
			}
			writeErr(w, &Error{http.StatusBadRequest, code, err.Error()})
			return
		}
		resp, err := s.Flush(r.Context(), deviceID, h, pr)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	default:
		writeErr(w, &Error{http.StatusNotFound, "not_found", r.URL.Path})
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var e *Error
	if errors.As(err, &e) {
		writeErr(w, e)
		return
	}
	if s.Log != nil {
		s.Log.Error("sync request failed", "error", err)
	}
	writeErr(w, &Error{http.StatusServiceUnavailable, "unavailable", "sync temporarily unavailable"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *Error) {
	if e.Status == http.StatusServiceUnavailable || e.Status == http.StatusInsufficientStorage || e.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "5")
	}
	writeJSON(w, e.Status, syncproto.ErrorResponse{Code: e.Code, Message: e.Msg})
}
