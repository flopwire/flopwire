// Token usage deduplicated by message.id.
//
// Rule ported from entireio/cli cmd/entire/cli/agent/claudecode/transcript.go
// CalculateTokenUsage at commit 2dbea8bff1db7188158420b1c863dfa2d9cd7b4a
// (MIT): Claude writes one line per content block of a streamed API
// message, every line repeats the message's usage, and only the line with
// the highest output_tokens holds the final count. No code copied.

package claude

import "github.com/flopwire/flopwire/internal/transcript"

// Usage is summed token usage over distinct API messages.
type Usage struct {
	APICalls            int
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
}

// SumUsage totals the usage recorded on assistant rows, counting each
// message_id once with its highest output_tokens. Rows without a
// message_id are counted individually.
func SumUsage(msgs []*transcript.Message) Usage {
	best := map[string]map[string]int64{}
	var anon []map[string]int64
	for _, m := range msgs {
		u, ok := m.Enrichment["usage"].(map[string]int64)
		if !ok {
			continue
		}
		id, _ := m.Enrichment["message_id"].(string)
		if id == "" {
			anon = append(anon, u)
			continue
		}
		if prev, ok := best[id]; !ok || u["output_tokens"] > prev["output_tokens"] {
			best[id] = u
		}
	}
	var t Usage
	add := func(u map[string]int64) {
		t.APICalls++
		t.InputTokens += u["input_tokens"]
		t.OutputTokens += u["output_tokens"]
		t.CacheCreationTokens += u["cache_creation_input_tokens"]
		t.CacheReadTokens += u["cache_read_input_tokens"]
	}
	for _, u := range best {
		add(u)
	}
	for _, u := range anon {
		add(u)
	}
	return t
}
