package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/devin"
)

// Hooks inside subagents (issue #107).
//
// All three harnesses run the configured hooks inside subagents too, and
// all three give such a hook the PARENT session's id (probes 2026-10-03):
//
//   - Claude Code 2.1.288: session_id and transcript_path are the
//     parent's; agent_id and agent_type are added ("agent_id: present only
//     when the hook fires inside a subagent call", hooks reference). With
//     `claude --agent NAME` the main thread gets agent_type alone. The
//     subagent's transcript is <session>/subagents/agent-<agent_id>.jsonl
//     (or under subagents/workflows/<run>/) beside the parent's.
//     SubagentStart and SubagentStop carry it as agent_transcript_path. No
//     Stop or SessionEnd fires for a subagent.
//   - Codex 0.160.0: session_id is the root thread's; agent_id is the
//     spawned thread's id and transcript_path its own rollout. No
//     SessionEnd fires for a subagent (core: run_session_end_hooks returns
//     early for SessionSource::SubAgent).
//   - Devin 3000.11.1: a run_subagent subagent's hooks carry the session's
//     id and nothing else that differs, in the input or the environment.
//     It fires PreToolUse, PostToolUse and a Stop of its own before the
//     run_subagent call returns. The session store tells them apart
//     (devin.OwnToolCall, devin.SubagentRunning).
//
// Messages are addressed to sessions, and a subagent is not one: peers
// lists sessions, never subagents (plan §9.2: addressing Codex subagents is
// out of scope). So a hook inside a subagent delivers nothing, takes no
// standing instruction, and sends its flush without an event. When it
// cannot tell, a delivering hook delivers nothing: the session's next hook
// of its own gets the messages.

// hookSub says whether a hook runs inside a subagent and, when known, the
// subagent's transcript for the flush.
type hookSub struct {
	inside     bool
	transcript string
}

// devinStoreBudget bounds the Devin store read a hook makes.
var devinStoreBudget = 100 * time.Millisecond

var (
	agentIDRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	rolloutUUID = regexp.MustCompile(`rollout-[^/]*-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)
)

func hookSubagent(ctx context.Context, in hookInput, harness transcript.Agent, getenv func(string) string) hookSub {
	switch harness {
	case transcript.AgentClaude:
		if in.AgentID != "" {
			return hookSub{inside: true, transcript: claudeSubagentTranscript(in)}
		}
		// No agent_id, but the transcript is a subagent's: a harness
		// version that marks subagents only by their file.
		if strings.Contains(filepath.ToSlash(in.TranscriptPath), "/subagents/") {
			return hookSub{inside: true, transcript: in.TranscriptPath}
		}
	case transcript.AgentCodex:
		path := in.TranscriptPath
		if in.AgentTranscriptPath != "" {
			path = in.AgentTranscriptPath
		}
		if in.AgentID != "" {
			return hookSub{inside: true, transcript: path}
		}
		// The rollout is another thread's than the session the hook names:
		// what it prints would enter that thread, not the session.
		if m := rolloutUUID.FindStringSubmatch(in.TranscriptPath); m != nil && in.SessionID != "" && m[1] != in.SessionID {
			return hookSub{inside: true, transcript: path}
		}
	case transcript.AgentDevin:
		return devinSubagent(ctx, in, getenv)
	}
	return hookSub{}
}

// devinSubagent reads the session store for the events a Devin subagent
// fires. A tool hook whose call is not the session's own, or whose store
// cannot be read, counts as a subagent's: it would deliver into a context
// the hook cannot name. A Stop counts as a subagent's only while the store
// shows one running: a Stop never delivers, and without the store its
// idle signal is the best there is.
func devinSubagent(ctx context.Context, in hookInput, getenv func(string) string) hookSub {
	if in.SessionID == "" {
		return hookSub{}
	}
	home, _ := os.UserHomeDir()
	db := devinStore(getenv, home)
	ctx, cancel := context.WithTimeout(ctx, devinStoreBudget)
	defer cancel()
	switch in.Event {
	case evPostToolUse, "PreToolUse":
		own, err := devin.OwnToolCall(ctx, db, in.SessionID, in.ToolUseID)
		return hookSub{inside: err != nil || !own}
	case "Stop":
		running, err := devin.SubagentRunning(ctx, db, in.SessionID)
		return hookSub{inside: err == nil && running}
	}
	return hookSub{}
}

// claudeSubagentTranscript is the subagent's transcript: the
// agent_transcript_path SubagentStart and SubagentStop carry, else
// <session>/subagents/agent-<agent_id>.jsonl beside the parent's, or the
// one file of that name under subagents/workflows/<run>/. An agent id that
// is not a plain token leaves the input's path.
func claudeSubagentTranscript(in hookInput) string {
	if in.AgentTranscriptPath != "" {
		return in.AgentTranscriptPath
	}
	if !agentIDRe.MatchString(in.AgentID) || !strings.HasSuffix(in.TranscriptPath, ".jsonl") {
		return in.TranscriptPath
	}
	dir := filepath.Join(strings.TrimSuffix(in.TranscriptPath, ".jsonl"), "subagents")
	name := "agent-" + in.AgentID + ".jsonl"
	direct := filepath.Join(dir, name)
	if _, err := fsprobe.Stat(direct); err == nil {
		return direct
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "workflows", "*", name)); len(m) == 1 {
		return m[0]
	}
	return direct
}
