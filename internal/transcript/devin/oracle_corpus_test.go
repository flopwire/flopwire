package devin

import (
	"context"
	"hash/fnv"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/oracle"
)

// TestOracleCorpusSample: see the Claude package's test of the same name.
// FAD emits every visible session of the copied sessions.db; about 50 are
// sampled by a hash of the session id, so the sample is stable.
func TestOracleCorpusSample(t *testing.T) {
	dir := os.Getenv(oracle.SampleDirEnv)
	if dir == "" {
		t.Skip("set " + oracle.SampleDirEnv + " (scripts/oracle-sample.sh)")
	}
	files, err := oracle.Load(filepath.Join(dir, "expected"))
	if err != nil {
		t.Fatal(err)
	}
	files = oracle.ForConnector(files, "devin")
	rep := oracle.NewReport("devin")
	av, _ := oracle.LoadAgentsview(filepath.Join(dir, "agentsview", "devin.jsonl"))
	avRep := oracle.NewReport("agentsview-devin")
	if len(files) == 0 {
		t.Skip("no devin expectations")
	}
	var total int
	for _, f := range files {
		total += len(f.Conversations)
	}
	keep := func(id string) bool {
		h := fnv.New32a()
		h.Write([]byte(id))
		return total <= 50 || int(h.Sum32()%uint32(total)) < 50
	}
	var c *transcript.Collector
	for _, f := range files {
		rep.Files++
		if c == nil {
			path := oracle.RealPath(filepath.Join(dir, "home"), f)
			c = &transcript.Collector{}
			p := &Parser{Caps: map[transcript.Kind]transcript.CapConfig{}}
			src := &transcript.Source{Agent: transcript.AgentDevin, Path: path, StorageKind: transcript.StorageSQLite, Parser: p.Name()}
			if _, err := p.Parse(context.Background(), transcript.Input{Source: src}, transcript.Cursor{}, c); err != nil {
				rep.Fail(f.Name, err)
				break
			}
		}
		convs := map[string]*transcript.Conversation{}
		for _, cv := range c.Conversations {
			convs[cv.SessionID] = cv
		}
		for _, exp := range f.Conversations {
			if exp.ExternalID == nil || !keep(*exp.ExternalID) {
				continue
			}
			cv := convs[*exp.ExternalID]
			if cv == nil {
				rep.Missing(f.Name, *exp.ExternalID)
				continue
			}
			rep.Add(f.Name, oracle.Compare(exp, cv, fadView(c.MessagesFor(cv.SessionID)), oracle.Rules{}))
			if s, ok := av[cv.SessionID]; ok {
				avRep.Files++
				cmp, n := oracle.CompareAgentsview(s, mainChain(c.MessagesFor(cv.SessionID)), nil)
				avRep.Normalized += n
				avRep.Add(f.Name, cmp)
			}
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

// mainChain keeps the main-chain rows, as agentsview shows them.
func mainChain(msgs []*transcript.Message) []*transcript.Message {
	var out []*transcript.Message
	for _, m := range msgs {
		if m.OnActivePath != nil && *m.OnActivePath {
			out = append(out, m)
		}
	}
	return out
}
