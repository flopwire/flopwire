package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestReadReferenceUsesAuthorizedFocusAndAddressedLine(t *testing.T) {
	for _, tc := range []struct {
		name, ref, text, want string
		from, status          int
	}{
		{"whole", "abcdef12/7", "one\n two", "user: one two", 1, 200},
		{"line", "abcdef12/7:2", "second fact\nnot requested", "user: second fact", 2, 200},
		{"out-of-range", "abcdef12/7:99", "last line", "", 2, 200},
		{"forbidden", "abcdef12/7", "", "", 0, 403},
		{"missing", "abcdef12/7", "", "", 0, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/read" || r.Header.Get("Authorization") != "Bearer receiving-device" {
					t.Errorf("wrong read authority: %s", r.URL.Path)
					w.WriteHeader(403)
					return
				}
				q := r.URL.Query()
				if q.Get("address") != tc.ref || q.Get("max_chars") != "640" || q.Get("before") != "" || q.Get("after") != "" {
					t.Errorf("unbounded or wrong read: %v", q)
				}
				w.WriteHeader(tc.status)
				if tc.status != 200 {
					return
				}
				json.NewEncoder(w).Encode(format.Context{Focus: "focus", Messages: []format.Message{
					{ID: "neighbor", Text: "DO NOT EXCERPT NEIGHBOR"},
					{ID: "focus", Role: "user", Text: tc.text, LineFrom: tc.from},
				}})
			}))
			defer srv.Close()
			got, err := (Bus{Server: srv.URL, Token: "receiving-device", HTTP: srv.Client()}).ReadReference(context.Background(), tc.ref)
			if (err != nil) != (tc.status != 200) || got != tc.want {
				t.Fatalf("got %q, %v; want %q, status %d", got, err, tc.want, tc.status)
			}
		})
	}
}
