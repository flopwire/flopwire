package transcript_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

func issueCount(r *transcript.ExtractionReport, code transcript.DiagnosticCode) uint64 {
	if r == nil {
		return 0
	}
	for _, issue := range r.Issues {
		if issue.Code == code {
			return issue.Count
		}
	}
	return 0
}
func extraction(t *testing.T, p transcript.Parser, data []byte, cur transcript.Cursor) transcript.ParseResult {
	t.Helper()
	result, err := transcript.Extract(context.Background(), p, transcript.Input{R: bytes.NewReader(data), Size: int64(len(data))}, cur, &transcript.Collector{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func TestReportAppendBoundariesAndRestart(t *testing.T) {
	cases := []struct {
		name string
		p    transcript.Parser
		data []byte
	}{
		{"claude", &claude.Parser{}, []byte("{broken\n" + `{"type":"user","sessionId":"s","message":{"content":"hello"}}` + "\n" + `{"type":"future-record"}` + "\n{broken\n")},
		{"codex", &codex.Parser{}, []byte(`{"type":"session_meta","payload":{"id":"s"}}` + "\n{broken\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":"hello"}}` + "\n" + `{"type":"future-record"}` + "\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := extraction(t, tc.p, tc.data, transcript.Cursor{})
			if issueCount(full.Report, transcript.MalformedRecord) == 0 || issueCount(full.Report, transcript.UnknownRecordType) != 1 {
				t.Fatalf("missing evidence %+v", full.Report)
			}
			for cut := 0; cut <= len(tc.data); cut++ {
				first := extraction(t, tc.p, tc.data[:cut], transcript.Cursor{})
				checkpoint, err := transcript.FinalizeExtraction(nil, 1, transcript.ParserContract(tc.p), first)
				if err != nil {
					t.Fatal(err)
				}
				second := extraction(t, tc.p, tc.data, first.Cursor)
				checkpoint, err = transcript.FinalizeExtraction(checkpoint, 1, transcript.ParserContract(tc.p), second)
				if err != nil {
					t.Fatalf("cut %d: %v", cut, err)
				}
				if !reflect.DeepEqual(checkpoint.Report, *full.Report) {
					t.Fatalf("cut %d differs", cut)
				}
			}
			previous, err := transcript.FinalizeExtraction(nil, 1, transcript.ParserContract(tc.p), full)
			if err != nil {
				t.Fatal(err)
			}
			broken := full.Cursor
			broken.State = []byte(`{"v":-1}`)
			restarted := extraction(t, tc.p, tc.data, broken)
			if restarted.FromOffset != 0 {
				t.Fatal("restart reported append scope")
			}
			replaced, err := transcript.FinalizeExtraction(previous, 1, transcript.ParserContract(tc.p), restarted)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replaced.Report, previous.Report) {
				t.Fatal("restart double counted")
			}
			appendResult := transcript.ParseResult{FromOffset: full.Cursor.Offset, Cursor: full.Cursor, Report: full.Report}
			if _, err := transcript.FinalizeExtraction(previous, 2, transcript.ParserContract(tc.p), appendResult); err == nil {
				t.Fatal("merged different generation")
			}
			if _, err := transcript.FinalizeExtraction(previous, 1, "different-contract", appendResult); err == nil {
				t.Fatal("merged different contract")
			}
		})
	}
}
func TestReportsBoundSamplesAndValidate(t *testing.T) {
	var combined = transcript.ExtractionReport{Version: 1, Issues: []transcript.DiagnosticSummary{}}
	for line := int64(1); line <= 10000; line++ {
		d := transcript.Diagnostics{FromOffset: (line - 1) * 8}
		d.Record(transcript.MalformedRecord, line, (line-1)*8)
		d.Record(transcript.MalformedRecord, line, (line-1)*8) // sibling observations count once
		next, err := transcript.MergeReports(combined, d.Report())
		if err != nil {
			t.Fatal(err)
		}
		combined = next
	}
	if combined.Issues[0].Count != 10000 || len(combined.Issues[0].Samples) != 8 {
		t.Fatal("incorrect bound")
	}
	original := combined.Issues[0].Samples[0]
	merged, err := transcript.MergeReports(combined, transcript.ExtractionReport{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	merged.Issues[0].Samples[0].Line = 99
	if combined.Issues[0].Samples[0] != original {
		t.Fatal("merge mutated input")
	}
	invalid := combined
	invalid.Version = 2
	if _, err := transcript.MergeReports(combined, invalid); err == nil {
		t.Fatal("unsupported report version merged")
	}
	if transcript.UnknownRecordType.Severity() != "info" || transcript.MalformedRecord.Severity() != "warning" {
		t.Fatal("wrong severity")
	}
}

var readFailure = errors.New("injected materialization failure")

type failingMaterialization struct{ data []byte }

func (r failingMaterialization) ReadAt(p []byte, off int64) (int, error) {
	if len(p) < 1024 {
		return 0, readFailure
	}
	return bytes.NewReader(r.data).ReadAt(p, off)
}
func TestCodexMaterializationFailurePropagates(t *testing.T) {
	for _, line := range []string{
		`{"type":"session_meta","payload":{"id":"s","padding":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`,
		`{"type":"turn_context","payload":{"turn_id":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}}`,
		`{"id":"s","padding":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`,
	} {
		data := []byte(line + "\n")
		p := &codex.Parser{LineOptions: transcript.LineReaderOptions{MaxLine: 32}}
		result, err := p.ParseWithReport(context.Background(), transcript.Input{R: failingMaterialization{data}, Size: int64(len(data))}, transcript.Cursor{}, &transcript.Collector{})
		if !errors.Is(err, readFailure) || result.Report != nil {
			t.Fatalf("read failure swallowed: %v %+v", err, result)
		}
	}
}
func TestCodexMismatchRetainsUsableFields(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"s","cli_version":22}}` + "\n")
	result := extraction(t, &codex.Parser{}, data, transcript.Cursor{})
	if issueCount(result.Report, transcript.FieldTypeMismatch) != 1 {
		t.Fatal("missing field mismatch")
	}
}
func TestContractEffectivePolicy(t *testing.T) {
	a := (&claude.Parser{}).ExtractionContract()
	if a != (&claude.Parser{Caps: transcript.DefaultCaps}).ExtractionContract() {
		t.Fatal("equivalent caps differ")
	}
	if a == (&claude.Parser{Caps: map[transcript.Kind]transcript.CapConfig{}}).ExtractionContract() {
		t.Fatal("uncapped contract matches capped")
	}
	if a == (&claude.Parser{MaxPersisted: 1}).ExtractionContract() {
		t.Fatal("companion policy missing from contract")
	}
}

type badReader struct{}

func (badReader) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestReadFailureHasNoReport(t *testing.T) {
	for _, p := range []transcript.ReportingParser{&claude.Parser{}, &codex.Parser{}} {
		result, err := p.ParseWithReport(context.Background(), transcript.Input{R: badReader{}, Size: 1}, transcript.Cursor{}, &transcript.Collector{})
		if err == nil || result.Report != nil {
			t.Fatal("failed parse returned evidence")
		}
	}
}

func TestClaudeFieldMismatchesAreVisible(t *testing.T) {
	for _, line := range []string{
		`{"type":"user","sessionId":"s","message":42}`,
		`{"type":"user","sessionId":"s","isCompactSummary":true,"message":{"content":[{"type":"text","text":42}]}}`,
		`{"type":"attachment","sessionId":"s","attachment":{"type":"queued_command","commandMode":"prompt","prompt":[{"type":"text","text":42}]}}`,
		`{"type":"attachment","sessionId":"s","attachment":{"type":"queued_command","commandMode":"prompt","prompt":42}}`,
		`{"type":"user","sessionId":"s","message":{"content":[{"type":"text","text":42}]}}`,
		`{"type":"assistant","sessionId":"s","message":{"content":[{"type":"tool_use","id":"c","name":42,"input":{}}]}}`,
	} {
		result := extraction(t, &claude.Parser{}, []byte(line+"\n"), transcript.Cursor{})
		if issueCount(result.Report, transcript.FieldTypeMismatch) != 1 {
			t.Fatalf("unreported mismatch %s: %+v", line, result.Report)
		}
	}
}
func TestCodexUnknownTypesBeyondPrefix(t *testing.T) {
	p := &codex.Parser{LineOptions: transcript.LineReaderOptions{MaxLine: 64, HeadSize: 64}}
	for _, payload := range []string{`{"type":"future_event"}`, `{"type":"item_completed","item":{"type":"FutureToolResult"}}`} {
		data := []byte(`{"type":"event_msg","padding":"` + string(bytes.Repeat([]byte("x"), 128)) + `","payload":` + payload + "}\n")
		result := extraction(t, p, data, transcript.Cursor{})
		if issueCount(result.Report, transcript.UnknownRecordType) != 1 {
			t.Fatal("unknown decoded type not recorded")
		}
	}
	short := []byte(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FutureToolResult"}}}` + "\n")
	if result := extraction(t, &codex.Parser{}, short, transcript.Cursor{}); issueCount(result.Report, transcript.UnknownRecordType) != 1 {
		t.Fatal("unknown item treated as mirror")
	}
}
func TestCodexUnpairedEventsUseEffectiveCaps(t *testing.T) {
	data := []byte(`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"e","command":"` + string(bytes.Repeat([]byte("x"), 128)) + `"}}}` + "\n")
	capped := &codex.Parser{Caps: map[transcript.Kind]transcript.CapConfig{transcript.KindToolResult: {Head: 8, Tail: 8}}}
	if result := extraction(t, capped, data, transcript.Cursor{}); issueCount(result.Report, transcript.TextTruncated) != 1 {
		t.Fatal("unpaired cap unreported")
	}
	uncapped := &codex.Parser{Caps: map[transcript.Kind]transcript.CapConfig{}}
	if result := extraction(t, uncapped, data, transcript.Cursor{}); issueCount(result.Report, transcript.TextTruncated) != 0 {
		t.Fatal("uncapped event incorrectly reported")
	}
	var rows transcript.Collector
	if _, err := uncapped.Parse(context.Background(), transcript.Input{R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows.Messages) != 1 || len(rows.Messages[0].Text) != 128 {
		t.Fatal("uncapped event did not preserve text")
	}
}
