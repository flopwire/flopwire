package digest

import (
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/transcript"
)

// summaryArgs are the arguments that best name what a call does, in order
// of preference, for tools without a rule of their own.
var summaryArgs = []string{"command", "cmd", "pattern", "query", "file_path", "notebook_path", "path", "url", "description", "prompt", "message", "task"}

// CallSummary renders a tool call as a one-line args summary for read's
// outline: the command a shell tool runs, the file an edit or read names,
// the files a patch touches, a subagent's description, else the first
// telling argument. Paths under root are shown relative to it. The result
// is at most n bytes.
func CallSummary(tool, text, root string, n int) string {
	rel := func(p string) string {
		if root != "" && strings.HasPrefix(p, root+"/") {
			return p[len(root)+1:]
		}
		return p
	}
	m := &transcript.Message{ToolName: tool, Text: text}
	var s string
	switch {
	case IsShell(tool):
		s = Command(m)
	case editTools[tool] != "":
		if ps := EditedPaths(m); len(ps) > 0 {
			s = rel(ps[0])
		}
	case strings.Contains(text, "*** Begin Patch"):
		ps := PatchPaths(text)
		for i := range ps {
			ps[i] = rel(ps[i])
		}
		s = strings.Join(ps, ", ")
	}
	if s == "" {
		if args := jsonArgs(text); args != nil {
			s = firstArgs(args, rel)
		} else {
			s = text
		}
	}
	return clip(oneLine(s), n)
}

// firstArgs is the preferred arguments' values, else every string
// argument's key=value, in key order.
func firstArgs(args map[string]any, rel func(string) string) string {
	for _, k := range summaryArgs {
		if v := argString(args[k]); v != "" {
			if k == "file_path" || k == "notebook_path" || k == "path" {
				v = rel(v)
			}
			if k == "description" {
				if t := argString(args["subagent_type"]); t != "" {
					v = t + ": " + v
				}
			}
			return v
		}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if v := argString(args[k]); v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// IsSpawn reports whether a tool call starts a subagent.
func IsSpawn(tool string) bool {
	switch tool {
	case "Task", "Agent", "spawn_agent", "spawn_subagent", "delegate":
		return true
	}
	return false
}
