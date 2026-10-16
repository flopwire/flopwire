package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

// This opt-in fixture emitter uses native renderers for paid model interpretation
// qualification. Ordinary unit tests already cover diagnostic collection/binding.
func TestEmitCoverageQualification(t *testing.T) {
	path := os.Getenv("FLOPWIRE_COVERAGE_QUALIFICATION")
	if path == "" {
		t.Skip("set FLOPWIRE_COVERAGE_QUALIFICATION to emit synthetic model fixtures")
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ready := func() *coverage.Report {
		return &coverage.Report{
			ObservedAt: now, DeviceID: "fixture-device", Server: "https://fixture.invalid",
			Collection: &coverage.CollectionSnapshot{IndexedSources: 4},
			Upload:     &coverage.UploadSnapshot{Captured: &coverage.CapturedSnapshot{}},
			Policy:     &coverage.PolicySnapshot{Cowork: &coverage.CoworkPolicySnapshot{}},
			Parse:      &coverage.ParseSnapshot{DeviceID: "fixture-device", ObservedAt: now},
			Unknown:    map[string]string{"discovery": coverage.DiscoveryUnknown, "other_devices": coverage.OtherDevicesUnknown},
		}
	}
	absent, pending, held := ready(), ready(), ready()
	pending.Upload.Captured.PendingGenerations = 2
	pending.Parse.Pending = 3
	held.Policy.Cowork.SharedHeld = 1
	unknown := coverage.UnknownReport("optional observation unavailable within retrieval budget")
	unknown.ObservedAt = now
	type fixture struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
		Output    string         `json:"output"`
		Status    string         `json:"status"`
	}
	var cases []fixture
	for _, c := range []struct {
		name     string
		report   *coverage.Report
		excluded string
	}{
		{"empty", absent, ""}, {"unavailable", unknown, ""}, {"pending", pending, ""},
		{"held", held, "left out your own session fixture-self and its subagents; include_self=true includes them"},
	} {
		var out bytes.Buffer
		scope := &format.Scope{Kind: "shared", Server: "https://fixture.invalid", Coverage: c.report}
		fmt.Fprintln(&out, scopeNote(scope)+coverageNote(c.report, true))
		if err := format.WriteSearch(&out, &format.Page{Hits: []format.Hit{}, Exact: true, Excluded: c.excluded}, format.Style{MCP: true}); err != nil {
			t.Fatal(err)
		}
		status, err := json.Marshal(map[string]any{"ok": true, "coverage": c.report})
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, fixture{c.name, map[string]any{"query": "\"synthetic-unmatched-decision\"", "matching": "all", "format": "text", "include_self": c.excluded == ""}, out.String(), string(status)})
	}
	data, err := json.MarshalIndent(map[string]any{"instructions": mcpInstructions, "cases": cases}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
