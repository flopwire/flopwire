package digest

import (
	"encoding/json"
	"fmt"
	"github.com/flopwire/flopwire/internal/transcript"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func assertProbeStreaming(t *testing.T, messages []transcript.Message) []CommitEvidence {
	t.Helper()
	state := &CommitEvidenceState{}
	var got []CommitEvidence
	for i, m := range messages {
		saved := cloneCommitEvidenceState(state)
		delta := state.Step(m)
		replayDelta := saved.Step(m)
		if !reflect.DeepEqual(delta, replayDelta) || !reflect.DeepEqual(state, saved) {
			t.Fatalf("clone branch differs at %d", i)
		}
		got = append(got, delta...)
		want := frozenExtractCommitEvidence(messages[:i+1])
		if wrapper := ExtractCommitEvidence(messages[:i+1]); !reflect.DeepEqual(wrapper, want) {
			t.Fatalf("wrapper differs at %d", i)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stream differs after row %d\ngot %+v\nwant %+v", i, got, want)
		}
	}
	return got
}

// Serialize pointed-to values, not pointer addresses, before mutating only one
// branch. This catches ownership bugs that identical branch steps can conceal.
func probeStateSnapshot(s *CommitEvidenceState) string {
	project := func(in map[string]pendingCommitEvidence) map[string][]any {
		out := map[string][]any{}
		for k, p := range in {
			out[k] = []any{p.ref, p.printedSafe, p.g, p.operation, p.cwd, p.known, p.launchResult, p.poll, p.handle, p.reveal}
		}
		return out
	}
	// callGit has private fields; expose every field explicitly in this snapshot.
	git := func(in map[string]pendingCommitEvidence) map[string][]any {
		out := map[string][]any{}
		for k, p := range in {
			g := p.g
			out[k] = []any{g.printed, g.pwhere, g.commit, g.subject, g.where, g.mover, g.reveal, g.style, g.revWhere, g.self, g.revOK, g.ends}
		}
		return out
	}
	b, err := json.Marshal([]any{project(s.calls), project(s.handles), git(s.calls), git(s.handles), s.lastMover, s.open, s.openWhere})
	if err != nil {
		panic(err)
	}
	return string(b)
}
func TestProbeStreamCloneDivergentBranches(t *testing.T) {
	hash := "abcdef01234567890123456789012345678901234"
	prefixes := map[string][]transcript.Message{
		"pending-call":    {evidenceCall("a", "git commit -m x", 0)},
		"running-handle":  {evidenceCall("a", "git commit -m x", 0), evidenceResult("a", "Chunk ID: abc\nProcess running with session ID 73\nOutput:\n", 1)},
		"quiet-open":      {evidenceCall("a", "git commit -qm x", 0), evidenceResult("a", "", 1)},
		"captured-reveal": {evidenceCall("a", "git commit -qm x", 0), evidenceResult("a", "", 1), evidenceCall("b", "git rev-parse HEAD", 2)},
	}
	for name, prefix := range prefixes {
		t.Run(name, func(t *testing.T) {
			state := &CommitEvidenceState{}
			var events []CommitEvidence
			for _, m := range prefix {
				events = append(events, state.Step(m)...)
			}
			before := probeStateSnapshot(state)
			branch := cloneCommitEvidenceState(state)
			branchMessages := append([]transcript.Message{}, prefix...)
			branchEvents := append([]CommitEvidence(nil), events...)
			poll := evidenceCall("poll", "", 20)
			poll.ToolName = "write_stdin"
			poll.Text = `{"session_id":73}`
			for _, m := range []transcript.Message{poll, evidenceResult("poll", "[main abcdef0] alternate", 21), evidenceResult("a", "[main 1234567] alternate", 22), evidenceResult("b", hash, 23), evidenceCall("barrier", "git reset --hard HEAD~1", 24)} {
				branchMessages = append(branchMessages, m)
				branchEvents = append(branchEvents, branch.Step(m)...)
				if want := frozenExtractCommitEvidence(branchMessages); !reflect.DeepEqual(branchEvents, want) {
					t.Fatalf("divergent branch differs: got %+v want %+v", branchEvents, want)
				}
			}
			if after := probeStateSnapshot(state); after != before {
				t.Fatalf("clone mutated original: before %s after %s", before, after)
			}
			suffix := []transcript.Message{evidenceResult("a", "[main abcdef0] intended", 30), evidenceCall("b", "git rev-parse HEAD", 31), evidenceResult("b", hash, 32)}
			for _, m := range suffix {
				events = append(events, state.Step(m)...)
			}
			all := append(append([]transcript.Message{}, prefix...), suffix...)
			if want := frozenExtractCommitEvidence(all); !reflect.DeepEqual(events, want) {
				t.Fatalf("original after divergent clone differs: got %+v want %+v", events, want)
			}
		})
	}
}

func TestCommitEvidenceOutputOwnership(t *testing.T) {
	state := &CommitEvidenceState{}
	state.Step(evidenceCall("a", "git commit -m x", 0))
	state.Step(evidenceResult("a", "Chunk ID: abc\nProcess running with session ID 73\nOutput:\n", 1))
	poll := evidenceCall("poll", "", 2)
	poll.ToolName = "write_stdin"
	poll.Text = `{"session_id":73}`
	state.Step(poll)
	checkpoint := cloneCommitEvidenceState(state)
	before := probeStateSnapshot(checkpoint)
	events := state.Step(evidenceResult("poll", "[main abcdef0] x", 3))
	if len(events) != 1 || events[0].RevealCommand == nil || events[0].RevealResult == nil {
		t.Fatalf("missing async refs: %+v", events)
	}
	events[0].RevealCommand.NativeID = "mutated"
	events[0].RevealResult.Ordinal = 999
	if after := probeStateSnapshot(checkpoint); after != before {
		t.Fatal("returned evidence aliases pending state")
	}
}

func TestCommitEvidencePendingBudget(t *testing.T) {
	var state CommitEvidenceState
	initial := state.RetainedSize()
	const count = 1024
	for i := 0; i < count; i++ {
		state.Step(evidenceCall(fmt.Sprint(i), "git commit -qm x", int64(i)))
	}
	pending := state.RetainedSize()
	if len(state.calls) != count || pending < int64(count)*2048 {
		t.Fatalf("pending estimate: %d, entries: %d", pending, len(state.calls))
	}
	for i := 0; i < count; i++ {
		state.Step(evidenceResult(fmt.Sprint(i), "", int64(count+i)))
	}
	drained := state.RetainedSize()
	if len(state.calls) != 0 || drained <= initial || drained > pending {
		t.Fatalf("map high water estimate: initial=%d pending=%d drained=%d", initial, pending, drained)
	}
	clone := cloneCommitEvidenceState(&state)
	if clone.RetainedSize() != drained {
		t.Fatal("clone lost conservative accounting")
	}
	long := strings.Repeat("x", 1<<20)
	m := evidenceCall(long, "git -C /"+long+" commit -qm x", 10000)
	m.NativeID, m.Locator = long, long
	before := state.RetainedSize()
	state.Step(m)
	if delta := state.RetainedSize() - before; delta < 4*int64(len(long)) {
		t.Fatalf("long pending strings undercounted: %d", delta)
	}
	t.Logf("pending state estimate: %d calls=%d bytes; drained=%d bytes; long-string delta=%d bytes", count, pending, drained, state.RetainedSize()-before)
}

func TestCommitEvidencePendingHandlesBudget(t *testing.T) {
	var state CommitEvidenceState
	const count = 256
	for i := 0; i < count; i++ {
		id := fmt.Sprint(i)
		state.Step(evidenceCall(id, "git commit -qm x", int64(i*2)))
		state.Step(evidenceResult(id, fmt.Sprintf("Chunk ID: abc\nProcess running with session ID %d\nOutput:\n", i), int64(i*2+1)))
	}
	if len(state.calls) != 0 || len(state.handles) != count {
		t.Fatalf("unexpected pending calls=%d handles=%d", len(state.calls), len(state.handles))
	}
	pending := state.RetainedSize()
	for i := 0; i < count; i++ {
		poll := evidenceCall("poll"+fmt.Sprint(i), "", int64(1000+i*2))
		poll.ToolName = "write_stdin"
		poll.Text = fmt.Sprintf(`{"session_id":%d}`, i)
		state.Step(poll)
		state.Step(evidenceResult(poll.ToolCallID, "", int64(1001+i*2)))
	}
	if len(state.calls)+len(state.handles) != 0 {
		t.Fatal("completed handles retained")
	}
	if state.RetainedSize() > pending {
		t.Fatal("drained handle accounting grew")
	}
	t.Logf("pending handles: %d estimated bytes; drained: %d bytes", pending, state.RetainedSize())
}

func TestCommitEvidenceCloneConcurrentBranches(t *testing.T) {
	var state CommitEvidenceState
	state.Step(evidenceCall("a", "git commit -m x", 0))
	branches := []*CommitEvidenceState{cloneCommitEvidenceState(&state), cloneCommitEvidenceState(&state)}
	var wg sync.WaitGroup
	for i, branch := range branches {
		wg.Add(1)
		go func(i int, branch *CommitEvidenceState) {
			defer wg.Done()
			result := evidenceResult("a", fmt.Sprintf("[main abcdef%d] branch", i), 1)
			got := branch.Step(result)
			if want := frozenExtractCommitEvidence([]transcript.Message{evidenceCall("a", "git commit -m x", 0), result}); !reflect.DeepEqual(got, want) {
				t.Errorf("branch differs: got %+v want %+v", got, want)
			}
		}(i, branch)
	}
	wg.Wait()
	if len(state.calls) != 1 {
		t.Fatal("branches changed original pending call")
	}
}

// Internal pointer targets are immutable. Clone maps to qualify independently
// evolving branches without widening the production API for speculative reuse.
func cloneCommitEvidenceState(s *CommitEvidenceState) *CommitEvidenceState {
	out := *s
	out.calls = maps.Clone(s.calls)
	out.handles = maps.Clone(s.handles)
	return &out
}

func BenchmarkCommitEvidenceReplay(b *testing.B) {
	for _, workload := range []struct{ name, cmd, output string }{
		{"ordinary", "go test ./internal/digest", "ok"},
		{"git-status", "git status --short", ""},
		{"git-commit", "git commit -m x", "[main abcdef0] x"},
	} {
		rows := make([]transcript.Message, 0, 1000)
		for i := 0; i < 500; i++ {
			id := fmt.Sprint(i)
			rows = append(rows, evidenceCall(id, workload.cmd, int64(i*2)), evidenceResult(id, workload.output, int64(i*2+1)))
		}
		for _, implementation := range []struct {
			name    string
			extract func([]transcript.Message) []CommitEvidence
		}{
			{"frozen", frozenExtractCommitEvidence}, {"stream", ExtractCommitEvidence},
		} {
			b.Run(workload.name+"/"+implementation.name, func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					implementation.extract(rows)
				}
			})
		}
	}
}
