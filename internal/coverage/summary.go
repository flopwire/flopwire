package coverage

import "time"

// Summary is an MCP projection. Omitted issue counts are not assertions of zero.
type Summary struct {
	Mode             string            `json:"mode"`
	ObservationScope string            `json:"observation_scope"`
	ObservedAt       *time.Time        `json:"observed_at,omitempty"`
	ParseObservedAt  *time.Time        `json:"parse_observed_at,omitempty"`
	Qualification    string            `json:"qualification"`
	Issues           map[string]int64  `json:"issues,omitempty"`
	BlockingReasons  []string          `json:"blocking_reasons,omitempty"`
	Unknown          map[string]string `json:"unknown,omitempty"`
}

// Summarize projects observations, never retrieval rows or query scope.
func Summarize(r *Report, shared bool) *Summary {
	s := &Summary{Mode: "summary", ObservationScope: "current_device", Qualification: "Coverage observations do not establish exhaustive discovery.", Issues: map[string]int64{}, Unknown: map[string]string{}}
	if shared {
		s.Qualification = "Coverage observations do not establish exhaustive discovery or other-device completeness."
	}
	if r == nil {
		s.Unknown["collection"] = "observation unavailable"
		if shared {
			for _, k := range []string{"upload", "policy", "parse"} {
				s.Unknown[k] = "observation unavailable"
			}
		}
		return s
	}
	for k, v := range r.Unknown {
		if k != "discovery" && k != "other_devices" {
			s.Unknown[k] = v
		}
	}
	if !r.ObservedAt.IsZero() && (r.Collection != nil || r.Upload != nil || r.Policy != nil) {
		t := r.ObservedAt
		s.ObservedAt = &t
	}
	if r.Collection == nil && s.Unknown["collection"] == "" {
		s.Unknown["collection"] = "observation unavailable"
	}
	add := func(k string, n int64) {
		if n > 0 {
			s.Issues[k] = n
		}
	}
	if !shared {
		return s
	}
	if u := r.Upload; u != nil {
		add("upload_failing_sources", int64(u.FailingSources))
		if u.BlockingReason != "" {
			s.BlockingReasons = []string{u.BlockingReason}
		}
		if c := u.Captured; c != nil {
			add("capture_pending_generations", c.PendingGenerations)
			add("capture_lost_generations", c.LostGenerations)
			add("capture_truncated_generations", c.TruncatedGenerations)
		} else if s.Unknown["captured_upload"] == "" {
			s.Unknown["captured_upload"] = "observation unavailable"
		}
	} else if s.Unknown["upload"] == "" {
		s.Unknown["upload"] = "observation unavailable"
	}
	if p := r.Policy; p != nil {
		if p.Cowork != nil {
			add("policy_shared_held", int64(p.Cowork.SharedHeld))
			add("policy_historical_unknown", int64(p.Cowork.HistoricalUnknown))
		} else if s.Unknown["policy"] == "" {
			s.Unknown["policy"] = "current policy observation unavailable"
		}
		if p.HistoricalMappingUnknown != nil {
			add("policy_historical_mapping_unknown", *p.HistoricalMappingUnknown)
		}
	} else if s.Unknown["policy"] == "" {
		s.Unknown["policy"] = "observation unavailable"
	}
	if p := r.Parse; p != nil {
		if !p.ObservedAt.IsZero() {
			t := p.ObservedAt
			s.ParseObservedAt = &t
		}
		add("parse_pending", p.Pending)
		add("parse_failing", p.Failing)
		add("parse_quarantined", p.Quarantined)
		add("parse_untracked_sources", p.UntrackedSources)
	} else if s.Unknown["parse"] == "" {
		s.Unknown["parse"] = "observation unavailable"
	}
	return s
}

// Presentation selects only encoding; the report remains the single snapshot.
type Presentation string

const (
	DetailMode        Presentation = "detail"
	SummaryMode       Presentation = "summary"
	BudgetSummaryMode Presentation = "summary_budget"
)

func Present(r *Report, shared bool, mode Presentation) *Summary {
	s := Summarize(r, shared)
	if mode == BudgetSummaryMode {
		return &Summary{Mode: "summary", ObservationScope: "current_device", Qualification: s.Qualification, Unknown: map[string]string{"coverage": "output budget; observations omitted"}}
	}
	return s
}
