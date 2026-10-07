package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

func TestCoverageFastRetrievalDoesNotWait(t *testing.T) {
	done := make(chan struct{})
	r := &retriever{coverageLocal: func(ctx context.Context) (*coverage.Report, error) { <-ctx.Done(); close(done); return nil, ctx.Err() }}
	p := r.startCoverage(context.Background())
	defer p.cancel()
	before := time.Now()
	scope := p.snapshot(&format.Scope{Kind: "shared"})
	if time.Since(before) > 100*time.Millisecond {
		t.Fatal("snapshot waited for diagnostics")
	}
	if scope.Coverage.Unknown["collection"] == "" || scope.Coverage.Unknown["parse"] == "" {
		t.Fatal("missing observations must be unknown")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe did not stop")
	}
}
func TestCoverageLocalIgnoresUploadAndDoesNotMutateReport(t *testing.T) {
	original := &coverage.Report{Upload: &coverage.UploadSnapshot{QueuedSourceChecks: 12}, Parse: &coverage.ParseSnapshot{Pending: 9}, Unknown: map[string]string{"upload": "busy"}}
	ch := make(chan *coverage.Report, 1)
	ch <- original
	p := coveragePending{local: ch, cancel: func() {}}
	scope := p.snapshot(&format.Scope{Kind: "local"})
	if scope.Coverage.Upload != nil || scope.Coverage.Parse != nil {
		t.Fatal("local availability includes shared facts")
	}
	if original.Upload == nil || original.Unknown["upload"] != "busy" {
		t.Fatal("mutated shared report")
	}
	note := coverageNote(scope.Coverage, false)
	if strings.Contains(note, "upload") || !strings.Contains(note, "discovery unknown") {
		t.Fatalf("note: %s", note)
	}
}
func TestCoverageBindingAndReadyFacts(t *testing.T) {
	cfg := &client.Config{DeviceID: "device-a", Server: "https://example.com"}
	for _, device := range []string{"device-a", "other"} {
		ch := make(chan *coverage.Report, 1)
		ch <- &coverage.Report{DeviceID: device, Server: cfg.Server, Collection: &coverage.CollectionSnapshot{IndexedSources: 7}}
		p := coveragePending{local: ch, cfg: cfg, cancel: func() {}}
		got := p.snapshot(&format.Scope{Kind: "shared"})
		if (got.Coverage.Collection != nil) != (device == cfg.DeviceID) {
			t.Fatal("incorrect identity binding")
		}
		if got.Coverage.Unknown["other_devices"] == "" {
			t.Fatal("missing other-device limitation")
		}
	}
}
func TestParseCoverageRequiresEveryCounter(t *testing.T) {
	fields := map[string]any{"device_id": "device-a", "observed_at": time.Now().UTC(), "pending": int64(0), "failing": int64(0), "quarantined": int64(0), "untracked_sources": int64(0), "oldest_pending": nil}
	for _, missing := range []string{"", "pending", "failing", "quarantined", "untracked_sources", "observed_at", "device_id", "oldest_pending"} {
		body := map[string]any{}
		for k, v := range fields {
			if k != missing {
				body[k] = v
			}
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != coverage.Path {
				t.Error(r.URL.Path)
			}
			json.NewEncoder(w).Encode(body)
		}))
		cfg := client.Config{Server: server.URL, DeviceID: "device-a"}
		got, err := parseCoverageReader(cfg)(context.Background())
		server.Close()
		if missing == "" {
			if err != nil || got == nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatalf("missing %s treated as known zero", missing)
		}
	}
}
func TestCoverageProbeFailureAndLostPresentation(t *testing.T) {
	r := &retriever{coverageLocal: func(context.Context) (*coverage.Report, error) { return nil, errors.New("private diagnostic text") }}
	p := r.startCoverage(context.Background())
	defer p.cancel()
	s := p.snapshot(&format.Scope{Kind: "shared"})
	b, _ := json.Marshal(s)
	if bytes.Contains(b, []byte("private diagnostic")) {
		t.Fatal("leaked diagnostic error")
	}
	report := &coverage.Report{Upload: &coverage.UploadSnapshot{Captured: &coverage.CapturedSnapshot{LostGenerations: 3}}, Parse: &coverage.ParseSnapshot{UntrackedSources: 2}}
	note := coverageNote(report, true)
	for _, want := range []string{"lost 3", "untracked 2", "other devices unknown"} {
		if !strings.Contains(note, want) {
			t.Fatalf("missing %s: %s", want, note)
		}
	}
	if strings.Contains(note, "complete") || strings.Contains(note, "caught up") {
		t.Fatal("invented completeness")
	}
}

func TestCoverageStatusPhasesAndJSONBudget(t *testing.T) {
	report := coverage.UnknownReport("diagnostic unavailable")
	report.Collection = &coverage.CollectionSnapshot{IndexedSources: 4}
	report.Upload = &coverage.UploadSnapshot{FailingSources: 2, BlockingReason: "spool_capacity", Captured: &coverage.CapturedSnapshot{LostGenerations: 1}}
	report.Parse = &coverage.ParseSnapshot{Pending: 3, Failing: 1, UntrackedSources: 8}
	var out bytes.Buffer
	printCoverage(&out, report)
	for _, want := range []string{"collection:", "upload:", "policy:", "server parse:", "1 lost generations", "spool_capacity", "8 untracked"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s: %s", want, out.String())
		}
	}
	page := &format.Page{Scope: &format.Scope{Kind: "shared", Coverage: report}}
	b, err := json.Marshal(boundPage("search", page, "", 24000, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 24000 || !bytes.Contains(b, []byte(`"coverage"`)) || !bytes.Contains(b, []byte(`"lost_generations":1`)) {
		t.Fatalf("budget/snapshot: %s", b)
	}
}

func TestAgentCoverageMissingAdditiveCounterIsUnknown(t *testing.T) {
	raw := json.RawMessage(`{"observed_at":"2026-10-07T00:00:00Z","upload":{"queued_source_checks":0,"active_source_turns":0,"failing_sources":0,"captured":{"pending_generations":0,"pending_manifest_entries":0,"pending_manifest_bytes":0,"pending_tail_bytes":0,"lost_generations":0}}}`)
	report, err := decodeAgentCoverage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if report.Upload == nil || report.Upload.Captured != nil || report.Unknown["captured_upload"] == "" {
		t.Fatalf("omitted truncated counter became known zero: %+v", report)
	}
}

func TestParseCoverageRejectsOversizeAndTrailingJSON(t *testing.T) {
	for _, body := range []string{strings.Repeat(" ", 16*1024+1), `{} {}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		_, err := parseCoverageReader(client.Config{Server: server.URL, DeviceID: "device-a"})(context.Background())
		server.Close()
		if err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}

// Existing oracle goldens check retrieval rows; live optional observations have
// their own tests. Remove only the adapter's first scope metadata, preserving
// its kind/server and all result bodies and pagination.
func withoutCoverageObservation(out string) string {
	if strings.HasPrefix(out, "[scope:") {
		first, rest, ok := strings.Cut(out, "\n")
		if !ok {
			return out
		}
		if i := strings.Index(first, "] [coverage:"); i >= 0 {
			first = first[:i+1]
		}
		return first + "\n" + rest
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		return out
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(out), &envelope) != nil {
		return out
	}
	scope, ok := envelope["scope"]
	if !ok {
		return out
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(scope, &fields) != nil || fields["coverage"] == nil {
		return out
	}
	// Restrict the replacement to the scope object, never transcript contents.
	start := strings.Index(out, string(scope))
	if start < 0 {
		return out
	}
	current := string(scope)
	key := strings.Index(current, `"coverage"`)
	if key < 0 {
		return out
	}
	colon := strings.Index(current[key:], ":") + key
	decoder := json.NewDecoder(strings.NewReader(current[colon+1:]))
	var value json.RawMessage
	if decoder.Decode(&value) != nil {
		return out
	}
	end := colon + 1 + int(decoder.InputOffset())
	begin := key
	for begin > 0 && (current[begin-1] == ' ' || current[begin-1] == '\n' || current[begin-1] == '\t') {
		begin--
	}
	if begin > 0 && current[begin-1] == ',' {
		begin--
	} else {
		return out
	}
	current = current[:begin] + current[end:]
	return out[:start] + current + out[start+len(scope):]
}

func TestCoverageMCPEmptyResultCarriesUnknownWithoutWaiting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(format.Page{Hits: []format.Hit{}, Exact: true})
	}))
	defer server.Close()
	r := &retriever{backend: client.HTTP{Server: server.URL}, scope: &format.Scope{Kind: "shared", Server: server.URL}, coverageLocal: func(ctx context.Context) (*coverage.Report, error) { <-ctx.Done(); return nil, ctx.Err() }}
	before := time.Now()
	text, err := mcpCall(context.Background(), r, "flopwire_search", map[string]any{"query": "no results", "format": "json", "include_self": true})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(before) > 200*time.Millisecond {
		t.Fatal("successful empty query waited for diagnostic")
	}
	var result struct {
		Scope *format.Scope `json:"scope"`
	}
	if err = json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	if result.Scope == nil || result.Scope.Coverage == nil || result.Scope.Coverage.Unknown["parse"] == "" {
		t.Fatalf("missing unknown metadata: %s", text)
	}
}

func TestCoverageSmallAndMCPOutputBudgets(t *testing.T) {
	empty := &retriever{backend: &bigBackend{sessions: &format.Sessions{Sessions: []format.ConversationInfo{}}}, scope: &format.Scope{Kind: "shared", Server: "https://example.test"}}
	o, err := parseArgs("sessions", []string{"--include-self"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runTool(context.Background(), empty, o, &out, format.Style{Budget: 512}, selfCLI); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 512 || !strings.Contains(out.String(), "output budget; observations omitted") {
		t.Fatalf("small budget: %d %s", out.Len(), out.String())
	}
	page := &format.Page{Exact: true, Total: 40, TotalSessions: 40}
	for i := 0; i < 40; i++ {
		page.Hits = append(page.Hits, format.Hit{Address: fmt.Sprintf("session-%d/1", i), SessionID: fmt.Sprintf("session-%d", i), Lines: []format.Line{{N: 1, Text: strings.Repeat("x", 1000), Match: true}}})
	}
	r := &retriever{backend: &bigBackend{page: page}, scope: &format.Scope{Kind: "shared", Server: "https://example.test"}}
	text, err := mcpCall(context.Background(), r, "flopwire_search", map[string]any{"query": "x", "format": "json", "include_self": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > format.MaxOutput || !strings.Contains(text, `"coverage"`) || !strings.Contains(text, `"next_offset"`) {
		t.Fatalf("MCP bounded output %d", len(text))
	}
}

func TestAgentCoverageSocketCancellationClosesRead(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "fw-coverage-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		var request map[string]any
		json.NewDecoder(conn).Decode(&request)
		close(accepted)
		io.Copy(io.Discard, conn)
		close(closed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, e := agentCoverageAt(ctx, socket); finished <- e }()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("request not accepted")
	}
	cancel()
	select {
	case e := <-finished:
		if e == nil {
			t.Fatal("canceled read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("socket read leaked")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("server connection leaked")
	}
}

func TestAgentCoverageMalformedStageCountersAreUnknown(t *testing.T) {
	cases := []struct {
		stage   string
		body    string
		unknown string
	}{
		{"parse", `{"parse":{"pending":-1}}`, "parse"},
		{"collection", `{"collection":{}}`, "collection"},
		{"collection", `{"collection":{"indexed_sources":null}}`, "collection"},
		{"collection", `{"collection":{"indexed_sources":-1}}`, "collection"},
		{"cowork", `{"policy":{"cowork":{"shared_held":0,"schedule_eligible":0}}}`, "cowork_policy"},
		{"cowork", `{"policy":{"cowork":{"shared_held":null,"schedule_eligible":0,"historical_unknown":0}}}`, "cowork_policy"},
		{"cowork", `{"policy":{"cowork":{"shared_held":0,"schedule_eligible":-1,"historical_unknown":0}}}`, "cowork_policy"},
		{"cowork", `{"policy":{"cowork":{"shared_held":0,"schedule_eligible":0,"historical_unknown":0,"hold_reasons":{"held":-1}}}}`, "cowork_policy"},
		{"upload", `{"upload":{"queued_source_checks":0,"active_source_turns":-1,"failing_sources":0}}`, "upload"},
		{"captured", `{"upload":{"queued_source_checks":0,"active_source_turns":0,"failing_sources":0,"captured":{"pending_generations":0,"pending_manifest_entries":0,"pending_manifest_bytes":0,"pending_tail_bytes":-1,"lost_generations":0,"truncated_generations":0}}}`, "captured_upload"},
		{"historical", `{"policy":{"historical_mapping_unknown":-1}}`, "historical_mapping"},
		{"copies", `{"policy":{"server_copies_retained":-1}}`, "server_copies"},
	}
	for _, tc := range cases {
		t.Run(tc.stage+tc.body, func(t *testing.T) {
			var fields map[string]json.RawMessage
			json.Unmarshal([]byte(tc.body), &fields)
			fields["observed_at"] = json.RawMessage(`"2026-10-07T00:00:00Z"`)
			raw, _ := json.Marshal(fields)
			report, err := decodeAgentCoverage(raw)
			if err != nil {
				t.Fatal(err)
			}
			if report.Unknown[tc.unknown] == "" {
				t.Fatalf("malformed count remained known: %+v", report)
			}
			switch tc.stage {
			case "parse":
				if report.Parse != nil {
					t.Fatal("local report retained server parse claim")
				}
			case "collection":
				if report.Collection != nil {
					t.Fatal("collection retained")
				}
			case "cowork":
				if report.Policy.Cowork != nil {
					t.Fatal("Cowork retained")
				}
			case "upload":
				if report.Upload != nil {
					t.Fatal("upload retained")
				}
			case "captured":
				if report.Upload.Captured != nil {
					t.Fatal("capture retained")
				}
			case "historical":
				if report.Policy.HistoricalMappingUnknown != nil {
					t.Fatal("historical retained")
				}
			case "copies":
				if report.Policy.ServerCopiesRetained != nil {
					t.Fatal("copies retained")
				}
			}
		})
	}
}

func TestCoverageSharedTextHeaderYieldsToSmallBudget(t *testing.T) {
	r := &retriever{backend: &bigBackend{sessions: &format.Sessions{Sessions: []format.ConversationInfo{}}}, scope: &format.Scope{Kind: "shared", Server: "https://example.test"}}
	o, err := parseArgs("sessions", []string{"--text", "--include-self"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runTool(context.Background(), r, o, &out, format.Style{Budget: 100}, selfCLI); err != nil {
		t.Fatal(err)
	}
	header, _, _ := strings.Cut(out.String(), "\n")
	if len(header)+1 > 100 || !strings.Contains(header, "[scope: shared server https://example.test]") {
		t.Fatalf("optional header exceeded budget: %s", header)
	}
	if strings.Contains(header, "parse unknown") {
		t.Fatal("full phase expansion consumed small text budget")
	}
}
func TestCoverageLocalUnknownKeysAllowlist(t *testing.T) {
	ch := make(chan *coverage.Report, 1)
	ch <- &coverage.Report{ObservedAt: time.Now(), Unknown: map[string]string{"collection": "busy", "server": "old server", "upload_blocking": "busy", "historical_mapping": "busy", "cowork_policy": "held"}}
	p := coveragePending{local: ch, cancel: func() {}}
	report := p.snapshot(&format.Scope{Kind: "local"}).Coverage
	for key := range report.Unknown {
		if key != "collection" && key != "discovery" {
			t.Fatalf("local query carries shared diagnostic %s", key)
		}
	}
	compact := budgetCoverageScope(&format.Scope{Kind: "local", Coverage: coverage.UnknownReport(strings.Repeat("x", 600))}, 512)
	if compact.Coverage.Unknown["other_devices"] != "" {
		t.Fatal("local compact metadata acquired shared device scope")
	}
}
func TestCoverageCapturedAndBlockingUnknownNotes(t *testing.T) {
	report := &coverage.Report{Upload: &coverage.UploadSnapshot{}, Unknown: map[string]string{"upload_blocking": "busy"}}
	note := coverageNote(report, true)
	if !strings.Contains(note, "captured unknown") || !strings.Contains(note, "blocking unknown") {
		t.Fatalf("missing unknown phase: %s", note)
	}
	var out bytes.Buffer
	printCoverage(&out, report)
	if !strings.Contains(out.String(), "blocking: unknown") {
		t.Fatalf("status omitted unknown blocker: %s", out.String())
	}
}
func TestAgentStatusEnvelopePartialCoverageDoesNotFail(t *testing.T) {
	var response agent.Response
	raw := `{"ok":true,"coverage":{"observed_at":"2026-10-07T00:00:00Z","collection":{"indexed_sources":null},"policy":{"cowork":{"shared_held":0}}}}`
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Coverage == nil || response.Coverage.Collection != nil || response.Coverage.Policy.Cowork != nil {
		t.Fatalf("partial status became facts: %+v", response)
	}
	if response.Coverage.Unknown["collection"] == "" || response.Coverage.Unknown["cowork_policy"] == "" {
		t.Fatal("status missing unknown markers")
	}
	var page format.Page
	if err := json.Unmarshal([]byte(`{"hits":[{"address":"s/1"}],"scope":{"kind":"shared","coverage":{"observed_at":"bad","collection":"unsupported"}}}`), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 || page.Hits[0].Address != "s/1" || page.Scope.Coverage.Unknown["collection"] == "" {
		t.Fatal("optional metadata damaged query results")
	}
}

func TestAgentStatusCoverageRequiresFreshServerProbe(t *testing.T) {
	dir := shortSockDir(t)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	ln, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		var request map[string]any
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			done <- err
			return
		}
		now := time.Now()
		report := &coverage.Report{DeviceID: "collector-device", Server: "https://server.test", ObservedAt: now, Parse: &coverage.ParseSnapshot{DeviceID: "different-device", ObservedAt: now, Pending: 3}}
		done <- json.NewEncoder(conn).Encode(agent.Response{OK: true, Coverage: report})
	}()
	var output bytes.Buffer
	if err := agentStatusOutput(context.Background(), &output, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var response agent.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Coverage == nil || response.Coverage.Parse != nil || response.Coverage.Unknown["parse"] == "" {
		t.Fatalf("unattested local parse observation survived: %s", output.Bytes())
	}
}
