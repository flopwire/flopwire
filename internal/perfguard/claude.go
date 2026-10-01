package perfguard

import (
	"encoding/json"
	"fmt"
	"time"
)

// ClaudeSession generates a deterministic synthetic Claude Code transcript
// (projects/<dir>/<SessionID>.jsonl). Record i is one parsed message; the
// records cycle user prompt, assistant text, assistant tool_use, user
// tool_result. Lines(0, n) equals Lines(0, m) followed by Lines(m, n), so
// appends can be modeled by writing a prefix and then the rest.
type ClaudeSession struct {
	SessionID string    // default "5e550000-0000-4000-8000-000000000001"
	Cwd       string    // default "/workspace/perf"
	Start     time.Time // default 2026-09-01T00:00:00Z; record i is Start + i seconds
}

// ClaudeTranscript returns n records of the default ClaudeSession.
func ClaudeTranscript(n int) []byte { return ClaudeSession{}.Lines(0, n) }

func (s ClaudeSession) withDefaults() ClaudeSession {
	if s.SessionID == "" {
		s.SessionID = "5e550000-0000-4000-8000-000000000001"
	}
	if s.Cwd == "" {
		s.Cwd = "/workspace/perf"
	}
	if s.Start.IsZero() {
		s.Start = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	}
	return s
}

// UUID is the uuid of record i.
func (s ClaudeSession) UUID(i int) string {
	return fmt.Sprintf("5e550000-0000-4000-9000-%012d", i)
}

var words = []string{"amber", "otter", "retry", "upload", "index", "cursor", "digest", "parser",
	"bucket", "schema", "token", "window", "sync", "chunk", "ledger", "branch", "kernel", "harbor"}

// text is deterministic prose of ~20 words that differs per record.
func text(i int) string {
	b := make([]byte, 0, 160)
	b = fmt.Appendf(b, "step %d:", i)
	x := uint32(i)*2654435761 + 1
	for range 20 {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b = append(b, ' ')
		b = append(b, words[x%uint32(len(words))]...)
	}
	return string(b)
}

// Lines returns records [from, to) as JSONL, one record per line.
func (s ClaudeSession) Lines(from, to int) []byte {
	s = s.withDefaults()
	var out []byte
	for i := from; i < to; i++ {
		rec := map[string]any{
			"isSidechain": false,
			"userType":    "external",
			"cwd":         s.Cwd,
			"sessionId":   s.SessionID,
			"version":     "2.1.0",
			"gitBranch":   "main",
			"uuid":        s.UUID(i),
			"timestamp":   s.Start.Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05.000Z"),
		}
		if i > 0 {
			rec["parentUuid"] = s.UUID(i - 1)
		} else {
			rec["parentUuid"] = nil
		}
		usage := map[string]any{"input_tokens": 100, "output_tokens": 30, "cache_read_input_tokens": 50, "cache_creation_input_tokens": 5}
		switch i % 4 {
		case 0:
			rec["type"] = "user"
			rec["message"] = map[string]any{"role": "user", "content": text(i)}
		case 1:
			rec["type"] = "assistant"
			rec["requestId"] = fmt.Sprintf("req_%d", i)
			rec["message"] = map[string]any{"id": fmt.Sprintf("msg_%d", i), "type": "message", "role": "assistant", "model": "claude-opus-4-1",
				"content": []any{map[string]any{"type": "text", "text": text(i)}}, "stop_reason": "end_turn", "usage": usage}
		case 2:
			rec["type"] = "assistant"
			rec["requestId"] = fmt.Sprintf("req_%d", i)
			rec["message"] = map[string]any{"id": fmt.Sprintf("msg_%d", i), "type": "message", "role": "assistant", "model": "claude-opus-4-1",
				"content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", i), "name": "Grep",
					"input": map[string]any{"pattern": words[i%len(words)], "path": "src"}}}, "stop_reason": "tool_use", "usage": usage}
		case 3:
			rec["type"] = "user"
			rec["message"] = map[string]any{"role": "user", "content": []any{map[string]any{
				"tool_use_id": fmt.Sprintf("toolu_%d", i-1), "type": "tool_result", "content": "src/file.go:1: " + text(i)}}}
		}
		line, err := json.Marshal(rec)
		if err != nil {
			panic(err)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out
}
