package claude

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	"github.com/flopwire/flopwire/internal/perfguard"
	"github.com/flopwire/flopwire/internal/transcript"
)

type allocSink struct{ n int }

func (s *allocSink) Conversation(*transcript.Conversation) error { return nil }
func (s *allocSink) Message(*transcript.Message) error           { s.n++; return nil }

// Parsing allocates a bounded amount per message, at any session length:
// no per-record cost that grows with what came before (perf-guards.md
// decision 9). Measured: about 23 allocations and 1.6KB per message.
func TestParseAllocsPerMessage(t *testing.T) {
	if perfguard.Race {
		t.Skip("the race detector inflates allocations")
	}
	const maxAllocs, maxBytes = 40, 4 << 10
	for _, n := range []int{500, 4000} {
		data := perfguard.ClaudeTranscript(n)
		src := transcript.Source{Agent: transcript.AgentClaude, Path: "/home/u/.claude/projects/-w/" + perfguard.ClaudeSession{}.UUID(0) + ".jsonl",
			StorageKind: transcript.StorageJSONLAppend, Parser: ParserName}
		sink := &allocSink{}
		parse := func() {
			sink.n = 0
			if _, err := (&Parser{}).Parse(context.Background(), transcript.Input{Source: &src, R: bytes.NewReader(data), Size: int64(len(data))}, transcript.Cursor{}, sink); err != nil {
				t.Fatal(err)
			}
		}
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		allocs := testing.AllocsPerRun(2, parse) // three parses: a warm-up and two measured
		runtime.ReadMemStats(&m1)
		if sink.n != n {
			t.Fatalf("parsed %d messages, want %d", sink.n, n)
		}
		perMsg, bytesPerMsg := allocs/float64(n), (m1.TotalAlloc-m0.TotalAlloc)/3/uint64(n)
		t.Logf("n=%d: %.1f allocations, %d bytes per message", n, perMsg, bytesPerMsg)
		if perMsg > maxAllocs || bytesPerMsg > maxBytes {
			t.Errorf("n=%d: %.1f allocations and %d bytes per message, bound %d and %d", n, perMsg, bytesPerMsg, maxAllocs, maxBytes)
		}
	}
}
