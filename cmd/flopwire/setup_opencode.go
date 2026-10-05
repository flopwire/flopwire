package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	opencodeplugin "github.com/flopwire/flopwire/plugins/opencode"
)

// opencode (issue #62). opencode loads every .js file in its global
// plugin directory, <config dir>/opencode/plugins (XDG_CONFIG_HOME or
// ~/.config), at start, with no config entry: setup installs the plugin by
// writing its one file there, and removes it by deleting that file. Its
// own `opencode plugin` command installs npm packages only. The plugin
// imports @opencode-ai/plugin, which opencode installs into the config
// directory itself.

// opencodePluginMark is in the first lines of the plugin file: a file
// without it is not Flopwire's, and setup neither overwrites nor deletes
// it.
const opencodePluginMark = "// Flopwire for opencode"

// opencodePluginDir is opencode's global plugin directory.
func opencodePluginDir(home string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "opencode", "plugins")
}

func setupOpencode(ctx context.Context, env *setupEnv) harnessReport {
	r := harnessReport{Harness: "opencode", Plugin: opencodeplugin.FileName, Done: []string{}, Todo: []string{}, Warnings: []string{}}
	path, err := env.lookPath("opencode")
	if err != nil {
		r.Plugin = ""
		return r
	}
	r.Detected, r.Command, r.Scope = true, path, "user"
	if out, _, err := env.run(ctx, path, "--version"); err == nil {
		r.HarnessVersion = strings.TrimSpace(string(out))
	}
	dir := opencodePluginDir(env.home)
	file := filepath.Join(dir, opencodeplugin.FileName)
	r.Marketplace = file
	mode := env.mode
	if env.scope != "user" && mode != setupCheck {
		mode = setupCheck
		r.Warnings = append(r.Warnings, "setup installs the opencode plugin for the user only, so --scope "+env.scope+" changed nothing in opencode; run flopwire setup without --scope to set it up")
	}
	have, err := os.ReadFile(file)
	switch {
	case os.IsNotExist(err):
		have = nil
	case err != nil:
		r.Error = err.Error()
		return r
	}
	ours := have != nil && bytes.Contains(firstLines(have, 5), []byte(opencodePluginMark))
	if have != nil && !ours {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s is not Flopwire's plugin; setup installs, updates and removes nothing there. Move it aside, then run flopwire setup", file))
	}
	current := ours && bytes.Equal(have, opencodeplugin.Source)

	switch mode {
	case setupInstall:
		switch {
		case have != nil && !ours:
			r.Error = "did not install the opencode plugin: " + file + " is another file (see warnings)"
		case current:
		default:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				r.Error = err.Error()
				return r
			}
			if err := writeFileAtomic(file, opencodeplugin.Source, 0o644); err != nil {
				r.Error = err.Error()
				return r
			}
			if have == nil {
				r.Done = append(r.Done, "installed the opencode plugin at "+file)
			} else {
				r.Done = append(r.Done, "updated the opencode plugin at "+file)
			}
			ours, current = true, true
		}
	case setupRemove:
		if ours {
			if err := os.Remove(file); err != nil {
				r.Error = err.Error()
				return r
			}
			r.Done = append(r.Done, "removed the opencode plugin "+file)
			ours, current = false, false
		}
	}
	r.Installed, r.Enabled = ours, ours
	if ours && current {
		r.Version = version
	}
	switch {
	case mode == setupRemove:
	case !ours && have == nil:
		r.Todo = append(r.Todo, "install the opencode plugin: flopwire setup")
	case ours && !current:
		r.Todo = append(r.Todo, "the opencode plugin differs from this flopwire's; run flopwire setup to update it")
	}
	if ours && mode != setupRemove {
		applyHookBinary(ctx, env, &r, opencodeHookBinary())
	}
	if len(r.Done) > 0 && mode != setupRemove {
		r.Todo = append(r.Todo, "restart your opencode sessions: opencode loads plugins at start")
	}
	if mode != setupRemove {
		// An MCP server for Flopwire in opencode's config serves the same
		// tools again, and its messaging tools cannot tell which session
		// calls them.
		for _, name := range []string{"opencode.json", "opencode.jsonc"} {
			b, err := os.ReadFile(filepath.Join(filepath.Dir(dir), name))
			if err == nil && bytes.Contains(b, []byte("flopwire mcp")) || err == nil && bytes.Contains(b, []byte(`"flopwire"`)) {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s names a flopwire MCP server; the plugin serves the same tools as the calling session, so remove that entry", filepath.Join(filepath.Dir(dir), name)))
			}
		}
	}
	return r
}

func firstLines(b []byte, n int) []byte {
	end := 0
	for i := 0; i < n && end < len(b); i++ {
		j := bytes.IndexByte(b[end:], '\n')
		if j < 0 {
			return b
		}
		end += j + 1
	}
	return b[:end]
}
