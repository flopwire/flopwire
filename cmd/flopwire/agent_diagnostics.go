package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/workmetrics"
)

func agentDiagnosticsCommand(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("agent diagnostics", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print cumulative counters as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: flopwire agent diagnostics [--json]")
	}
	dir, err := configDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := agent.Call(ctx, filepath.Join(dir, "agent.sock"), agent.Request{Op: "diagnostics"})
	if err != nil {
		return err
	}
	if resp.Diagnostics == nil {
		return errors.New("running agent does not expose work diagnostics; upgrade and restart it")
	}
	if *asJSON {
		return json.NewEncoder(w).Encode(resp.Diagnostics)
	}
	return printAgentDiagnostics(w, *resp.Diagnostics)
}

func printAgentDiagnostics(w io.Writer, d agent.DiagnosticsSnapshot) error {
	fmt.Fprintf(w, "Local daemon PID %d; agent uptime %s; process CPU total %s\n", d.PID, time.Duration(d.UptimeNS), time.Duration(d.ProcessCPUNS))
	fmt.Fprintf(w, "Process totals: %d GC cycles; %d allocated bytes\n", d.GCCyclesTotal, d.HeapAllocatedBytesTotal)
	fmt.Fprintln(w, "Cumulative wall times include waits and overlap. Compare snapshots; do not sum them as CPU.")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "OPERATION\tCALLS\tIN FLIGHT\tTOTAL WALL\tMAX WALL")
	printRows := func(prefix string, rows []workmetrics.Snapshot) {
		for _, r := range rows {
			fmt.Fprintf(tw, "%s%s\t%d\t%d\t%s\t%s\n", prefix, r.Name, r.Calls, r.InFlight, time.Duration(r.TotalWallNS), time.Duration(r.MaxWallNS))
		}
	}
	printRows("agent/", d.Operations)
	if d.Sync != nil {
		printRows("sync/", d.Sync.Operations)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if d.Sync != nil {
		_, err := fmt.Fprintf(w, "Settled turns skipped: %d\n", d.Sync.SettledTurns)
		return err
	}
	return nil
}
