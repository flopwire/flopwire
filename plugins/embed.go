// Package plugins holds the Claude Code, Codex and Antigravity plugin directories, so
// `flopwire probe --as-installed` installs the plugin of the same commit
// as the binary that runs it, the way flopwire setup installs it from
// this repository.
package plugins

import "embed"

// FS holds the source files for the bundled plugins.
//
//go:embed all:claude-code/flopwire all:codex/flopwire all:antigravity/flopwire
var FS embed.FS

// The plugin directories in FS.
const (
	ClaudeCodeDir  = "claude-code/flopwire"
	CodexDir       = "codex/flopwire"
	AntigravityDir = "antigravity/flopwire"
)

// ShimPath is the hook shim inside a plugin directory.
const ShimPath = "bin/flopwire-hook"
