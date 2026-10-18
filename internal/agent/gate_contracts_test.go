package agent

import (
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
)

type countingContractParser struct {
	transcript.ReportingParser
	calls int
}

func (p *countingContractParser) ExtractionContract() string {
	p.calls++
	return p.ReportingParser.ExtractionContract()
}

// A Parser may be a value with an uncomparable field, including one hidden
// behind an interface in an otherwise comparable struct type.
type valueContractParser struct {
	transcript.Parser
	state any
	calls *int
}

func (p valueContractParser) Name() string {
	(*p.calls)++
	return p.Parser.Name()
}

func TestGateContractsReuseAndFallback(t *testing.T) {
	p := &countingContractParser{ReportingParser: &claude.Parser{}}
	target := &target{kind: kindTranscript, parser: p, indexedWith: indexingVersion(p)}
	p.calls = 0
	var pass gateContracts
	for range 4 {
		if target.staleWithContracts(&pass) {
			t.Fatal("unchanged contract became stale")
		}
	}
	if p.calls != 1 {
		t.Fatalf("contract evaluated %d times in one pass, want 1", p.calls)
	}
	other := &countingContractParser{ReportingParser: &claude.Parser{MaxPersisted: 1}}
	if pass.currentVersion(other) == pass.currentVersion(p) || other.calls != 1 {
		t.Fatal("distinct parser instance reused another instance's contract")
	}
	var next gateContracts
	target.staleWithContracts(&next)
	if p.calls != 2 {
		t.Fatal("next pass reused the preceding pass's contract")
	}
	calls := 0
	custom := valueContractParser{Parser: &claude.Parser{}, state: []int{1}, calls: &calls}
	target.parser, target.indexedWith = custom, transcript.ReparseKey(custom.Name())
	calls = 0
	for range 4 {
		if target.staleWithContracts(&pass) {
			t.Fatal("custom parser fallback changed version semantics")
		}
	}
	if calls != 4 {
		t.Fatalf("uncomparable parser evaluated %d times, want 4", calls)
	}
}

func TestGateContractsMutablePolicyAndVersions(t *testing.T) {
	p := &claude.Parser{}
	target := &target{kind: kindTranscript, parser: p, indexedWith: indexingVersion(p)}
	check := func(want bool) {
		t.Helper()
		var pass gateContracts
		for range 2 {
			if got := target.staleWithContracts(&pass); got != want {
				t.Fatalf("stale=%v, want %v", got, want)
			}
		}
	}
	check(false)
	p.Caps = transcript.DefaultCaps // explicit equivalent configuration
	check(false)
	p.Caps = map[transcript.Kind]transcript.CapConfig{}
	check(true)
	target.reparseFailed = indexingVersion(p)
	check(false) // unchanged failed contract remains suppressed
	p.MaxPersisted = 1
	check(true) // a different contract must not inherit failure suppression
	p.Caps, p.MaxPersisted = nil, 0
	check(false)

	v := &versioned{Parser: p, name: "synthetic@1.0"}
	target.parser, target.indexedWith, target.reparseFailed = v, "synthetic@1.0", ""
	check(false)
	v.name = "synthetic@1.1"
	check(false)
	v.name = "synthetic@2.0"
	check(true)
	target.reparseFailed = "synthetic@2"
	check(false)
	v.name = "synthetic@3.0"
	check(true)
}

func TestFastLaneReevaluatesContractNextTick(t *testing.T) {
	f := newFixture(t, "-")
	waitRacy()
	f.once() // real parser contracts and settled identities from indexing
	f.a.fastLane(ctx)
	if f.a.background.len() != 0 || f.a.urgent.len() != 0 || f.a.normal.len() != 0 {
		t.Fatal("settled fast lane queued work")
	}
	parser, ok := f.a.claude.(*claude.Parser)
	if !ok {
		t.Fatalf("fixture Claude parser is %T", f.a.claude)
	}
	parser.Caps = map[transcript.Kind]transcript.CapConfig{}
	f.a.fastLane(ctx)
	want := 0
	for _, target := range f.a.targets {
		if target.parser == f.a.claude && target.kind == kindTranscript &&
			(target.hotUntil.After(time.Now()) || target.seen.CTime > time.Now().Add(-f.a.cfg.HotWindow).UnixNano()) {
			want++
			if !f.a.background.has(target) || !target.inBackground {
				t.Fatal("changed extraction contract was not queued in background")
			}
		}
	}
	if want == 0 || f.a.background.len() != want || f.a.urgent.len() != 0 || f.a.normal.len() != 0 {
		t.Fatalf("contract change queued background=%d, want %d only", f.a.background.len(), want)
	}
}
