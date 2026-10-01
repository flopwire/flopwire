package transcript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
)

const ReportVersion = 1
const DiagnosticSampleLimit = 8

type DiagnosticCode string

const (
	MalformedRecord   DiagnosticCode = "malformed_record"
	RecordTooLarge    DiagnosticCode = "record_too_large"
	FieldTypeMismatch DiagnosticCode = "field_type_mismatch"
	UnknownRecordType DiagnosticCode = "unknown_record_type"
	TextTruncated     DiagnosticCode = "text_truncated"
)

var diagnosticCodes = [...]DiagnosticCode{MalformedRecord, RecordTooLarge, FieldTypeMismatch, UnknownRecordType, TextTruncated}

// Severity derives presentation from the evidence code. Unknown types are
// coverage observations, not proof that user messages were lost.
func (c DiagnosticCode) Severity() string {
	if c == UnknownRecordType {
		return "info"
	}
	return "warning"
}

type DiagnosticLocator struct {
	Line       int64 `json:"line"`
	ByteOffset int64 `json:"byte_offset"`
}
type DiagnosticSummary struct {
	Code    DiagnosticCode      `json:"code"`
	Count   uint64              `json:"count"`
	Samples []DiagnosticLocator `json:"samples"`
}

// ExtractionReport counts affected records per code, not lost messages.
// The same record can have more than one code. An empty report is assessed
// input; a nil persisted checkpoint means unassessed input.
type ExtractionReport struct {
	Version int                 `json:"version"`
	Issues  []DiagnosticSummary `json:"issues"`
}

// ParseResult describes the actual range consumed, including an automatic
// restart from zero after incompatible cursor state. On failure no result
// is committable. FromOffset is invocation metadata, not parser state.
type ParseResult struct {
	FromOffset int64
	Cursor     Cursor
	Report     *ExtractionReport
}
type ReportingParser interface {
	Parser
	ExtractionContract() string
	ParseWithReport(context.Context, Input, Cursor, Sink) (ParseResult, error)
}

// Extract preserves support for storage kinds without report semantics.
// Such sources remain unassessed instead of being labelled issue-free.
func Extract(ctx context.Context, p Parser, in Input, cur Cursor, sink Sink) (ParseResult, error) {
	if rp, ok := p.(ReportingParser); ok {
		return rp.ParseWithReport(ctx, in, cur, sink)
	}
	next, err := p.Parse(ctx, in, cur, sink)
	if err != nil {
		return ParseResult{}, err
	}
	return ParseResult{FromOffset: cur.Offset, Cursor: next}, nil
}
func ParserContract(p Parser) string {
	if rp, ok := p.(ReportingParser); ok {
		return rp.ExtractionContract()
	}
	return ""
}

// Diagnostics lives for one invocation and retains a fixed number of numeric
// locators. One code is counted once per physical record. Callers report
// records in input order and do not report re-emitted historical messages.
type Diagnostics struct {
	FromOffset int64
	counts     [len(diagnosticCodes)]uint64
	last       [len(diagnosticCodes)]int64
	samples    [len(diagnosticCodes)][DiagnosticSampleLimit]DiagnosticLocator
}

func (d *Diagnostics) Record(code DiagnosticCode, line, offset int64) {
	if d == nil || offset < d.FromOffset {
		return
	}
	i := slices.Index(diagnosticCodes[:], code)
	if i < 0 {
		panic("unknown diagnostic code")
	}
	n := d.counts[i]
	if n != 0 && d.last[i] == offset {
		return
	}
	d.last[i] = offset
	if n < DiagnosticSampleLimit {
		d.samples[i][n] = DiagnosticLocator{Line: line, ByteOffset: offset}
	}
	// A uint64 is much larger than any realizable source; never wrap evidence.
	if n < math.MaxUint64 {
		d.counts[i]++
	}
}
func (d *Diagnostics) Report() ExtractionReport {
	r := ExtractionReport{Version: ReportVersion, Issues: []DiagnosticSummary{}}
	for i, n := range d.counts {
		if n == 0 {
			continue
		}
		samples := append([]DiagnosticLocator(nil), d.samples[i][:min(n, uint64(DiagnosticSampleLimit))]...)
		r.Issues = append(r.Issues, DiagnosticSummary{Code: diagnosticCodes[i], Count: n, Samples: samples})
	}
	return r
}
func (r ExtractionReport) Validate() error {
	if r.Version != ReportVersion {
		return fmt.Errorf("unsupported extraction report version %d", r.Version)
	}
	seen := map[DiagnosticCode]bool{}
	for _, issue := range r.Issues {
		if !slices.Contains(diagnosticCodes[:], issue.Code) || seen[issue.Code] || issue.Count == 0 || len(issue.Samples) > DiagnosticSampleLimit || uint64(len(issue.Samples)) > issue.Count {
			return errors.New("invalid extraction issue summary")
		}
		seen[issue.Code] = true
		for _, sample := range issue.Samples {
			if sample.Line < 1 || sample.ByteOffset < 0 {
				return errors.New("invalid extraction locator")
			}
		}
	}
	return nil
}
func MergeReports(a, b ExtractionReport) (ExtractionReport, error) {
	if err := a.Validate(); err != nil {
		return ExtractionReport{}, err
	}
	if err := b.Validate(); err != nil {
		return ExtractionReport{}, err
	}
	r := ExtractionReport{Version: ReportVersion, Issues: []DiagnosticSummary{}}
	for _, code := range diagnosticCodes {
		dst := DiagnosticSummary{Code: code}
		for _, src := range []ExtractionReport{a, b} {
			for _, issue := range src.Issues {
				if issue.Code != code {
					continue
				}
				if math.MaxUint64-dst.Count < issue.Count {
					return ExtractionReport{}, errors.New("extraction count overflow")
				}
				dst.Count += issue.Count
				remaining := DiagnosticSampleLimit - len(dst.Samples)
				dst.Samples = append(dst.Samples, issue.Samples[:min(remaining, len(issue.Samples))]...)
			}
		}
		if dst.Count != 0 {
			r.Issues = append(r.Issues, dst)
		}
	}
	return r, nil
}

// ExtractionCheckpoint is scoped evidence. Its scope travels with the report
// so a generation started before a failed parse cannot relabel old evidence.
type ExtractionCheckpoint struct {
	Generation int64            `json:"generation"`
	Contract   string           `json:"contract"`
	Offset     int64            `json:"offset"`
	LineNo     int64            `json:"line"`
	Report     ExtractionReport `json:"report"`
}

func FinalizeExtraction(previous *ExtractionCheckpoint, generation int64, contract string, result ParseResult) (*ExtractionCheckpoint, error) {
	if result.Report == nil {
		return nil, nil
	}
	if contract == "" || result.FromOffset < 0 || result.Cursor.Offset < result.FromOffset {
		return nil, errors.New("invalid extraction scope")
	}
	if err := result.Report.Validate(); err != nil {
		return nil, err
	}
	report := *result.Report
	if result.FromOffset != 0 {
		if previous == nil || previous.Generation != generation || previous.Contract != contract || previous.Offset != result.FromOffset {
			return nil, errors.New("extraction append does not match committed checkpoint")
		}
		var err error
		report, err = MergeReports(previous.Report, report)
		if err != nil {
			return nil, err
		}
	}
	return &ExtractionCheckpoint{Generation: generation, Contract: contract, Offset: result.Cursor.Offset, LineNo: result.Cursor.LineNo, Report: report}, nil
}

// Contract hashes the effective extraction policy. Caps are normalized so
// nil/default and an explicit equivalent configuration share a contract.
func Contract(name string, caps map[Kind]CapConfig, maxRecord, maxCompanion int64) string {
	if caps == nil {
		caps = DefaultCaps
	}
	type capPolicy struct {
		Kind                        Kind
		Head, Tail, Middle, MaxLine int
		Keep                        []string
	}
	policy := struct {
		Version                 int
		MaxRecord, MaxCompanion int64
		Caps                    []capPolicy
	}{Version: ReportVersion, MaxRecord: maxRecord, MaxCompanion: maxCompanion}
	keys := make([]Kind, 0, len(caps))
	for kind := range caps {
		keys = append(keys, kind)
	}
	slices.Sort(keys)
	for _, kind := range keys {
		cfg := caps[kind]
		if cfg.Head <= 0 && cfg.Tail <= 0 {
			continue
		}
		entry := capPolicy{Kind: kind, Head: cfg.Head, Tail: cfg.Tail, Middle: cfg.MiddleBudget, MaxLine: cfg.MaxLineLen}
		for _, pattern := range cfg.Keep {
			if pattern != nil {
				entry.Keep = append(entry.Keep, pattern.String())
			}
		}
		policy.Caps = append(policy.Caps, entry)
	}
	b, _ := json.Marshal(policy)
	sum := sha256.Sum256(b)
	return ReparseKey(name) + "/extraction@1/" + hex.EncodeToString(sum[:16])
}
