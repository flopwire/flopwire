package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/busproto"
)

func TestMessagesTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != busproto.PathMessages || r.Header.Get("Authorization") != "Bearer device" {
			t.Errorf("wrong request %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		for k, v := range map[string]string{"repo": "/src/api", "repo_root": "/src/api", "repo_remote": "host/team/api", "repo_checkout": "/src", "session": "other-session", "user": "other@example.test", "thread": "canonical", "since": "2026-01-01T00:00:00Z", "until": "2027-01-01T00:00:00Z", "cursor": "time|id", "limit": "7", "max_bytes": "9000"} {
			if q.Get(k) != v {
				t.Errorf("%s: %q", k, q.Get(k))
			}
		}
		_, _ = w.Write([]byte(`{"kind":"messages","scope":"accepted_team_repo","more":false}`))
	}))
	defer srv.Close()
	out, err := (Bus{Server: srv.URL, Token: "device"}).Messages(context.Background(), busproto.MessagesQuery{Repo: "/src/api", Roots: []string{"/src/api"}, Remotes: []string{"host/team/api"}, Mains: []string{"/src"}, User: "other@example.test", Session: "other-session", Thread: "canonical", Since: "2026-01-01T00:00:00Z", Until: "2027-01-01T00:00:00Z", Before: "time|id", Limit: 7, MaxBytes: 9000})
	if err != nil || out.Scope != "accepted_team_repo" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestMessagesRotationRaceAndSentCredentialSanitization(t *testing.T) {
	for _, leak := range []string{"", "detail", "code"} {
		t.Run(fmt.Sprint(leak), func(t *testing.T) {
			var cfg Config
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != busproto.PathMessages {
					t.Errorf("not message GET: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") == "Bearer old" {
					next := cfg
					next.Token = "rotated-secret"
					if err := Save(next); err != nil {
						t.Error(err)
					}
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer rotated-secret" {
					t.Error("rotation not followed")
				}
				if leak != "" {
					w.WriteHeader(400)
					if leak == "code" {
						fmt.Fprint(w, `{"type":"about:blank","status":400,"code":"rotated-secret","detail":"bad request"}`)
					} else {
						fmt.Fprint(w, `{"type":"about:blank","status":400,"code":"bad_request","detail":"rotated-secret"}`)
					}
					return
				}
				fmt.Fprint(w, `{"kind":"messages"}`)
			}))
			defer srv.Close()
			cfg = retrievalConfig(t, srv.URL)
			api := cfg.RetrievalAPI()
			out, err := (Bus{Server: api.Server, Token: api.Token, HTTP: api.Client}).Messages(t.Context(), busproto.MessagesQuery{})
			if calls != 2 {
				t.Fatalf("rotation retries: %d", calls)
			}
			if leak != "" {
				var ae *APIError
				if !errors.As(err, &ae) || strings.Contains(err.Error(), "rotated-secret") {
					t.Fatalf("sent credential leaked: %v", err)
				}
			} else if err != nil || out.Kind != "messages" {
				t.Fatalf("rotated read: %+v %v", out, err)
			}
		})
	}
}
