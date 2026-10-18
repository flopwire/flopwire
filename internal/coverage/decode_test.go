package coverage

import (
	"encoding/json"
	"testing"
)

func TestReportMalformedFactsDoNotFailEnvelope(t *testing.T) {
	cases := []struct{ name, fields, key string }{
		{"collection missing", `"collection":{}`, "collection"},
		{"collection null", `"collection":{"indexed_sources":null}`, "collection"},
		{"collection negative", `"collection":{"indexed_sources":-1}`, "collection"},
		{"Cowork missing", `"policy":{"cowork":{"shared_held":0,"schedule_eligible":0}}`, "cowork_policy"},
		{"Cowork null", `"policy":{"cowork":{"shared_held":null,"schedule_eligible":0,"historical_unknown":0}}`, "cowork_policy"},
		{"Cowork hold null", `"policy":{"cowork":{"shared_held":0,"schedule_eligible":0,"historical_unknown":0,"hold_reasons":{"held":null}}}`, "cowork_policy"},
		{"Cowork negative", `"policy":{"cowork":{"shared_held":0,"schedule_eligible":-1,"historical_unknown":0}}`, "cowork_policy"},
		{"historical negative", `"policy":{"historical_mapping_unknown":-1}`, "historical_mapping"},
		{"copies negative", `"policy":{"server_copies_retained":-1}`, "server_copies"},
		{"upload negative", `"upload":{"queued_source_checks":0,"active_source_turns":0,"failing_sources":-1}`, "upload"},
		{"parse omitted", `"parse":{"device_id":"d","observed_at":"2026-10-07T00:00:00Z","pending":0}`, "parse"},
		{"captured malformed", `"upload":{"queued_source_checks":2,"active_source_turns":0,"failing_sources":0,"captured":"bad"}`, "captured_upload"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var envelope struct {
				OK       bool    `json:"ok"`
				Coverage *Report `json:"coverage"`
			}
			raw := `{"ok":true,"coverage":{"observed_at":"2026-10-07T00:00:00Z",` + tc.fields + `}}`
			if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || envelope.Coverage == nil || envelope.Coverage.Unknown[tc.key] == "" {
				t.Fatalf("optional fact affected core envelope: %+v", envelope)
			}
			if tc.key == "captured_upload" && (envelope.Coverage.Upload == nil || envelope.Coverage.Upload.QueuedSourceChecks != 2) {
				t.Fatal("bad capture lost independent queue facts")
			}
		})
	}
}
func TestReportInvalidObservationKeepsSuccessfulEnvelope(t *testing.T) {
	for _, raw := range []string{`{}`, `{"observed_at":"bad","collection":{"indexed_sources":2}}`, `{"observed_at":null}`, `"unsupported"`} {
		var report Report
		if err := json.Unmarshal([]byte(raw), &report); err != nil {
			t.Fatal(err)
		}
		if report.Collection != nil || report.Unknown["collection"] == "" {
			t.Fatalf("bad observation became facts: %+v", report)
		}
	}
}
func TestReportKnownZeroCountersRoundTrip(t *testing.T) {
	raw := `{"observed_at":"2026-10-07T00:00:00Z","collection":{"indexed_sources":0},"upload":{"queued_source_checks":0,"active_source_turns":0,"failing_sources":0,"captured":{"pending_generations":0,"pending_manifest_entries":0,"pending_manifest_bytes":0,"pending_tail_bytes":0,"lost_generations":0,"truncated_generations":0}},"policy":{"cowork":{"shared_held":0,"schedule_eligible":0,"historical_unknown":0},"historical_mapping_unknown":0,"server_copies_retained":0}}`
	var report Report
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if report.Collection == nil || report.Upload == nil || report.Upload.Captured == nil || report.Policy.Cowork == nil || report.Policy.HistoricalMappingUnknown == nil || report.Policy.ServerCopiesRetained == nil {
		t.Fatalf("explicit zero facts lost: %+v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var again Report
	if err = json.Unmarshal(encoded, &again); err != nil {
		t.Fatal(err)
	}
	if again.Upload.Captured == nil || len(again.Unknown) > 0 {
		t.Fatalf("zero roundtrip became unknown: %+v", again)
	}
}
