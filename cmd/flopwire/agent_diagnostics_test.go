package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/flopwire/flopwire/internal/agent"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentWorkDiagnosticsCLI(t *testing.T) {
	for _, format := range []string{"json", "text", "old-agent"} {
		t.Run(format, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "fw-work-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			t.Setenv("FLOPWIRE_CONFIG", filepath.Join(dir, "config.json")) // no config/index/server is required
			ln, err := net.Listen("unix", filepath.Join(dir, "agent.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				c, e := ln.Accept()
				if e != nil {
					done <- e
					return
				}
				defer c.Close()
				var req agent.Request
				if e = json.NewDecoder(c).Decode(&req); e != nil {
					done <- e
					return
				}
				if req.Op != "diagnostics" {
					done <- context.Canceled
					return
				}
				resp := agent.Response{OK: true}
				if format != "old-agent" {
					resp.Diagnostics = &agent.DiagnosticsSnapshot{PID: 123, ProcessCPUNS: 2000000000}
				}
				done <- json.NewEncoder(c).Encode(resp)
			}()
			args := []string{}
			if format == "json" {
				args = []string{"--json"}
			}
			var out bytes.Buffer
			err = agentDiagnosticsCommand(context.Background(), args, &out)
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			if format == "old-agent" {
				if err == nil || !strings.Contains(err.Error(), "upgrade") {
					t.Fatalf("old agent: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				var d agent.DiagnosticsSnapshot
				if err = json.Unmarshal(out.Bytes(), &d); err != nil || d.PID != 123 || d.ProcessCPUNS != 2000000000 {
					t.Fatalf("snapshot: %s %v", out.String(), err)
				}
			} else if !strings.Contains(out.String(), "process CPU total 2s") || !strings.Contains(out.String(), "overlap") {
				t.Fatal(out.String())
			}
		})
	}
	if err := agentDiagnosticsCommand(context.Background(), []string{"unexpected"}, &bytes.Buffer{}); err == nil {
		t.Fatal("positional argument accepted")
	}
}
