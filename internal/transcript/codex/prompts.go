// User prompt dedupe between the two streams Codex writes a prompt to.
//
// Ported by hand from franken-agent-detection 0.3.1
// src/connectors/codex/user_prompts.rs (git c06d1cb14e0edb1cd82f1c58021d86b6beacd2de,
// MIT, see third_party/fad). The pairing rule is FAD's: only neighbouring
// physical records from opposite streams pair, each record pairs at most
// once, the trimmed texts must be equal, known timestamps must be within one
// second, and turn ids must be equal.
//
// Difference: FAD buffers the whole file and always drops the event copy.
// This parser streams, so it keeps whichever copy came first and drops the
// second. In every one of the 102 rollouts on the reference machine that
// carry both copies, the response_item comes first, so the kept row is the
// same one FAD keeps.

package codex

import (
	"crypto/sha256"
	"strings"
)

const maxPairDeltaMS = 1000

type stream uint8

const (
	streamResponse stream = 1
	streamEvent    stream = 2
)

// promptMark is the last user prompt record, in cursor state.
type promptMark struct {
	Stream stream  `json:"s"`
	LineNo int64   `json:"l"`
	Sum    [8]byte `json:"h"`
	TSms   *int64  `json:"t,omitempty"`
	TurnID string  `json:"u,omitempty"`
}

func promptSum(text string) [8]byte {
	full := sha256.Sum256([]byte(strings.TrimSpace(text)))
	var s [8]byte
	copy(s[:], full[:8])
	return s
}

// observe reports whether the prompt at lineNo duplicates the previous
// prompt record and must be dropped. It updates *last either way.
func observePrompt(last **promptMark, s stream, lineNo int64, text string, tsMS *int64, turnID string) (duplicate bool) {
	cur := &promptMark{Stream: s, LineNo: lineNo, Sum: promptSum(text), TSms: tsMS, TurnID: turnID}
	prev := *last
	if prev != nil && prev.Stream != s && prev.LineNo+1 == lineNo && prev.Sum == cur.Sum &&
		timesCompatible(prev.TSms, tsMS) && prev.TurnID == turnID {
		*last = nil // consume both sides
		return true
	}
	*last = cur
	return false
}

func timesCompatible(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	d := *a - *b
	if d < 0 {
		d = -d
	}
	return d <= maxPairDeltaMS
}
