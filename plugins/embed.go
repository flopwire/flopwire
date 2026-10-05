// Package plugins holds the Claude Code and Codex plugin directories, so
// `flopwire probe --as-installed` installs the plugin of the same commit
// as the binary that runs it, the way flopwire setup installs it from
// this repository.
package plugins

import "embed"

// FS holds claude-code/flopwire and codex/flopwire.
//
//go:embed all:claude-code/flopwire all:codex/flopwire
var FS embed.FS

// The plugin directories in FS.
const (
	ClaudeCodeDir = "claude-code/flopwire"
	CodexDir      = "codex/flopwire"
)

// ShimPath is the hook shim inside a plugin directory.
const ShimPath = "bin/flopwire-hook"
