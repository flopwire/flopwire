package coverage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSummaryAvailableIssuesAndSeparateTimes(t *testing.T) {
	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	historical := int64(7)
	r := &Report{ObservedAt: now, Collection: &CollectionSnapshot{IndexedSources: 125}, Upload: &UploadSnapshot{FailingSources: 2, QueuedSourceChecks: 99, BlockingReason: "capacity", Captured: &CapturedSnapshot{PendingGenerations: 4, LostGenerations: 2, TruncatedGenerations: 1}}, Policy: &PolicySnapshot{Cowork: &CoworkPolicySnapshot{SharedHeld: 3}, HistoricalMappingUnknown: &historical}, Parse: &ParseSnapshot{ObservedAt: now.Add(time.Second), Pending: 5, Failing: 1, Quarantined: 2, UntrackedSources: 3}, Unknown: map[string]string{"discovery": DiscoveryUnknown, "other_devices": OtherDevicesUnknown}}
	s := Summarize(r, true)
	for k, n := range map[string]int64{"capture_pending_generations": 4, "capture_lost_generations": 2, "capture_truncated_generations": 1, "policy_shared_held": 3, "policy_historical_mapping_unknown": 7, "parse_pending": 5, "parse_failing": 1, "parse_quarantined": 2, "parse_untracked_sources": 3, "upload_failing_sources": 2} {
		if s.Issues[k] != n {
			t.Errorf("%s=%d want %d", k, s.Issues[k], n)
		}
	}
	if s.ObservedAt == nil || !s.ObservedAt.Equal(now) || s.ParseObservedAt == nil || !s.ParseObservedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("timestamps: %+v", s)
	}
	b, _ := json.Marshal(s)
	for _, k := range []string{"indexed_sources", "queued_source_checks", "other_devices\"", "discovery\"", "manifest"} {
		if strings.Contains(string(b), k) {
			t.Fatalf("unexpected %s: %s", k, b)
		}
	}
	if len(s.Unknown) != 0 || len(s.BlockingReasons) != 1 {
		t.Fatalf("summary: %+v", s)
	}
}
func TestSummaryUnknownIsNotZeroOrObservation(t *testing.T) {
	s := Summarize(UnknownReport("retrieval budget"), true)
	if s.ObservedAt != nil || s.ParseObservedAt != nil || len(s.Issues) != 0 || len(s.Unknown) != 4 {
		t.Fatalf("unknown: %+v", s)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), "observed_at") || strings.Contains(string(b), "issues") {
		t.Fatalf("fabricated observations: %s", b)
	}
	local := Summarize(&Report{Collection: &CollectionSnapshot{}}, false)
	if strings.Contains(local.Qualification, "other-device") || len(local.Unknown) != 0 {
		t.Fatalf("local scope: %+v", local)
	}
}
func TestSummaryBudgetPreservesQualification(t *testing.T) {
	s := Present(UnknownReport("unavailable"), true, BudgetSummaryMode)
	if s.ObservedAt != nil || s.ParseObservedAt != nil || len(s.Unknown) != 1 || s.Unknown["coverage"] == "" || !strings.Contains(s.Qualification, "other-device") {
		t.Fatalf("budget: %+v", s)
	}
}
