package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestSyncFailureLoggingPreservesResponse(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, tc := range []struct {
		name           string
		ctx            context.Context
		err            error
		level, message string
	}{
		{"request canceled", canceled, context.Canceled, "DEBUG", "sync request canceled"},
		{"wrapped request cancellation", canceled, fmt.Errorf("flush: %w", context.Canceled), "DEBUG", "sync request canceled"},
		{"operation canceled with live request", context.Background(), context.Canceled, "ERROR", "sync request failed"},
		{"deadline exceeded", expired, context.DeadlineExceeded, "ERROR", "sync request failed"},
		{"canceled operation after deadline", expired, context.Canceled, "ERROR", "sync request failed"},
		{"ordinary failure", context.Background(), errors.New("storage failed"), "ERROR", "sync request failed"},
		{"ordinary failure after cancellation", canceled, errors.New("storage failed"), "ERROR", "sync request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Server{Log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			w := httptest.NewRecorder()
			s.fail(tc.ctx, w, tc.err)
			var entry struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Level != tc.level || entry.Message != tc.message {
				t.Fatalf("log=%+v", entry)
			}
			var response syncproto.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "5" || response.Code != "unavailable" || response.Message != "sync temporarily unavailable" {
				t.Fatalf("status=%d retry=%q response=%+v", w.Code, w.Header().Get("Retry-After"), response)
			}
		})
	}
}
