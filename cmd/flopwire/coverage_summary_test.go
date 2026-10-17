package main

import (
	"bytes"
	"encoding/json"
	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"strings"
	"testing"
)

func TestMCPCoverageArgument(t *testing.T) {
	for _, verb := range []string{"grep", "search", "sessions", "read"} {
		for _, value := range []any{"summary", "detail", 42, "", nil, "verbose"} {
			o, _, err := mcpOpts("flopwire_"+verb, map[string]any{"coverage": value})
			valid := value == "summary" || value == "detail"
			if (err == nil) != valid {
				t.Fatalf("%s %v: %v", verb, value, err)
			}
			if valid && o.vals["coverage"] != value {
				t.Fatal("coverage not retained")
			}
		}
	}
}
func TestMCPCoverageFormatsAndCLIParity(t *testing.T) {
	for _, verb := range []string{"grep", "search", "sessions", "read"} {
		for _, mode := range []string{"text", "json"} {
			r := &retriever{backend: &bigBackend{page: &format.Page{}, sessions: &format.Sessions{}, cx: &format.Context{}}, scope: &format.Scope{Kind: "shared", Server: "https://shared.example.test"}}
			args := map[string]any{"format": mode, "include_self": true}
			if verb == "read" {
				delete(args, "include_self")
			}
			switch verb {
			case "grep":
				args["pattern"] = "backoff"
			case "search":
				args["query"] = "backoff"
			case "read":
				args["address"] = "session/1"
			}
			out, err := mcpCall(t.Context(), r, "flopwire_"+verb, args)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, `"mode":"summary"`) || !strings.Contains(out, "other-device completeness") {
				t.Fatalf("%s/%s: %s", verb, mode, out)
			}
			args["coverage"] = "detail"
			out, err = mcpCall(t.Context(), r, "flopwire_"+verb, args)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, `"mode":"summary"`) {
				t.Fatalf("detail projected: %s", out)
			}
		}
	}
	report := coverage.UnknownReport("unavailable")
	scope := &format.Scope{Kind: "shared", Coverage: report}
	before, _ := json.Marshal(scope)
	detail := mcpCoverageScope(scope, true, 24000)
	after, _ := json.Marshal(detail)
	if !bytes.Equal(before, after) {
		t.Fatalf("detail changed CLI scope: %s vs %s", before, after)
	}
	compact := mcpCoverageScope(scope, false, 24000)
	if compact.Coverage != report || scope.CoverageMode != "" {
		t.Fatal("projection mutated original")
	}
}

func TestMCPCoverageProjectionBudgetAndDetailParity(t *testing.T) {
	report := coverage.UnknownReport("unavailable")
	report.Upload = &coverage.UploadSnapshot{Captured: &coverage.CapturedSnapshot{LostGenerations: 2, PendingManifestBytes: 999}}
	scope := &format.Scope{Kind: "shared", Server: "https://shared.example.test", Coverage: report}
	compact := mcpCoverageScope(scope, false, 24000)
	b, _ := json.Marshal(compact)
	if !bytes.Contains(b, []byte(`"capture_lost_generations":2`)) {
		t.Fatalf("lost fact omitted before compact measure: %s", b)
	}
	budget := mcpCoverageScope(scope, false, 100)
	b, _ = json.Marshal(budget)
	if !bytes.Contains(b, []byte(`"coverage":"output budget; observations omitted"`)) || bytes.Contains(b, []byte("observed_at")) {
		t.Fatalf("budget: %s", b)
	}
	detail := mcpCoverageScope(scope, true, 100)
	b, _ = json.Marshal(detail)
	if !bytes.Contains(b, []byte(`"pending_manifest_bytes":999`)) || bytes.Contains(b, []byte("observations omitted")) {
		t.Fatalf("detail: %s", b)
	}
	// Encoding selection has no effect on the source snapshot or CLI shape.
	if report.Upload.Captured.LostGenerations != 2 || scope.CoverageMode != "" {
		t.Fatal("mutated source scope")
	}
}
func TestMCPCoverageTinyBudgetFailsExplicitly(t *testing.T) {
	for _, mode := range []string{"text", "json"} {
		o := newOpts("sessions")
		o.on[mode] = true
		o.on["include-self"] = true
		r := &retriever{backend: &bigBackend{sessions: &format.Sessions{}}, scope: &format.Scope{Kind: "shared"}}
		var b bytes.Buffer
		err := runTool(t.Context(), r, o, &b, format.Style{MCP: true, Budget: 100}, selfMCP)
		if err == nil || b.Len() != 0 {
			t.Fatalf("%s silently exceeded budget: %v %s", mode, err, b.String())
		}
	}
}
