// Package coverage describes observed stages of history coverage. A snapshot
// does not prove that local discovery or collection is complete.
package coverage

import "time"

const Path = "/v1/sync/coverage"

// ParseSnapshot reports the durable server parse queue for one device. Pending
// excludes quarantined sources; Failing is the subset of pending sources with
// consecutive failures. UntrackedSources counts active non-companion sources without
// an applied parser or parse state. Zero counts describe this observation, not completeness.
type ParseSnapshot struct {
	DeviceID         string     `json:"device_id"`
	ObservedAt       time.Time  `json:"observed_at"`
	UntrackedSources int64      `json:"untracked_sources"`
	Pending          int64      `json:"pending"`
	Failing          int64      `json:"failing"`
	Quarantined      int64      `json:"quarantined"`
	OldestPending    *time.Time `json:"oldest_pending"`
}
