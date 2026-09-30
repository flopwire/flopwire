package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

// TestOracleCorpusSample: see the Claude package's test of the same name.
func TestOracleCorpusSample(t *testing.T) {
	dir := os.Getenv(oracle.SampleDirEnv)
	if dir == "" {
		t.Skip("set " + oracle.SampleDirEnv + " (scripts/oracle-sample.sh)")
	}
	files, err := oracle.Load(filepath.Join(dir, "expected"))
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "codex")
	rep := oracle.NewReport("codex")
	av, _ := oracle.LoadAgentsview(filepath.Join(dir, "agentsview", "codex.jsonl"))
	avRep := oracle.NewReport("agentsview-codex")
	for _, f := range files {
		rep.Files++
		path := oracle.RealPath(filepath.Join(dir, "home"), f)
		data, err := os.ReadFile(path)
		if err != nil {
			rep.Fail(f.Name, err)
			continue
		}
		var c transcript.Collector
		src := &transcript.Source{Agent: transcript.AgentCodex, Path: path}
		if _, err := (&Parser{Caps: uncapped}).Parse(context.Background(), transcript.Input{Source: src, R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, &c); err != nil {
			rep.Fail(f.Name, err)
			continue
		}
		if len(c.Conversations) == 0 {
			rep.Fail(f.Name, os.ErrNotExist)
			continue
		}
		conv := lastConv(&c)
		var hso int64
		if v, ok := conv.Extra["subagent_history_start_ordinal"].(int64); ok {
			hso = v
		}
		rules := codexRules(hso)
		rules.Normalize = func(s string) string { return unwrapExecOutput(decodeStringified(s)) }
		rules.Divergences = append(rules.Divergences, oracle.Divergence{
			Name: "FAD drops a tool output that is an empty string; Flopwire keeps every tool output as a row (D19)",
			DropOurs: func(m *transcript.Message) bool {
				if m.Kind != transcript.KindToolResult || m.Text != "" || m.ByteOffset+m.ByteLen > int64(len(data)) {
					return false
				}
				var ln line[struct {
					Output json.RawMessage `json:"output"`
				}]
				return json.Unmarshal(bytes.TrimSpace(data[m.ByteOffset:m.ByteOffset+m.ByteLen]), &ln) == nil && string(ln.Payload.Output) == `""`
			},
		})
		for _, exp := range f.Conversations {
			rep.Add(f.Name, oracle.Compare(exp, conv, upsert(c.Messages), rules))
		}
		if s, ok := av[conv.SessionID]; ok {
			avRep.Files++
			cmp, n := oracle.CompareAgentsview(s, upsert(c.Messages), nil)
			avRep.Normalized += n
			avRep.Add(f.Name, cmp)
		}
	}
	if avRep.Files > 0 {
		if err := avRep.Write(dir); err != nil {
			t.Fatal(err)
		}
		t.Log(avRep.Summary())
	}
	if err := rep.Write(dir); err != nil {
		t.Fatal(err)
	}
	t.Log(rep.Summary())
	if rep.ParseErrors > 0 {
		t.Errorf("%d parse errors", rep.ParseErrors)
	}
}

// uncapped parses without the storage caps: the cap is an index policy,
// and FAD keeps whole texts.
var uncapped = map[transcript.Kind]transcript.CapConfig{}

// decodeStringified (P6) is applied to both sides: FAD indexes a tool
// output that is JSON written into a string as that JSON; Flopwire indexes
// its text. Documented divergence, normalized here.
//
// unwrapExecOutput: 2025 rollouts store a function_call_output as a JSON
// string {"output": ..., "metadata": {...}}. FAD indexes the envelope;
// Flopwire indexes the output text. Documented divergence, normalized here.
func unwrapExecOutput(s string) string {
	if strings.HasPrefix(s, `{"output":`) {
		var env struct {
			Output string `json:"output"`
		}
		if json.Unmarshal([]byte(s), &env) == nil {
			s = env.Output
		}
	}
	return oracle.CollapseSpace(s)
}
