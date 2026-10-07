package coverage

import (
	"encoding/json"
	"strings"
	"time"
)

const malformedObservation = "unsupported or malformed coverage observation"

// UnmarshalJSON makes optional diagnostics conservative across agent status and
// retrieval adapters. Invalid metadata never fails the enclosing successful RPC.
func (r *Report) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		*r = *UnknownReport(malformedObservation)
		return nil
	}
	var observed time.Time
	if json.Unmarshal(fields["observed_at"], &observed) != nil || observed.IsZero() {
		*r = *UnknownReport(malformedObservation)
		return nil
	}
	*r = Report{ObservedAt: observed, Unknown: map[string]string{}}
	json.Unmarshal(fields["unknown"], &r.Unknown)
	if r.Unknown == nil {
		r.Unknown = map[string]string{}
	}
	if json.Unmarshal(fields["device_id"], &r.DeviceID) != nil && present(fields["device_id"]) {
		r.Unknown["device"] = malformedObservation
	}
	if json.Unmarshal(fields["server"], &r.Server) != nil && present(fields["server"]) {
		r.Unknown["server"] = malformedObservation
	}
	var invalid bool
	r.Collection, invalid = decodeCounts[CollectionSnapshot](fields["collection"], "indexed_sources")
	if invalid {
		r.Unknown["collection"] = malformedObservation
	}
	// Capture counters are a separate optional fact from the queue counters.
	r.Upload, invalid = decodeCounts[UploadSnapshot](withoutField(fields["upload"], "captured"), "queued_source_checks", "active_source_turns", "failing_sources")
	if invalid {
		r.Unknown["upload"] = malformedObservation
	}
	if r.Upload != nil {
		var upload map[string]json.RawMessage
		json.Unmarshal(fields["upload"], &upload)
		r.Upload.Captured, invalid = decodeCounts[CapturedSnapshot](upload["captured"], "pending_generations", "pending_manifest_entries", "pending_manifest_bytes", "pending_tail_bytes", "lost_generations", "truncated_generations")
		if invalid {
			r.Unknown["captured_upload"] = malformedObservation
		}
	}
	if present(fields["policy"]) {
		var policy map[string]json.RawMessage
		if json.Unmarshal(fields["policy"], &policy) != nil || policy == nil {
			r.Unknown["policy"] = malformedObservation
		} else {
			r.Policy = &PolicySnapshot{}
			r.Policy.Cowork, invalid = decodeCounts[CoworkPolicySnapshot](policy["cowork"], "shared_held", "schedule_eligible", "historical_unknown")
			if r.Policy.Cowork != nil {
				var cowork map[string]json.RawMessage
				json.Unmarshal(policy["cowork"], &cowork)
				if present(cowork["hold_reasons"]) {
					var reasons map[string]json.RawMessage
					if json.Unmarshal(cowork["hold_reasons"], &reasons) != nil {
						invalid = true
					}
					for _, value := range reasons {
						var n int64
						if !present(value) || json.Unmarshal(value, &n) != nil || n < 0 {
							invalid = true
						}
					}
				}
				if invalid {
					r.Policy.Cowork = nil
				}
			}
			if invalid {
				r.Unknown["cowork_policy"] = malformedObservation
			}
			if present(policy["historical_mapping_unknown"]) {
				var n int64
				if json.Unmarshal(policy["historical_mapping_unknown"], &n) != nil || n < 0 {
					r.Unknown["historical_mapping"] = malformedObservation
				} else {
					r.Policy.HistoricalMappingUnknown = &n
				}
			}
			if present(policy["server_copies_retained"]) {
				var n int
				if json.Unmarshal(policy["server_copies_retained"], &n) != nil || n < 0 {
					r.Unknown["server_copies"] = malformedObservation
				} else {
					r.Policy.ServerCopiesRetained = &n
				}
			}
		}
	}
	r.Parse, invalid = decodeCounts[ParseSnapshot](fields["parse"], "pending", "failing", "quarantined", "untracked_sources")
	if r.Parse != nil {
		var parse map[string]json.RawMessage
		json.Unmarshal(fields["parse"], &parse)
		if r.Parse.DeviceID == "" || r.Parse.ObservedAt.IsZero() || r.Parse.Failing > r.Parse.Pending || parse["oldest_pending"] == nil {
			r.Parse = nil
			invalid = true
		}
	}
	if invalid {
		r.Unknown["parse"] = malformedObservation
	}
	return nil
}
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && strings.TrimSpace(string(raw)) != "null"
}
func decodeCounts[T any](raw json.RawMessage, keys ...string) (*T, bool) {
	if !present(raw) {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, true
	}
	for _, key := range keys {
		value := fields[key]
		var n int64
		if !present(value) || json.Unmarshal(value, &n) != nil || n < 0 {
			return nil, true
		}
	}
	var snapshot T
	if json.Unmarshal(raw, &snapshot) != nil {
		return nil, true
	}
	return &snapshot, false
}

func withoutField(raw json.RawMessage, key string) json.RawMessage {
	if !present(raw) {
		return raw
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return raw
	}
	delete(fields, key)
	value, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return value
}
