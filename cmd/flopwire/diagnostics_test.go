package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"github.com/flopwire/flopwire/internal/agent"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestDiagnosticsCLIJSONAndHuman(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := localindex.Open(path, localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSource(context.Background(), transcript.Source{Agent: transcript.AgentClaude, Path: "/synthetic.jsonl", StorageKind: transcript.StorageJSONLAppend, Parser: "claude@2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := diagnosticsCommand(context.Background(), []string{"--index", path, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var summary transcript.ExtractionSummary
	if err := json.Unmarshal(out.Bytes(), &summary); err != nil || summary.UnassessedSources != 1 {
		t.Fatalf("JSON %s %v", out.String(), err)
	}
	out.Reset()
	if err := diagnosticsCommand(context.Background(), []string{"--index", path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "0 assessed, 1 unassessed") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := diagnosticsCommand(context.Background(), []string{"--index", path, "--source", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "unassessed") || !strings.Contains(out.String(), "synthetic.jsonl") {
		t.Fatal(out.String())
	}
	if err := diagnosticsCommand(context.Background(), []string{"--index", path, "unexpected"}, &out); err == nil {
		t.Fatal("positional argument accepted")
	}
}

// JSON status must remain parseable when human status includes credentials.
func TestAgentStatusJSONIsOneDocument(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "flopwire-status-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json"))
	ln, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		_, err = bufio.NewReader(c).ReadBytes('\n')
		if err != nil {
			done <- err
			return
		}
		done <- json.NewEncoder(c).Encode(agent.Response{OK: true, Extraction: &transcript.ExtractionSummary{AssessedSources: 3}})
	}()
	var out bytes.Buffer
	if err := agentStatusOutput(context.Background(), &out, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	var response agent.Response
	if err := decoder.Decode(&response); err != nil || response.Extraction == nil || response.Extraction.AssessedSources != 3 {
		t.Fatalf("status %+v %v", response, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("extra status output: %v", err)
	}
}
