package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// Exercise the real transport deadlines, including HTTP/2's stream reset
// when WriteTimeout expires. Scale all durations so this regression does
// not require a minute per run. The handler consumes the maximum query
// budget and then spends time completing and encoding the partial answer.
func TestServerWriteDeadlineAllowsIncompleteRetrieval(t *testing.T) {
	for _, http2 := range []bool{false, true} {
		name := "HTTP1"
		if http2 {
			name = "HTTP2"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const scale = 100
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), api.MaxBudget/scale)
				defer cancel()
				<-ctx.Done()
				time.Sleep(10 * time.Second / scale)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(format.Page{Truncated: true, Reason: "query budget expired"})
			}))
			server.EnableHTTP2 = http2
			server.Config.WriteTimeout = serverWriteTimeout / scale
			server.StartTLS()
			defer server.Close()
			client := server.Client()
			client.Timeout = 5 * time.Second
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatalf("maximum-budget retrieval lost its response: %v", err)
			}
			defer resp.Body.Close()
			wantProto := 1
			if http2 {
				wantProto = 2
			}
			if resp.ProtoMajor != wantProto {
				t.Fatalf("transport = %s, want HTTP/%d", resp.Proto, wantProto)
			}
			var page format.Page
			if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
				t.Fatalf("decode incomplete page: %v", err)
			}
			if resp.StatusCode != http.StatusOK || !page.Truncated || page.Reason != "query budget expired" {
				t.Fatalf("incomplete retrieval response: status=%d page=%+v", resp.StatusCode, page)
			}
		})
	}
}
