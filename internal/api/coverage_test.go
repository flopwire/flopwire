package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/api"
	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/ingest"
	"github.com/flopwire/flopwire/internal/pgtest"
	"github.com/flopwire/flopwire/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type coverageBackend struct {
	api.ParseStatus
	read func(context.Context, string) (coverage.ParseSnapshot, error)
}

func (b *coverageBackend) DeviceParseCoverage(ctx context.Context, id string) (coverage.ParseSnapshot, error) {
	return b.read(ctx, id)
}

func coverageDevice(t *testing.T, url, session string) (string, string) {
	t.Helper()
	d := postJSON(t, url+"/v1/devices", map[string]string{"name": "coverage-test", "platform": "test"}, bearer(session))
	return d["token"].(string), d["device"].(map[string]any)["id"].(string)
}

func TestDeviceParseCoverageAuthorizationAndFailures(t *testing.T) {
	memory := store.NewMemory()
	admin := seedAdmin(t, memory)
	calls := 0
	expectedID := ""
	backend := &coverageBackend{read: func(ctx context.Context, id string) (coverage.ParseSnapshot, error) {
		calls++
		if id != expectedID {
			t.Errorf("scope %q != authenticated %q", id, expectedID)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("missing bounded query context")
		}
		return coverage.ParseSnapshot{ObservedAt: time.Now().UTC()}, nil
	}}
	srv := newServer(t, memory, api.Config{Parse: backend})
	token, id := coverageDevice(t, srv.URL, admin)
	expectedID = id
	_, up := mint(t, srv.URL, token, map[string]any{"scopes": []string{"upload"}, "label": "coverage-upload"})
	for _, tc := range []struct {
		token, path string
		code        int
	}{
		{"", coverage.Path, 401}, {admin, coverage.Path, 403}, {up["token"].(string), coverage.Path, 403},
		{token, coverage.Path + "?device_id=someone-else", 400},
	} {
		res := request(t, http.MethodGet, srv.URL+tc.path, nil, bearer(tc.token))
		body := read(res)
		if res.StatusCode != tc.code {
			t.Fatalf("status %d want %d: %s", res.StatusCode, tc.code, body)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized request reached snapshot backend")
	}
	res := request(t, http.MethodGet, srv.URL+coverage.Path, nil, bearer(token))
	body := read(res)
	if res.StatusCode != 200 {
		t.Fatal(body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pending", "failing", "quarantined"} {
		if out[key] != float64(0) {
			t.Fatalf("known zero %s missing: %s", key, body)
		}
	}
	if out["device_id"] != id || out["oldest_pending"] != nil || out["complete"] != nil {
		t.Fatal(body)
	}
	backend.read = func(context.Context, string) (coverage.ParseSnapshot, error) {
		return coverage.ParseSnapshot{}, errors.New("secret transcript /private/path")
	}
	res = request(t, http.MethodGet, srv.URL+coverage.Path, nil, bearer(token))
	body = read(res)
	if res.StatusCode != 503 || strings.Contains(body, "private") || strings.Contains(body, "pending") {
		t.Fatalf("unavailable response: %s", body)
	}
	unsupported := newServer(t, memory, api.Config{})
	res = request(t, http.MethodGet, unsupported.URL+coverage.Path, nil, bearer(token))
	body = read(res)
	if res.StatusCode != 501 {
		t.Fatal(body)
	}
	auditFail := newServer(t, &actionFailStore{Store: memory, action: "coverage.read"}, api.Config{Parse: &coverageBackend{read: func(context.Context, string) (coverage.ParseSnapshot, error) {
		return coverage.ParseSnapshot{ObservedAt: time.Now()}, nil
	}}})
	res = request(t, http.MethodGet, auditFail.URL+coverage.Path, nil, bearer(token))
	body = read(res)
	if res.StatusCode != 503 || strings.Contains(body, "pending") {
		t.Fatalf("audit must fail closed: %s", body)
	}
}

func TestDeviceParseCoveragePostgresSnapshot(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, pgtest.NewDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	s := store.NewPostgres(pool, nil, "")
	admin := seedAdmin(t, s)
	q := &ingest.Queue{Pool: pool}
	srv := newServer(t, s, api.Config{Parse: q})
	token, id := coverageDevice(t, srv.URL, admin)
	_, other := coverageDevice(t, srv.URL, admin)
	oldest := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	for i, tc := range []struct {
		device                      string
		requested, parsed, attempts int
		quarantine                  bool
	}{
		{id, 2, 1, 0, false}, {id, 3, 1, 2, false}, {id, 3, 1, 12, true}, {id, 3, 3, 12, true}, {id, 3, 3, 2, false}, {other, 4, 0, 3, false},
	} {
		source := uuid.NewString()
		_, err = pool.Exec(ctx, `INSERT INTO sources(id,device_id,agent,path,file_id,storage_kind,parser,first_seen_at) VALUES($1,$2,'claude',$3,$1,'jsonl_append','claude',now())`, source, tc.device, "/private/transcript/"+source)
		if err != nil {
			t.Fatal(err)
		}
		var quarantine *time.Time
		if tc.quarantine {
			quarantine = &oldest
		}
		at := oldest.Add(time.Duration(i) * time.Minute)
		if tc.device == other {
			at = oldest.Add(-time.Hour)
		}
		_, err = pool.Exec(ctx, `INSERT INTO source_parse_state(source_id,requested_seq,parsed_seq,attempts,quarantined_at,requested_at,last_error) VALUES($1,$2,$3,$4,$5,$6,'private transcript error')`, source, tc.requested, tc.parsed, tc.attempts, quarantine, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	res := request(t, http.MethodGet, srv.URL+coverage.Path, nil, bearer(token))
	body := read(res)
	if res.StatusCode != 200 || strings.Contains(body, "private") {
		t.Fatal(body)
	}
	var out coverage.ParseSnapshot
	if err = json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.DeviceID != id || out.Pending != 2 || out.Failing != 1 || out.Quarantined != 2 || out.OldestPending == nil || !out.OldestPending.Equal(oldest) || out.ObservedAt.IsZero() {
		t.Fatalf("snapshot %+v", out)
	}
	_, err = pool.Exec(ctx, `UPDATE source_parse_state SET parsed_seq=requested_seq,quarantined_at=NULL WHERE source_id IN(SELECT id FROM sources WHERE device_id=$1)`, id)
	if err != nil {
		t.Fatal(err)
	}
	out, err = q.DeviceParseCoverage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out.Pending != 0 || out.Failing != 0 || out.Quarantined != 0 || out.OldestPending != nil {
		t.Fatalf("zero snapshot %+v", out)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = q.DeviceParseCoverage(cancelled, id); err == nil {
		t.Fatal("canceled snapshot reported known counters")
	}
}

// The read audit shares the snapshot deadline, so a successful SQL query cannot
// leave this diagnostic waiting indefinitely for its required audit.
type coverageAuditContextStore struct {
	store.Store
	t *testing.T
}

func (s *coverageAuditContextStore) AppendAudit(ctx context.Context, event domain.AuditEvent) error {
	if event.Action == "coverage.read" {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			s.t.Error("coverage audit has no bounded context")
		}
		return context.DeadlineExceeded
	}
	return s.Store.AppendAudit(ctx, event)
}
func TestDeviceParseCoverageAuditSharesBudget(t *testing.T) {
	memory := store.NewMemory()
	admin := seedAdmin(t, memory)
	backend := &coverageBackend{read: func(context.Context, string) (coverage.ParseSnapshot, error) {
		return coverage.ParseSnapshot{ObservedAt: time.Now().UTC()}, nil
	}}
	srv := newServer(t, &coverageAuditContextStore{Store: memory, t: t}, api.Config{Parse: backend})
	token, _ := coverageDevice(t, srv.URL, admin)
	res := request(t, http.MethodGet, srv.URL+coverage.Path, nil, bearer(token))
	body := read(res)
	if res.StatusCode != 503 || strings.Contains(body, "pending") {
		t.Fatalf("failed audit released counters: %s", body)
	}
}
