package claude

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

// TestOracleCorpusSample diffs this parser against FAD 0.3.1 on a sample
// of real transcripts (scripts/oracle-sample.sh prepares it) and writes a
// report of match rates and divergence categories. It fails only on parse
// errors: divergences are measured, then documented.
func TestOracleCorpusSample(t *testing.T) {
	dir := os.Getenv(oracle.SampleDirEnv)
	if dir == "" {
		t.Skip("set " + oracle.SampleDirEnv + " (scripts/oracle-sample.sh)")
	}
	files, err := oracle.Load(filepath.Join(dir, "expected"))
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "claude")
	rep := oracle.NewReport("claude")
	av, _ := oracle.LoadAgentsview(filepath.Join(dir, "agentsview", "claude.jsonl"))
	avRep := oracle.NewReport("agentsview-claude")
	for _, f := range files {
		rep.Files++
		path := oracle.RealPath(filepath.Join(dir, "home"), f)
		b, err := os.ReadFile(path)
		if err != nil {
			rep.Fail(f.Name, err)
			continue
		}
		src := newSource(path, "")
		var c transcript.Collector
		if _, err := (&Parser{Caps: uncapped}).Parse(context.Background(), transcript.Input{Source: &src, R: bytes.NewReader(b), Size: int64(len(b))}, transcript.Cursor{}, &c); err != nil {
			rep.Fail(f.Name, err)
			continue
		}
		if len(c.Conversations) == 0 {
			rep.Fail(f.Name, os.ErrNotExist)
			continue
		}
		conv := c.Conversations[len(c.Conversations)-1]
		for _, exp := range f.Conversations {
			rep.Add(f.Name, oracle.Compare(exp, conv, fadView(c.Messages), claudeRules()))
		}
		if s, ok := av[conv.SessionID]; ok {
			avRep.Files++
			cmp, n := oracle.CompareAgentsview(s, c.Messages, nil)
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
