// Session linkage for Codex rollouts: thread_spawn subagents, codex exec
// review children, and forks.
//
// Rules ported by hand from:
//   - codecast-sh/codecast packages/cli/src/parser.ts (CodexThreadSpawn,
//     extractCodexSessionMetadata, isCompletedNativeCodexReviewChild,
//     extractCodexForkRoot) at commit 55ec36ee4ca54de86a1298797c6aba95a8b7d976
//     (MIT, see third_party/codecast);
//   - gbasin/agentboard src/server/logDiscovery.ts extractCodexSubagentLink
//     and src/server/subagentLogs.ts at commit
//     4b2f9c88c4f92b333f7758fd3481082eb899a69f (MIT, see third_party/agentboard);
//   - kenn-io/agentsview internal/parser/codex.go codexSubagentParentThreadID
//     at commit 563023de1d7b7f5af50ad5967c101341a44a2bfc (MIT, see
//     third_party/agentsview).
//
// Differences: codecast collapses fork lineages into one conversation;
// Flopwire keeps every rollout its own conversation and records the fork edge.
// agentsview suppresses a fork's replayed parent history by resolving the
// parent's turn ids across files; Flopwire uses the child's own
// subagent_history_start_ordinal instead (lines below it are the copied
// prefix), so the parse needs no second file.

package codex

import (
	"encoding/json"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// Link kinds recorded in Conversation.Extra["link"].
const (
	LinkThreadSpawn = "thread_spawn" // in-process subagent of a TUI thread
	LinkReview      = "review"       // codex exec review child (source.subagent == "review")
	LinkFork        = "fork"         // forked_from_id without a subagent source
	LinkExec        = "exec"         // headless codex exec, no parent
)

// meta is what the parser keeps of session_meta, in cursor state.
type meta struct {
	SessionID     string       `json:"sid,omitempty"`
	RootSessionID string       `json:"root,omitempty"` // payload.session_id when it differs
	StartedNS     int64        `json:"ts,omitempty"`
	Cwd           string       `json:"cwd,omitempty"`
	Cwds          string       `json:"cwds,omitempty"` // other cwds, transcript.AddCwd
	Originator    string       `json:"orig,omitempty"`
	CLIVersion    string       `json:"cli,omitempty"`
	Source        string       `json:"src,omitempty"` // "cli", "exec", "vscode", or "subagent"
	Link          string       `json:"link,omitempty"`
	Parent        string       `json:"parent,omitempty"`
	ForkedFrom    string       `json:"fork,omitempty"`
	Depth         int          `json:"depth,omitempty"`
	AgentPath     string       `json:"apath,omitempty"`
	AgentNickname string       `json:"nick,omitempty"`
	AgentRole     string       `json:"role,omitempty"`
	ThreadSource  string       `json:"tsrc,omitempty"`
	Git           *Git         `json:"git,omitempty"`
	HistoryStart  int64        `json:"hso,omitempty"`
	HistoryBase   *HistoryBase `json:"hbase,omitempty"`
	Title         string       `json:"title,omitempty"`
	Legacy        bool         `json:"legacy,omitempty"`
}

type threadSpawn struct {
	ParentThreadID string  `json:"parent_thread_id"`
	Depth          *int    `json:"depth"`
	AgentPath      *string `json:"agent_path"`
	AgentNickname  *string `json:"agent_nickname"`
	AgentRole      *string `json:"agent_role"`
}

// metaFrom classifies a session_meta payload.
func metaFrom(p *sessionMeta, envTS string) meta {
	m := meta{
		SessionID:     p.ID,
		Cwd:           p.Cwd,
		Originator:    p.Originator,
		CLIVersion:    p.CLIVersion,
		ForkedFrom:    strings.TrimSpace(p.ForkedFromID),
		AgentPath:     p.AgentPath,
		AgentNickname: p.AgentNickname,
		AgentRole:     p.AgentRole,
		ThreadSource:  p.ThreadSource,
		Git:           p.Git,
		HistoryBase:   p.HistoryBase,
	}
	if m.SessionID == "" {
		m.SessionID = p.SessionID
	}
	if p.SessionID != "" && p.SessionID != m.SessionID {
		m.RootSessionID = p.SessionID
	}
	ts := parseTS(p.Timestamp)
	if ts.IsZero() {
		ts = parseTS(envTS)
	}
	if !ts.IsZero() {
		m.StartedNS = ts.UnixNano()
	}
	if p.HistoryStart != nil && *p.HistoryStart > 0 {
		m.HistoryStart = *p.HistoryStart
	}

	// source is a string ("cli", "exec", "vscode") or an object
	// {"subagent": "review" | {"thread_spawn": {...}}}.
	var srcString string
	var srcObj struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	raw := trimSpace(p.Source)
	switch {
	case len(raw) > 0 && raw[0] == '"':
		_ = json.Unmarshal(raw, &srcString)
		m.Source = srcString
	case len(raw) > 0 && raw[0] == '{':
		_ = json.Unmarshal(raw, &srcObj)
		m.Source = "subagent"
	}

	parentTop := strings.TrimSpace(p.ParentThreadID)
	sub := trimSpace(srcObj.Subagent)
	switch {
	case len(sub) > 0 && sub[0] == '{':
		var s struct {
			ThreadSpawn *threadSpawn `json:"thread_spawn"`
		}
		_ = json.Unmarshal(sub, &s)
		if s.ThreadSpawn != nil {
			m.Link = LinkThreadSpawn
			m.Parent = firstNonEmpty(strings.TrimSpace(s.ThreadSpawn.ParentThreadID), parentTop)
			m.Depth = 1
			if s.ThreadSpawn.Depth != nil && *s.ThreadSpawn.Depth > 0 {
				m.Depth = *s.ThreadSpawn.Depth
			}
			m.AgentPath = firstNonEmpty(m.AgentPath, deref(s.ThreadSpawn.AgentPath))
			m.AgentNickname = firstNonEmpty(m.AgentNickname, deref(s.ThreadSpawn.AgentNickname))
			m.AgentRole = firstNonEmpty(m.AgentRole, deref(s.ThreadSpawn.AgentRole))
		} else {
			m.Link = "subagent"
			m.Parent = parentTop
		}
	case len(sub) > 0 && sub[0] == '"':
		var name string
		_ = json.Unmarshal(sub, &name)
		m.Link = name // "review" for codex exec review children
		if name == "" {
			m.Link = "subagent"
		}
		m.Parent = parentTop // absent for standalone reviews
	case m.ForkedFrom != "":
		m.Link = LinkFork
	case m.Source == "exec":
		m.Link = LinkExec
	case parentTop != "" && m.ThreadSource == "subagent":
		m.Link = "subagent"
		m.Parent = parentTop
	}
	if m.Parent != "" && m.Depth == 0 {
		m.Depth = 1
	}
	return m
}

// conversation renders m. It is rebuilt from cursor state on every call, so
// an incremental parse emits values identical to a full parse.
func (m *meta) conversation() *transcript.Conversation {
	c := &transcript.Conversation{
		Agent:           transcript.AgentCodex,
		SessionID:       m.SessionID,
		Cwd:             m.Cwd,
		OtherCwds:       transcript.SplitCwds(m.Cwds),
		Title:           m.Title,
		ParentSessionID: m.Parent,
		Depth:           m.Depth,
	}
	if m.StartedNS != 0 {
		c.StartedAt = tsFromNS(m.StartedNS)
	}
	if m.Git != nil && m.Git.Branch != "" {
		c.Branches = []string{m.Git.Branch}
	}
	extra := map[string]any{}
	put := func(k, v string) {
		if v != "" {
			extra[k] = v
		}
	}
	put("originator", m.Originator)
	put("cli_version", m.CLIVersion)
	put("source", m.Source)
	put("link", m.Link)
	put("forked_from_id", m.ForkedFrom)
	put("root_session_id", m.RootSessionID)
	put("agent_path", m.AgentPath)
	put("agent_nickname", m.AgentNickname)
	put("agent_role", m.AgentRole)
	put("thread_source", m.ThreadSource)
	if m.Git != nil {
		extra["git"] = *m.Git
	}
	if m.HistoryStart > 0 {
		extra["subagent_history_start_ordinal"] = m.HistoryStart
	}
	if m.HistoryBase != nil {
		extra["history_base"] = *m.HistoryBase
	}
	if m.Legacy {
		extra["format"] = "legacy"
	}
	if len(extra) > 0 {
		c.Extra = extra
	}
	return c
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
