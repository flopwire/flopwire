package agent

import (
	"context"
	"strings"

	"github.com/flopwire/flopwire/internal/coverage"
)

// coverageReport omits exhaustive discovery and remote observations: neither
// configured roots nor a drained local queue prove those stages complete.
func (a *Agent) coverageReport(ctx context.Context) *coverage.Report {
	report := &coverage.Report{DeviceID: a.cfg.DeviceID, Server: strings.TrimRight(a.cfg.Server, "/"), ObservedAt: a.now(), Unknown: map[string]string{
		"discovery": coverage.DiscoveryUnknown, "other_devices": coverage.OtherDevicesUnknown,
		"parse": "server parse progress is not observed by this local report",
	}}
	if report.DeviceID == "" {
		report.Unknown["device"] = "initial collector device identity is unavailable"
	}
	if report.Server == "" {
		report.Unknown["server"] = "initial collector server is unavailable"
	}
	// Memory health does not wait for the optional captured-metadata query.
	if sync, ok := a.cfg.Sync.(interface {
		CoverageContext(context.Context) (*coverage.UploadSnapshot, error)
	}); ok {
		var err error
		report.Upload, err = sync.CoverageContext(ctx)
		if err != nil {
			report.Unknown["captured_upload"] = "retained capture counts unavailable; diagnostic budget or database read failed"
		}
	} else if a.cfg.Sync == nil {
		report.Unknown["upload"] = "sync is disabled; captured upload progress is not observed"
	} else {
		report.Unknown["upload"] = "sync implementation does not expose coverage observations"
	}
	var count int64
	if err := a.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM sources`).Scan(&count); err != nil {
		report.Unknown["collection"] = "indexed source count unavailable; diagnostic budget or database read failed"
	} else {
		report.Collection = &coverage.CollectionSnapshot{IndexedSources: count}
	}
	policy := new(coverage.PolicySnapshot)
	if a.mu.TryLock() {
		n := 0
		if a.serverCopies != nil {
			n = a.serverCopies.Count
		}
		policy.ServerCopiesRetained = &n
		a.mu.Unlock()
	} else {
		report.Unknown["server_copies"] = "known retained server copies unavailable; local state busy"
	}
	cowork, err := a.coworkStatusContext(ctx)
	if err != nil || cowork == nil {
		report.Unknown["cowork_policy"] = "Cowork policy observation unavailable; local state busy or diagnostic budget exhausted"
	} else {
		policy.Cowork = &coverage.CoworkPolicySnapshot{SharedHeld: cowork.Held, ScheduleEligible: cowork.ScheduleEligible, HistoricalUnknown: cowork.HistoricalUnknown, HoldReasons: cowork.HoldReasons}
	}
	report.Policy = policy
	return report
}
