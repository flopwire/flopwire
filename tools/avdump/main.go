//go:build ignore

// avdump prints agentsview's parse of a provider root as JSONL (one
// oracle.AVSession per line). It is built inside an agentsview clone
// (kenn-io/agentsview, MIT) by scripts/oracle-sample.sh, never by this
// module: it imports agentsview's internal parser package.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go.kenn.io/agentsview/internal/parser"
)

type msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	TS      int64  `json:"ts"`
	System  bool   `json:"system"`
	Tools   int    `json:"tool_calls"`
	Results int    `json:"tool_results"`
}

func main() {
	agent := flag.String("agent", "", "claude|codex|devin")
	root := flag.String("root", "", "provider root")
	flag.Parse()
	ctx := context.Background()
	enc := json.NewEncoder(os.Stdout)
	for _, f := range parser.ProviderFactories() {
		if string(f.Definition().Type) != *agent {
			continue
		}
		p := f.NewProvider(parser.ProviderConfig{Roots: []string{*root}, Machine: "local"})
		refs, err := p.Discover(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "discover:", err)
			os.Exit(1)
		}
		for _, ref := range refs {
			fp, err := p.Fingerprint(ctx, ref)
			if err != nil {
				fmt.Fprintln(os.Stderr, "fingerprint:", err)
				continue
			}
			out, err := p.Parse(ctx, parser.ParseRequest{Source: ref, Fingerprint: fp, Machine: "local", ForceParse: true})
			if err != nil {
				fmt.Fprintln(os.Stderr, "parse:", err)
				continue
			}
			for _, r := range out.Results {
				var ms []msg
				for _, m := range r.Result.Messages {
					ms = append(ms, msg{Role: string(m.Role), Content: m.Content, TS: m.Timestamp.UnixMilli(), System: m.IsSystem, Tools: len(m.ToolCalls), Results: len(m.ToolResults)})
				}
				enc.Encode(map[string]any{"session_id": r.Result.Session.ID, "path": r.Result.Session.File.Path, "messages": ms})
			}
		}
	}
}
