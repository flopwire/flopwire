package format

import (
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/coverage"
)

func TestDetailActionDoesNotRequestSameDiagnostic(t *testing.T) {
	s := &Scope{Kind: "shared", CoverageMode: coverage.SummaryMode, Coverage: &coverage.Report{
		Upload: &coverage.UploadSnapshot{Captured: &coverage.CapturedSnapshot{LostGenerations: 2}},
	}}
	p := &Page{Scope: s, Hits: []Hit{{Address: "session/1"}}}
	if got := PageAction(p); !strings.Contains(got, "coverage=detail") {
		t.Fatalf("summary should offer optional detail for recorded loss: %s", got)
	}
	s.CoverageMode = coverage.DetailMode
	if got := PageAction(p); strings.Contains(got, "coverage=detail") || !strings.Contains(got, "Read a relevant") {
		t.Fatalf("full diagnostic should return to evidence navigation: %s", got)
	}
	if s.Coverage.Upload.Captured.LostGenerations != 2 {
		t.Fatal("action changed observed loss")
	}
}
