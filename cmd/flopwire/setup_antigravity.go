package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/flopwire/flopwire/plugins"
)

const antigravityPlugin = "flopwire"
const antigravityShim = "scripts/flopwire-hook"
const antigravityMessagingSkill = "skills/messaging/SKILL.md"

type antigravityPluginEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
	Enabled bool   `json:"enabled"`
}

func setupAntigravity(ctx context.Context, env *setupEnv) harnessReport {
	r := harnessReport{Harness: "antigravity", Done: []string{}, Todo: []string{}, Warnings: []string{}}
	path, err := env.lookPath("agy")
	if err != nil {
		return r
	}
	r.Detected, r.Command, r.Scope, r.Plugin = true, path, "user", antigravityPlugin
	if out, _, err := env.run(ctx, path, "--version"); err == nil {
		r.HarnessVersion = strings.TrimSpace(string(out))
	}
	mode := env.mode
	if env.scope != "user" && mode != setupCheck {
		mode = setupCheck
		r.Warnings = append(r.Warnings, "Antigravity installs for the user only. Run flopwire setup without --scope to change its plugin.")
	}
	fail := func(err error) harnessReport {
		r.Error = err.Error()
		return r
	}
	list := func() (antigravityPluginEntry, error) {
		out, stderr, err := env.run(ctx, path, "plugin", "list", "--json")
		if err != nil {
			return antigravityPluginEntry{}, antigravityCLIError("list", stderr, err)
		}
		var entries []antigravityPluginEntry
		if err := json.Unmarshal(out, &entries); err != nil {
			return antigravityPluginEntry{}, fmt.Errorf("read agy plugin list: %w", err)
		}
		for _, entry := range entries {
			if entry.Name == antigravityPlugin {
				return entry, nil
			}
		}
		return antigravityPluginEntry{}, nil
	}
	entry, err := list()
	if err != nil {
		return fail(err)
	}
	if entry.Name != "" && !antigravityPluginOwned(entry.Path, env.home) {
		message := "the installed Antigravity plugin named flopwire is outside the Flopwire-managed user plugin or has a different manifest"
		if mode == setupInstall {
			return fail(errors.New(message))
		}
		r.Warnings = append(r.Warnings, message)
		return r
	}
	files, err := antigravityPluginFiles()
	if err != nil {
		return fail(err)
	}
	if mode == setupInstall && (entry.Name == "" || !antigravityPluginCurrent(entry.Path, files)) {
		stage, err := os.MkdirTemp("", "flopwire-antigravity-*")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(stage)
		for _, file := range files {
			dest := filepath.Join(stage, file.name)
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fail(err)
			}
			if err := os.WriteFile(dest, file.body, file.mode); err != nil {
				return fail(err)
			}
		}
		_, stderr, err := env.run(ctx, path, "plugin", "install", stage)
		if err != nil {
			return fail(antigravityCLIError("install", stderr, err))
		}
		r.Done = append(r.Done, "installed the bundled Flopwire plugin")
		entry, err = list()
		if err != nil {
			return fail(err)
		}
		if !antigravityPluginOwned(entry.Path, env.home) || !antigravityPluginCurrent(entry.Path, files) {
			return fail(errors.New("agy did not install the expected Flopwire plugin files"))
		}
	}
	if mode == setupRemove && entry.Name != "" {
		_, stderr, err := env.run(ctx, path, "plugin", "uninstall", antigravityPlugin)
		if err != nil {
			return fail(antigravityCLIError("uninstall", stderr, err))
		}
		entry, err = list()
		if err != nil {
			return fail(err)
		}
		if entry.Name != "" {
			return fail(errors.New("agy plugin list still shows Flopwire after uninstalling"))
		}
		r.Done = append(r.Done, "uninstalled the Flopwire plugin")
	}
	r.Installed, r.Enabled, r.Version = entry.Name != "", entry.Enabled, entry.Version
	if mode != setupRemove {
		switch {
		case !r.Installed:
			r.Todo = append(r.Todo, "install the Antigravity plugin: flopwire setup")
		case !r.Enabled:
			r.Todo = append(r.Todo, "enable the Antigravity plugin: agy plugin enable flopwire")
		case !antigravityPluginCurrent(entry.Path, files):
			r.Todo = append(r.Todo, "update the Antigravity plugin: flopwire setup")
		}
		if r.Installed && antigravityPluginCurrent(entry.Path, files) {
			applyHookBinary(ctx, env, &r, resolveHookBinary(ctx, env, "antigravity", filepath.Join(entry.Path, antigravityShim)))
		}
	}
	return r
}

func antigravityCLIError(operation string, stderr []byte, err error) error {
	if detail := strings.TrimSpace(string(stderr)); detail != "" {
		return fmt.Errorf("agy plugin %s: %w: %s", operation, err, detail)
	}
	return fmt.Errorf("agy plugin %s: %w", operation, err)
}

type antigravityPluginFile struct {
	name string
	body []byte
	mode os.FileMode
}

func antigravityPluginFiles() ([]antigravityPluginFile, error) {
	var files []antigravityPluginFile
	for _, name := range []string{"plugin.json", "hooks.json", "mcp_config.json"} {
		body, err := plugins.FS.ReadFile(plugins.AntigravityDir + "/" + name)
		if err != nil {
			return nil, err
		}
		files = append(files, antigravityPluginFile{name, body, 0o644})
	}
	shim, err := plugins.FS.ReadFile(plugins.CodexDir + "/" + plugins.ShimPath)
	if err != nil {
		return nil, err
	}
	skill, err := plugins.FS.ReadFile(plugins.CodexDir + "/" + antigravityMessagingSkill)
	if err != nil {
		return nil, err
	}
	return append(files,
		antigravityPluginFile{antigravityShim, shim, 0o755},
		antigravityPluginFile{antigravityMessagingSkill, skill, 0o644},
	), nil
}

func antigravityPluginOwned(root, home string) bool {
	if root == "" || !samePath(root, filepath.Join(home, ".gemini", "config", "plugins", antigravityPlugin)) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Name   string `json:"name"`
		Author struct {
			URL string `json:"url"`
		} `json:"author"`
	}
	return json.Unmarshal(data, &manifest) == nil && manifest.Name == antigravityPlugin &&
		manifest.Author.URL == "https://github.com/flopwire/flopwire"
}

func antigravityPluginCurrent(root string, files []antigravityPluginFile) bool {
	for _, file := range files {
		path := filepath.Join(root, file.name)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || (file.mode&0o111 != 0 && info.Mode().Perm()&0o100 == 0) {
			return false
		}
		body, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(body, file.body) {
			return false
		}
	}
	return true
}
