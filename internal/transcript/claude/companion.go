// Companion-file resolution: persisted tool outputs and the spawned-by
// fallback for subagents.
//
// Ported by hand from kenn-io/agentsview internal/parser/claude.go at
// commit 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT):
// resolveClaudePersistedToolResultsContext (path from the
// "Full output saved to:" line, else toolUseResult.persistedOutputPath when
// the line has one tool_result or the content is a placeholder),
// claudeToolResultDirs (the session's tool-results/ serves its subagents
// too) and extractToolResultAgentIDLink (toolUseResult.agentId on a line
// with exactly one tool_result). Differences: the file is looked up by base
// name inside the session's own tool-results/ directory, so a relocated
// CLAUDE_CONFIG_DIR still resolves, and workflow runs link through
// toolUseResult.runId.

package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/flopwire/flopwire/internal/transcript"
)

var persistedPathRe = regexp.MustCompile(`(?m)Full output saved to:\s*(.+)$`)

const persistedMarker = "<persisted-output>"

// persisted returns the full text of a persisted tool output when content
// is Claude's <persisted-output> preview. ref is "tool-results/<name>"
// whenever a reference was found; ok is false when the file is unreadable,
// and the caller keeps the preview.
func (r *run) persisted(content, fromResult string, nResults int) (full, ref string, ok, truncated bool) {
	placeholder := strings.Contains(content, persistedMarker)
	if !placeholder && fromResult == "" {
		return "", "", false, false
	}
	path := ""
	if m := persistedPathRe.FindStringSubmatch(content); placeholder && m != nil {
		path = strings.TrimSpace(m[1])
	}
	if path == "" && (placeholder || nResults == 1) {
		path = fromResult
	}
	if path == "" || filepath.Base(filepath.Dir(path)) != "tool-results" || r.loc.sessionDir == "" {
		return "", "", false, false
	}
	name := filepath.Base(path)
	ref = "tool-results/" + name
	if !placeholder {
		// The block holds the whole output inline; nothing to replace.
		return "", "", false, false
	}
	text, ok, truncated := r.readPersisted(filepath.Join(r.loc.sessionDir, "tool-results", name))
	if r.p.Stats != nil {
		if ok {
			r.p.Stats.Persisted.Add(1)
		} else {
			r.p.Stats.PersistMiss.Add(1)
		}
	}
	return text, ref, ok, truncated
}

func (r *run) readPersisted(path string) (string, bool, bool) {
	limit := r.p.MaxPersisted
	if limit <= 0 {
		limit = DefaultMaxPersisted
	}
	f, err := r.p.fs().Open(path)
	if err != nil {
		return "", false, false
	}
	defer f.Close()
	size := f.Size()
	if size < 0 {
		return "", false, false
	}
	b := make([]byte, min(size, limit))
	n, err := f.ReadAt(b, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, false
	}
	return string(b[:n]), true, size > limit

}

// resolveSpawn fills SpawnedBy for a subagent whose meta.json lacks
// toolUseId: the parent transcript's tool_result whose toolUseResult names
// this agent (agentId) or its workflow run (runId) holds the spawning call
// id. The parent writes that result when the child finishes, so a live
// child retries whenever the parent has grown since the last attempt.
func (r *run) resolveSpawn() {
	c := &r.st.Conv
	if !r.loc.subagent() || c.SpawnedBy != "" || r.loc.sessionDir == "" {
		return
	}
	parent := r.parentPath()
	f, err := r.p.fs().Open(parent)
	if err != nil {
		return
	}
	size := f.Size()
	f.Close()
	if size == r.st.ParentSize {
		return
	}
	idx := r.p.spawnIndexFor(parent, size)
	id := idx["agent:"+c.AgentID]
	if c.RunID != "" && idx["run:"+c.RunID] != "" {
		id = idx["run:"+c.RunID]
	}
	if id != "" {
		c.SpawnedBy, r.st.ParentSize = id, 0
		return
	}
	r.st.ParentSize = size
}

// spawnIndex maps "agent:<agentId>" and "run:<runId>" to the tool_use id
// of the parent's spawning call, for one parent transcript at one size.
// Workflow runs spawn many children from one call, so the parent is
// scanned once per size, not once per child.
type spawnIndex struct {
	mu   sync.Mutex
	size int64
	ids  map[string]string
}

func (p *Parser) spawnIndexFor(path string, size int64) map[string]string {
	v, _ := p.spawn.LoadOrStore(path, &spawnIndex{size: -1})
	e := v.(*spawnIndex)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.size != size {
		e.ids, e.size = scanSpawns(p.fs(), path, size), size
	}
	return e.ids
}

// parentPath is the transcript holding the spawning tool call: the parent
// agent's file for a nested subagent, else the main session transcript.
func (r *run) parentPath() string {
	if p := r.st.Conv.Parent; strings.HasPrefix(p, "agent-") {
		dir := filepath.Join(r.loc.sessionDir, "subagents")
		if cand := filepath.Join(dir, p+".jsonl"); fileExists(r.p.fs(), cand) {
			return cand
		}
		if m, _ := r.p.fs().Glob(filepath.Join(dir, "workflows", "*", p+".jsonl")); len(m) > 0 {
			return m[0]
		}
	}
	return r.loc.sessionDir + ".jsonl"
}

func scanSpawns(fsys FS, path string, size int64) map[string]string {
	ids := map[string]string{}
	f, err := fsys.Open(path)
	if err != nil {
		return ids
	}
	defer f.Close()
	needles := [][]byte{[]byte(`"agentId":"`), []byte(`"runId":"`)}
	in := transcript.Input{R: f, Size: size}
	_, _ = transcript.ScanJSONL(context.Background(), in, transcript.Cursor{}, transcript.LineReaderOptions{}, func(_ *transcript.LineReader, l *transcript.Line) error {
		if l.Oversized || !containsAny(l.Data, needles) {
			return nil
		}
		var rec record
		if decodeRecord(l.Data, &rec) != nil || rec.Type != "user" || rec.Message == nil {
			return nil
		}
		t := rec.ToolUseResult
		if t.AgentID == "" && t.RunID == "" {
			return nil
		}
		var calls []string
		for _, b := range contentBlocks(rec.Message.Content) {
			if b.Type == "tool_result" {
				calls = append(calls, b.ToolUseID)
			}
		}
		if len(calls) != 1 || calls[0] == "" {
			return nil
		}
		if t.AgentID != "" {
			ids["agent:"+t.AgentID] = calls[0]
		}
		if t.RunID != "" {
			ids["run:"+t.RunID] = calls[0]
		}
		return nil
	})
	return ids
}

func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}
