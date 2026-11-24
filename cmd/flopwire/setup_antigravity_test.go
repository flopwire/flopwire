package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSetupAntigravityPluginLifecycle(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "config", "plugins", "flopwire")
	var installed, enabled bool
	var changes []string
	env := &setupEnv{home: home, scope: "user", mode: setupCheck,
		lookPath: func(name string) (string, error) {
			if name != "agy" {
				t.Fatalf("unexpected executable %q", name)
			}
			return "/fake/bin/agy", nil
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
			switch {
			case name == "/bin/sh":
				return []byte("recorded /fake/bin/flopwire\n"), nil, nil
			case name == "/fake/bin/flopwire":
				return nil, nil, errors.New("no build info in fixture")
			case slices.Equal(args, []string{"--version"}):
				return []byte("1.5.16\n"), nil, nil
			case slices.Equal(args, []string{"plugin", "list", "--json"}):
				report := []antigravityPluginEntry{}
				if installed {
					report = append(report, antigravityPluginEntry{Name: "flopwire", Enabled: enabled, Path: root, Version: "0.1.0"})
				}
				out, err := json.Marshal(report)
				return out, nil, err
			case len(args) == 3 && args[0] == "plugin" && args[1] == "install":
				changes = append(changes, "install")
				if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
					return nil, nil, err
				}
				if installed {
					if err := os.RemoveAll(root); err != nil {
						return nil, nil, err
					}
				} else {
					enabled = true
				}
				if err := os.CopyFS(root, os.DirFS(args[2])); err != nil {
					return nil, nil, err
				}
				installed = true
				return nil, nil, nil
			case slices.Equal(args, []string{"plugin", "uninstall", "flopwire"}):
				changes = append(changes, "uninstall")
				installed = false
				return nil, nil, os.RemoveAll(root)
			default:
				t.Fatalf("unexpected command: %s %q", name, args)
				return nil, nil, nil
			}
		},
	}
	check := func(mode string, wantInstalled, wantEnabled bool) {
		t.Helper()
		env.mode = mode
		got := setupAntigravity(ctx, env)
		if got.Error != "" || got.Installed != wantInstalled || got.Enabled != wantEnabled || got.HarnessVersion != "1.5.16" {
			t.Fatalf("%s: %+v", mode, got)
		}
	}
	check(setupCheck, false, false)
	check(setupInstall, true, true)
	shim := filepath.Join(root, antigravityShim)
	if info, err := os.Stat(shim); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed shim is not executable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, antigravityMessagingSkill)); err != nil {
		t.Fatalf("installed messaging skill: %v", err)
	}
	check(setupInstall, true, true)
	if err := os.Chmod(shim, 0o644); err != nil {
		t.Fatal(err)
	}
	check(setupInstall, true, true)
	enabled = false
	check(setupCheck, true, false)
	if err := os.WriteFile(filepath.Join(root, "hooks.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	check(setupInstall, true, false)
	check(setupRemove, false, false)
	check(setupRemove, false, false)
	// Preserve an unrelated plugin with the same name.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{"name":"flopwire"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	installed = true
	env.mode = setupInstall
	if got := setupAntigravity(ctx, env); got.Error == "" {
		t.Fatalf("same-name installation was not refused: %+v", got)
	}
	env.mode = setupRemove
	if got := setupAntigravity(ctx, env); len(got.Warnings) == 0 {
		t.Fatalf("same-name removal has no warning: %+v", got)
	}
	if want := []string{"install", "install", "install", "uninstall"}; !slices.Equal(changes, want) {
		t.Fatalf("mutation commands %q, want %q", changes, want)
	}
}
