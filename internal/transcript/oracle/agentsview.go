package oracle

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// agentsview as a second oracle on the real-corpus sample. agentsview
// (kenn-io/agentsview, MIT, commit 563023de1d7b7f5af50ad5967c101341a44a2bfc)
// needs Go 1.27 and cgo-backed stores, so it is never linked here: a
// scratch driver built from a clone of it prints its parse of the sample
// home as JSONL (AVSession per line; see scripts/oracle-sample.sh), and the
// parser tests compare against that file.
//
// agentsview folds tool calls into assistant text and tool results into
// user turns, so the comparison covers what both models share: the
// sequence of user prompts, and the number of tool calls.

// AVSession is one session of the driver's output.
type AVSession struct {
	SessionID string      `json:"session_id"`
	Path      string      `json:"path"`
	Messages  []AVMessage `json:"messages"`
}

// AVMessage is one agentsview message.
type AVMessage struct {
	Role        string `json:"role"`
	Content     string `json:"content"`
	TS          int64  `json:"ts"`
	System      bool   `json:"system"`
	ToolCalls   int    `json:"tool_calls"`
	ToolResults int    `json:"tool_results"`
}

// LoadAgentsview reads the driver's JSONL, keyed by session id without
// agentsview's agent prefix ("codex:", "devin:").
func LoadAgentsview(path string) (map[string]AVSession, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]AVSession{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		var s AVSession
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		id := s.SessionID
		if i := strings.IndexByte(id, ':'); i >= 0 {
			id = id[i+1:]
		}
		out[id] = s
	}
	return out, sc.Err()
}

// CompareAgentsview compares user prompts (in order) and the tool call
// count. userText selects and renders our prompt rows; nil takes every
// KindUser row's text. Harness-injected texts (see injected) are dropped
// on both sides first: Flopwire keeps them as user rows, agentsview
// reclassifies or unwraps them. dropped counts the rows so removed.
func CompareAgentsview(av AVSession, msgs []*transcript.Message, userText func(*transcript.Message) (string, bool)) (c Comparison, dropped int) {
	if userText == nil {
		userText = func(m *transcript.Message) (string, bool) { return m.Text, m.Kind == transcript.KindUser }
	}
	var theirs, ours []Row
	avCalls, ourCalls := 0, 0
	for _, m := range av.Messages {
		avCalls += m.ToolCalls
		if m.Role == "user" && !m.System && m.ToolResults == 0 && strings.TrimSpace(m.Content) != "" {
			if isInjected(m.Content) {
				dropped++
				continue
			}
			theirs = append(theirs, Row{Role: "user", Content: CollapseSpace(m.Content)})
		}
	}
	for _, m := range msgs {
		if m.Kind == transcript.KindToolCall {
			ourCalls++
		}
		if t, ok := userText(m); ok && strings.TrimSpace(t) != "" {
			if isInjected(t) {
				dropped++
				continue
			}
			ours = append(ours, Row{Role: "user", Content: CollapseSpace(t)})
		}
	}
	c = Comparison{Row: -1, NFAD: len(theirs), NOur: len(ours)}
	for i := 0; i < max(len(theirs), len(ours)); i++ {
		var a, b Row
		if i < len(theirs) {
			a = theirs[i]
		}
		if i < len(ours) {
			b = ours[i]
		}
		if a != b {
			c.Row, c.FAD, c.Ours = i, a, b
			c.Problems = append(c.Problems, fmt.Sprintf("prompt %d differs", i))
			return c, dropped
		}
	}
	if avCalls != ourCalls {
		c.Problems = append(c.Problems, fmt.Sprintf("tool calls: agentsview %d, ours %d", avCalls, ourCalls))
	}
	return c, dropped
}

// slashCommand is how agentsview renders a Claude <command-name> wrapper.
var slashCommand = regexp.MustCompile(`^/[\w:.-]+(\s|$)`)

func isInjected(s string) bool {
	s = strings.TrimSpace(s)
	if slashCommand.MatchString(s) {
		return true
	}
	for _, w := range injected {
		if strings.HasPrefix(s, w.prefix) {
			return true
		}
	}
	return false
}
