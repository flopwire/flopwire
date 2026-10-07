package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/coverage"
	"github.com/flopwire/flopwire/internal/retrieval/format"
)

const retrievalCoverageBudget = 250 * time.Millisecond

func agentCoverage(ctx context.Context) (*coverage.Report, error) {
	return agentCoverageAt(ctx, "")
}
func agentCoverageAt(ctx context.Context, socket string) (*coverage.Report, error) {
	var err error
	if socket == "" {
		socket, err = defaultSocket()
	}
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	if err = json.NewEncoder(conn).Encode(map[string]string{"op": "coverage"}); err != nil {
		return nil, err
	}
	var envelope struct {
		OK       bool            `json:"ok"`
		Coverage json.RawMessage `json:"coverage"`
	}
	if err = json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&envelope); err != nil {
		return nil, err
	}
	if !envelope.OK || len(envelope.Coverage) == 0 || string(envelope.Coverage) == "null" {
		return nil, errors.New("coverage unavailable")
	}
	return decodeAgentCoverage(envelope.Coverage)
}

// Keep older agents' missing additive counters unknown rather than implicit zero.
func decodeAgentCoverage(raw json.RawMessage) (*coverage.Report, error) {
	var report coverage.Report
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, err
	}
	if report.ObservedAt.IsZero() {
		return nil, errors.New("invalid agent observation")
	}
	if report.Unknown == nil {
		report.Unknown = map[string]string{}
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(raw, &fields)
	if report.Upload != nil {
		var upload map[string]json.RawMessage
		json.Unmarshal(fields["upload"], &upload)
		if !requiredCoverageFields(upload, "queued_source_checks", "active_source_turns", "failing_sources") {
			report.Upload = nil
			report.Unknown["upload"] = "unsupported agent upload observation"
		} else if report.Upload.Captured != nil {
			var captured map[string]json.RawMessage
			json.Unmarshal(upload["captured"], &captured)
			if !requiredCoverageFields(captured, "pending_generations", "pending_manifest_entries", "pending_manifest_bytes", "pending_tail_bytes", "lost_generations", "truncated_generations") {
				report.Upload.Captured = nil
				report.Unknown["captured_upload"] = "unsupported agent retained capture observation"
			}
		}
	}
	return &report, nil
}
func requiredCoverageFields(fields map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		v, ok := fields[key]
		if !ok || string(v) == "null" {
			return false
		}
	}
	return true
}

// Use the same credential and pinned transport as the retrieval request. Decode
// presence separately: an older or partial response must not invent zero counts.
func parseCoverageReader(cfg client.Config) func(context.Context) (*coverage.ParseSnapshot, error) {
	return func(ctx context.Context) (*coverage.ParseSnapshot, error) {
		var raw map[string]json.RawMessage
		api := cfg.API(cfg.Token)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api.Server, "/")+coverage.Path, nil)
		if err != nil {
			return nil, err
		}
		if api.Token != "" {
			req.Header.Set("Authorization", "Bearer "+api.Token)
		}
		hc := api.Client
		if hc == nil {
			hc = client.DefaultHTTPClient()
		}
		response, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, errors.New("server coverage unavailable")
		}
		const limit = 16 * 1024
		body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if len(body) > limit {
			return nil, errors.New("oversized server coverage observation")
		}
		if err = json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		for _, k := range []string{"device_id", "observed_at", "pending", "failing", "quarantined", "untracked_sources", "oldest_pending"} {
			v, ok := raw[k]
			if !ok || (k != "oldest_pending" && string(v) == "null") {
				return nil, errors.New("unsupported coverage response")
			}
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}
		var p coverage.ParseSnapshot
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		if cfg.DeviceID == "" || p.DeviceID != cfg.DeviceID || p.ObservedAt.IsZero() || p.Pending < 0 || p.Failing < 0 || p.Quarantined < 0 || p.UntrackedSources < 0 || p.Failing > p.Pending {
			return nil, errors.New("invalid coverage observation")
		}
		return &p, nil
	}
}

type coveragePending struct {
	cancel context.CancelFunc
	local  <-chan *coverage.Report
	parse  <-chan *coverage.ParseSnapshot
	cfg    *client.Config
}

// Diagnostic work begins beside retrieval. Snapshot consumes only ready results,
// so successful queries never wait for a diagnostic socket or server response.
func (r *retriever) startCoverage(ctx context.Context) coveragePending {
	ctx, cancel := context.WithTimeout(ctx, retrievalCoverageBudget)
	p := coveragePending{cancel: cancel, cfg: r.coverageConfig}
	if r.coverageLocal != nil {
		ch := make(chan *coverage.Report, 1)
		p.local = ch
		reader := r.coverageLocal
		// Agent.Call's socket deadline also bounds cleanup after cancellation.
		go func() {
			v, err := reader(ctx)
			if err != nil {
				v = nil
			}
			ch <- v
		}()
	}
	if r.coverageParse != nil {
		ch := make(chan *coverage.ParseSnapshot, 1)
		p.parse = ch
		reader := r.coverageParse
		go func() {
			v, err := reader(ctx)
			if err != nil {
				v = nil
			}
			ch <- v
		}()
	}
	return p
}
func sameCoverageBinding(r *coverage.Report, cfg *client.Config) bool {
	if r == nil || cfg == nil || cfg.DeviceID == "" || r.DeviceID != cfg.DeviceID {
		return false
	}
	a, e1 := client.NormalizeServer(r.Server)
	b, e2 := client.NormalizeServer(cfg.Server)
	return e1 == nil && e2 == nil && a == b
}
func (p coveragePending) snapshot(scope *format.Scope) *format.Scope {
	if scope == nil {
		return nil
	}
	out := *scope
	report := coverage.UnknownReport("optional observation unavailable within retrieval budget")
	select {
	case v := <-p.local:
		if v != nil && (scope.Kind != "shared" || sameCoverageBinding(v, p.cfg)) {
			report = v
		}
	default:
	}
	// Copy all mutable report metadata: MCP requests share retriever configuration.
	clone := *report
	clone.Unknown = make(map[string]string, len(report.Unknown)+2)
	for k, v := range report.Unknown {
		clone.Unknown[k] = v
	}
	report = &clone
	report.Unknown["discovery"] = coverage.DiscoveryUnknown
	report.Unknown["other_devices"] = coverage.OtherDevicesUnknown
	if scope.Kind == "shared" {
		report.Parse = nil
		report.Unknown["parse"] = "optional server observation unavailable within retrieval budget"
		select {
		case v := <-p.parse:
			if v != nil && p.cfg != nil && v.DeviceID == p.cfg.DeviceID {
				report.Parse = v
				delete(report.Unknown, "parse")
			}
		default:
		}
	} else {
		report.Upload = nil
		report.Policy = nil
		report.Parse = nil
		report.Server = ""
		report.DeviceID = ""
		for _, k := range []string{"upload", "policy", "parse", "captured_upload", "cowork_policy", "server_copies", "device"} {
			delete(report.Unknown, k)
		}
		delete(report.Unknown, "other_devices")
	}
	out.Coverage = report
	p.cancel()
	return &out
}

func coverageNote(r *coverage.Report, shared bool) string {
	if r == nil {
		return ""
	}
	parts := []string{"discovery unknown"}
	if shared {
		if r.Upload != nil {
			parts = append(parts, fmt.Sprintf("upload checks %d, active %d", r.Upload.QueuedSourceChecks, r.Upload.ActiveSourceTurns))
			if c := r.Upload.Captured; c != nil {
				parts = append(parts, fmt.Sprintf("captured pending %d, lost %d, truncated %d", c.PendingGenerations, c.LostGenerations, c.TruncatedGenerations))
			}
			if r.Upload.BlockingReason != "" || r.Upload.FailingSources > 0 {
				parts = append(parts, fmt.Sprintf("upload blocked %s, failing sources %d", r.Upload.BlockingReason, r.Upload.FailingSources))
			}
		} else {
			parts = append(parts, "upload unknown")
		}
		if r.Policy != nil && r.Policy.Cowork != nil {
			parts = append(parts, fmt.Sprintf("Cowork held %d", r.Policy.Cowork.SharedHeld))
		} else {
			parts = append(parts, "current policy unknown")
			if r.Policy != nil && r.Policy.HistoricalMappingUnknown != nil {
				parts = append(parts, fmt.Sprintf("historical mapping unknown %d", *r.Policy.HistoricalMappingUnknown))
			}
		}
		if p := r.Parse; p != nil {
			parts = append(parts, fmt.Sprintf("parse pending %d, failing %d, quarantined %d, untracked %d", p.Pending, p.Failing, p.Quarantined, p.UntrackedSources))
		} else {
			parts = append(parts, "parse unknown")
		}
		parts = append(parts, "this device only; other devices unknown")
	}
	return " [coverage: " + strings.Join(parts, "; ") + "]"
}
func printCoverage(w io.Writer, r *coverage.Report) {
	if r == nil {
		return
	}
	fmt.Fprintln(w, "coverage: this device only; other devices unknown")
	if r.Collection != nil {
		fmt.Fprintf(w, "  collection: %d indexed sources; discovery unknown\n", r.Collection.IndexedSources)
	} else {
		fmt.Fprintln(w, "  collection: unknown; discovery unknown")
	}
	if u := r.Upload; u != nil {
		fmt.Fprintf(w, "  upload: %d queued checks, %d active turns, %d failing sources\n", u.QueuedSourceChecks, u.ActiveSourceTurns, u.FailingSources)
		if u.BlockingReason != "" {
			fmt.Fprintf(w, "    blocked: %s\n", u.BlockingReason)
		}
		if c := u.Captured; c != nil {
			fmt.Fprintf(w, "    retained capture: %d pending generations, %d manifest bytes, %d tail bytes, %d lost generations, %d truncated generations\n", c.PendingGenerations, c.PendingManifestBytes, c.PendingTailBytes, c.LostGenerations, c.TruncatedGenerations)
		} else {
			fmt.Fprintln(w, "    retained capture: unknown")
		}
	} else {
		fmt.Fprintln(w, "  upload: unknown")
	}
	if p := r.Policy; p != nil && p.Cowork != nil {
		fmt.Fprintf(w, "  policy: Cowork shared held %d, schedule eligible %d, historical unknown %d\n", p.Cowork.SharedHeld, p.Cowork.ScheduleEligible, p.Cowork.HistoricalUnknown)
	} else {
		fmt.Fprintln(w, "  policy: unknown")
	}
	if p := r.Policy; p != nil && p.HistoricalMappingUnknown != nil {
		fmt.Fprintf(w, "    historical policy mapping unknown: %d\n", *p.HistoricalMappingUnknown)
	}
	if p := r.Policy; p != nil && p.ServerCopiesRetained != nil {
		fmt.Fprintf(w, "    server copies retained: %d\n", *p.ServerCopiesRetained)
	}
	if p := r.Parse; p != nil {
		fmt.Fprintf(w, "  server parse: %d pending, %d failing, %d quarantined, %d untracked\n", p.Pending, p.Failing, p.Quarantined, p.UntrackedSources)
	} else {
		fmt.Fprintln(w, "  server parse: unknown")
	}
}

// Optional metadata yields space to retrieval rows under a small output budget.
func budgetCoverageScope(scope *format.Scope, budget int) *format.Scope {
	if scope == nil || scope.Coverage == nil || budget <= 0 {
		return scope
	}
	raw, _ := json.Marshal(scope.Coverage)
	if len(raw) <= max(128, budget/4) {
		return scope
	}
	out := *scope
	out.Coverage = &coverage.Report{ObservedAt: scope.Coverage.ObservedAt, Unknown: map[string]string{"coverage": "output budget; observations omitted", "discovery": "unknown", "other_devices": "unknown"}}
	return &out
}
