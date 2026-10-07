package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/coverage"
)

type observedCoverageSync struct {
	*recorder
	report *coverage.UploadSnapshot
	wait   bool
}

func (s observedCoverageSync) CoverageContext(ctx context.Context) (*coverage.UploadSnapshot, error) {
	if s.wait {
		<-ctx.Done()
		return s.report, ctx.Err()
	}
	return s.report, nil
}

func TestCoverageEmptyQueuesNeverClaimExhaustiveDiscovery(t *testing.T) {
	f := newFixture(t, "-")
	f.a.cfg.DeviceID = "synthetic-device"
	f.a.cfg.Server = "https://synthetic.invalid/"
	f.a.cfg.Sync = observedCoverageSync{recorder: f.rec, report: &coverage.UploadSnapshot{Captured: &coverage.CapturedSnapshot{}}}
	resp := ask(t, f.a, Request{Op: "coverage"})
	if !resp.OK || resp.Coverage == nil {
		t.Fatalf("response=%+v", resp)
	}
	r := resp.Coverage
	if r.DeviceID != "synthetic-device" || r.Server != "https://synthetic.invalid" || r.Collection == nil || r.Collection.IndexedSources != 0 || r.Upload == nil || r.Upload.Captured == nil || r.Upload.QueuedSourceChecks != 0 {
		t.Fatalf("facts=%+v", r)
	}
	if r.Unknown["discovery"] == "" || r.Unknown["other_devices"] == "" || r.Unknown["parse"] == "" || r.Parse != nil {
		t.Fatalf("unknown stages=%v", r.Unknown)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["complete"]; ok {
		t.Fatal("empty queues emitted a completeness assertion")
	}
	if resp.Extraction != nil || resp.DesktopCode != nil || resp.Sync != nil {
		t.Fatal("dedicated coverage invoked full status diagnostics")
	}
}

func TestCoverageBusyCapturesAndCoworkRemainUnknownWithinCallerBudget(t *testing.T) {
	f := newCoworkFixture(t)
	f.a.cfg.Sync = observedCoverageSync{recorder: f.rec, report: &coverage.UploadSnapshot{QueuedSourceChecks: 7, ActiveSourceTurns: 1}, wait: true}
	f.a.captureScopeMu.Lock()
	defer f.a.captureScopeMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := f.a.coverageReport(ctx)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("coverage exceeded caller diagnostic budget")
	}
	if r.Upload == nil || r.Upload.QueuedSourceChecks != 7 || r.Upload.ActiveSourceTurns != 1 || r.Upload.Captured != nil || r.Unknown["captured_upload"] == "" || r.Policy.Cowork != nil || r.Unknown["cowork_policy"] == "" {
		t.Fatalf("partial facts=%+v unknown=%v", r, r.Unknown)
	}
	if r.Unknown["device"] == "" || r.DeviceID != "" {
		t.Fatal("missing initial device binding was inferred")
	}
}

func TestCoverageLegacySyncStubRemainsCompatibleAndUnknown(t *testing.T) {
	f := newFixture(t, "-")
	f.a.cfg.Sync = statusSync{recorder: f.rec}
	r := f.a.coverageReport(t.Context())
	if r.Upload != nil || r.Unknown["upload"] == "" {
		t.Fatalf("legacy sync observations=%+v", r)
	}
	resp := ask(t, f.a, Request{Op: "status"})
	if !resp.OK || resp.Sync == nil || resp.Coverage == nil || resp.Coverage.Unknown["upload"] == "" {
		t.Fatalf("legacy status=%+v", resp)
	}
}

func TestLightweightCoverageReadsHistoryEvidenceWithoutClaimingEligibility(t *testing.T) {
	f := newCoworkFixture(t)
	f.a.cfg.DeviceID = "local"
	coworkMetadata(t, f, nil, nil, nil)
	coworkTranscript(t, f)
	if err := f.a.Once(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.a.captureScopeMu.Lock()
	r := f.a.coverageReport(t.Context())
	f.a.captureScopeMu.Unlock()
	if r.Policy == nil || r.Policy.Cowork != nil || r.Unknown["cowork_policy"] == "" || r.Policy.HistoricalMappingUnknown == nil || *r.Policy.HistoricalMappingUnknown < 1 {
		t.Fatalf("durable evidence was mistaken for eligibility: %+v unknown=%v", r.Policy, r.Unknown)
	}
}

func TestStatusCopiesAlreadyObservedCoworkWithoutPermissionProbe(t *testing.T) {
	report := coverage.UnknownReport("not observed")
	report.Unknown["cowork_policy"] = "not observed"
	observed := &CoworkStatus{Held: 3, ScheduleEligible: 2, HistoricalUnknown: 1, HoldReasons: map[string]int{"history_unknown": 1}}
	attachObservedCowork(report, observed)
	observed.HoldReasons["history_unknown"] = 99
	if report.Policy == nil || report.Policy.Cowork == nil || report.Policy.Cowork.SharedHeld != 3 || report.Policy.Cowork.ScheduleEligible != 2 || report.Policy.Cowork.HoldReasons["history_unknown"] != 1 || report.Unknown["cowork_policy"] != "" {
		t.Fatalf("copy=%+v unknown=%v", report.Policy, report.Unknown)
	}
}
