package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/retrieval/local"
	"github.com/flopwire/flopwire/internal/transcript"
)

func diagnosticsCommand(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	server := fs.Bool("server", false, "read team server diagnostics")
	source := fs.String("source", "", "inspect one source ID")
	index := fs.String("index", "", "local index path")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: flopwire diagnostics [--server] [--source ID] [--index PATH] [--json]")
	}
	var summary *transcript.ExtractionSummary
	var detail *transcript.SourceDiagnostics
	var err error
	if *server {
		c, e := serverClient()
		if e != nil {
			return e
		}
		if *source == "" {
			summary = &transcript.ExtractionSummary{}
			err = c.JSON(ctx, http.MethodGet, "/v1/diagnostics", nil, summary)
		} else {
			detail = &transcript.SourceDiagnostics{}
			err = c.JSON(ctx, http.MethodGet, "/v1/diagnostics?source_id="+url.QueryEscape(*source), nil, detail)
		}
	} else {
		if *index == "" {
			*index = local.IndexPath()
		}
		if _, e := os.Stat(*index); e != nil {
			return e
		}
		s, e := localindex.Open(*index, localindex.Options{ReadOnly: true})
		if e != nil {
			return e
		}
		defer s.Close()
		if *source == "" {
			summary, err = s.ExtractionSummary(ctx)
		} else {
			detail, err = s.SourceDiagnostics(ctx, *source)
		}
	}
	if err != nil {
		return err
	}
	if *asJSON {
		var out any = summary
		if detail != nil {
			out = detail
		}
		return json.NewEncoder(w).Encode(out)
	}
	if summary != nil {
		printExtractionSummary(w, summary)
		for _, ref := range summary.Affected {
			fmt.Fprintf(w, "  source %s (%s): %s\n", ref.SourceID, ref.Agent, ref.Path)
		}
		if int64(len(summary.Affected)) < summary.AffectedSources {
			fmt.Fprintln(w, "  Showing at most 20 affected sources.")
		}
		return nil
	}
	fmt.Fprintf(w, "source %s (%s): %s\n", detail.SourceID, detail.Agent, detail.Path)
	if cp := detail.Extraction; cp == nil {
		fmt.Fprintln(w, "extraction: unassessed")
	} else {
		fmt.Fprintf(w, "extraction: generation %d, contract %s, consumed %d bytes / %d lines\n", cp.Generation, cp.Contract, cp.Offset, cp.LineNo)
		if len(cp.Report.Issues) == 0 {
			fmt.Fprintln(w, "issues: none observed")
		}
		for _, issue := range cp.Report.Issues {
			fmt.Fprintf(w, "  %s (%s): %d\n", issue.Code, issue.Code.Severity(), issue.Count)
			for _, loc := range issue.Samples {
				fmt.Fprintf(w, "    line %d, byte %d\n", loc.Line, loc.ByteOffset)
			}
		}
	}
	fmt.Fprintf(w, "indexed companion outputs: %d missing, %d truncated\n", detail.MissingCompanions, detail.TruncatedCompanions)
	return nil
}
func printExtractionSummary(w io.Writer, s *transcript.ExtractionSummary) {
	fmt.Fprintf(w, "extraction: %d assessed, %d unassessed; %d with warnings, %d with informational issues\n", s.AssessedSources, s.UnassessedSources, s.WarningSources, s.InfoSources)
	codes := make([]string, 0, len(s.Counts))
	for code := range s.Counts {
		codes = append(codes, string(code))
	}
	sort.Strings(codes)
	for _, code := range codes {
		fmt.Fprintf(w, "  %s: %d\n", code, s.Counts[transcript.DiagnosticCode(code)])
	}
}
