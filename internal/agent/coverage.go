package agent

import (
	"context"
	"errors"
	"github.com/flopwire/flopwire/internal/devicesync"
	"maps"
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
			if report.Upload == nil {
				report.Unknown["upload"] = "scheduled source observations unavailable; scheduler state busy"
			} else if report.Upload.Captured == nil {
				report.Unknown["captured_upload"] = "retained capture counts unavailable; diagnostic budget or database read failed"
			}
			if errors.Is(err, devicesync.ErrCoverageSpoolBusy) {
				report.Unknown["upload_blocking"] = "spool blocking state unavailable; local state busy"
			}
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
	report.Unknown["cowork_policy"] = "current Cowork hold and scheduling eligibility were not observed by this lightweight report"
	if report.DeviceID == "" {
		report.Unknown["historical_mapping"] = "durable historical mapping evidence cannot be scoped without the initial collector device identity"
	} else {
		var historical int64
		if err := a.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM cowork_history WHERE device_id=?`, report.DeviceID).Scan(&historical); err != nil {
			report.Unknown["historical_mapping"] = "durable historical mapping evidence unavailable; not recorded or diagnostic budget exhausted"
		} else {
			policy.HistoricalMappingUnknown = &historical
		}
	}
	report.Policy = policy
	return report
}

// The ordinary status request already obtained this observation. Copy it
// without another scope lock, filesystem read or permission-graph walk.
func attachObservedCowork(report *coverage.Report, st *CoworkStatus) {
	if report == nil || st == nil {
		return
	}
	if report.Policy == nil {
		report.Policy = new(coverage.PolicySnapshot)
	}
	report.Policy.Cowork = &coverage.CoworkPolicySnapshot{SharedHeld: st.Held, ScheduleEligible: st.ScheduleEligible, HistoricalUnknown: st.HistoricalUnknown, HoldReasons: maps.Clone(st.HoldReasons)}
	delete(report.Unknown, "cowork_policy")
}
