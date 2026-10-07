package coverage

import "time"

// Report contains observations of separate coverage stages. Missing snapshots
// are unknown; an empty scheduler or parse queue does not prove completeness.
type Report struct {
	Server     string              `json:"server,omitempty"`
	DeviceID   string              `json:"device_id,omitempty"`
	ObservedAt time.Time           `json:"observed_at"`
	Collection *CollectionSnapshot `json:"collection,omitempty"`
	Upload     *UploadSnapshot     `json:"upload,omitempty"`
	Policy     *PolicySnapshot     `json:"policy,omitempty"`
	Parse      *ParseSnapshot      `json:"parse,omitempty"`
	Unknown    map[string]string   `json:"unknown,omitempty"`
}

type CollectionSnapshot struct {
	IndexedSources int64 `json:"indexed_sources"`
}

// QueuedSourceChecks counts scheduled work, not every unuploaded source.
// ActiveSourceTurns includes capture, authorization and upload work.
type UploadSnapshot struct {
	QueuedSourceChecks int               `json:"queued_source_checks"`
	ActiveSourceTurns  int               `json:"active_source_turns"`
	Captured           *CapturedSnapshot `json:"captured,omitempty"`
}

// CapturedSnapshot describes unacknowledged retained capture metadata. It
// excludes lost generations and does not verify object contents on the server.
type CapturedSnapshot struct {
	PendingGenerations     int64 `json:"pending_generations"`
	PendingManifestEntries int64 `json:"pending_manifest_entries"`
	PendingManifestBytes   int64 `json:"pending_manifest_bytes"`
	PendingTailBytes       int64 `json:"pending_tail_bytes"`
}

type PolicySnapshot struct {
	Cowork               *CoworkPolicySnapshot `json:"cowork,omitempty"`
	ServerCopiesRetained *int                  `json:"server_copies_retained,omitempty"`
}

// Scheduling eligibility is not a fresh capture authorization.
type CoworkPolicySnapshot struct {
	SharedHeld        int            `json:"shared_held"`
	ScheduleEligible  int            `json:"schedule_eligible"`
	HistoricalUnknown int            `json:"historical_unknown"`
	HoldReasons       map[string]int `json:"hold_reasons,omitempty"`
}

const DiscoveryUnknown = "configured roots and indexed sources do not prove exhaustive discovery"
const OtherDevicesUnknown = "this observation does not establish coverage on other devices"

// UnknownReport preserves missing observations explicitly for optional probes.
func UnknownReport(reason string) *Report {
	return &Report{ObservedAt: time.Now(), Unknown: map[string]string{
		"collection": reason, "upload": reason, "policy": reason, "parse": reason,
		"discovery": DiscoveryUnknown, "other_devices": OtherDevicesUnknown,
	}}
}
