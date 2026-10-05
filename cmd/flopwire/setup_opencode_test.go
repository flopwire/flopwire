package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	opencodeplugin "github.com/flopwire/flopwire/plugins/opencode"
)

func opencodeFixture(t *testing.T) (*setupFixture, string) {
	t.Helper()
	f := newSetupFixture(t, false)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, filepath.Join(f.dir, "bin", "opencode")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	return f, filepath.Join(f.home, ".config", "opencode", "plugins", "flopwire.js")
}

func opencodeEntry(t *testing.T, rep setupReport) harnessReport {
	t.Helper()
	for _, h := range rep.Harnesses {
		if h.Harness == "opencode" {
			return h
		}
	}
	t.Fatalf("no opencode entry in %+v", rep)
	return harnessReport{}
}

// setup writes the plugin into opencode's global plugin directory, reports
// it with --check, leaves it alone when current, and --remove deletes it.
func TestSetupOpencode(t *testing.T) {
	f, file := opencodeFixture(t)
	rep, _, err := f.run("--check")
	if err != nil {
		t.Fatal(err)
	}
	if h := opencodeEntry(t, rep); !h.Detected || h.Installed || h.HarnessVersion != "1.18.30" || !slices.Contains(h.Todo, "install the opencode plugin: flopwire setup") {
		t.Fatalf("check before install: %+v", h)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("--check wrote the plugin: %v", err)
	}

	rep, _, err = f.run()
	if err != nil {
		t.Fatal(err)
	}
	h := opencodeEntry(t, rep)
	if !h.Installed || len(h.Done) != 1 || h.Error != "" || h.Version != version {
		t.Fatalf("install: %+v", h)
	}
	got, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(got, opencodeplugin.Source) {
		t.Fatalf("installed plugin differs: %v", err)
	}
	// The plugin starts the binary setup recorded (no shell).
	if self, err := selfPath(); err != nil || h.HookBinary == nil || h.HookBinary.Path != self || h.HookBinary.Via != "recorded path" || h.HookBinary.Shim != "" {
		t.Fatalf("hook binary: %v %+v", err, h.HookBinary)
	}

	rep, _, _ = f.run()
	if h := opencodeEntry(t, rep); len(h.Done) != 0 || !h.Installed {
		t.Fatalf("second install changed something: %+v", h)
	}

	// An older copy is updated.
	if err := os.WriteFile(file, append([]byte(opencodePluginMark+" (old)\n"), "export const Flopwire = async () => ({})\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _, _ = f.run("--check")
	if h := opencodeEntry(t, rep); !h.Installed || h.Version != "" || len(h.Todo) == 0 {
		t.Fatalf("check of an old copy: %+v", h)
	}
	rep, _, _ = f.run()
	if h := opencodeEntry(t, rep); len(h.Done) != 1 || h.Done[0] != "updated the opencode plugin at "+file {
		t.Fatalf("update: %+v", h)
	}

	rep, _, err = f.run("--remove")
	if err != nil {
		t.Fatal(err)
	}
	if h := opencodeEntry(t, rep); h.Installed || len(h.Done) != 1 {
		t.Fatalf("remove: %+v", h)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("--remove left the plugin: %v", err)
	}
}

// A file of the same name that is not Flopwire's is neither overwritten
// nor deleted.
func TestSetupOpencodeForeignFile(t *testing.T) {
	f, file := opencodeFixture(t)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := []byte("export const Mine = async () => ({})\n")
	if err := os.WriteFile(file, foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _, err := f.run()
	if err == nil {
		t.Fatal("install over a foreign file succeeded")
	}
	if h := opencodeEntry(t, rep); h.Error == "" || h.Installed || len(h.Warnings) == 0 {
		t.Fatalf("install over a foreign file: %+v", h)
	}
	f.run("--remove")
	if got, _ := os.ReadFile(file); !bytes.Equal(got, foreign) {
		t.Fatal("setup changed a file it does not own")
	}
}
