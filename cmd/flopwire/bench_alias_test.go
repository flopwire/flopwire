package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveQueryRepo(t *testing.T) {
	home := t.TempDir()
	if got := resolveQueryRepo("~/Code/console", nil, home); got != filepath.Join(home, "Code", "console") {
		t.Fatalf("no aliases: %q", got)
	}
	if got := resolveQueryRepo("~/Code/console", map[string]string{"console": "/srv/real"}, home); got != "/srv/real" {
		t.Fatalf("alias: %q", got)
	}
	userHome, _ := os.UserHomeDir()
	if got := resolveQueryRepo("~/Code/console", map[string]string{"console": "~/real"}, home); got != filepath.Join(userHome, "real") {
		t.Fatalf("alias under ~: %q", got)
	}
	if got := resolveQueryRepo("", nil, home); got != "" {
		t.Fatalf("empty: %q", got)
	}
	dir := t.TempDir()
	if m, err := loadRepoAliases(filepath.Join(dir, "missing.yaml")); err != nil || m != nil {
		t.Fatalf("missing file: %v %v", m, err)
	}
	os.WriteFile(filepath.Join(dir, "a.yaml"), []byte("console: ~/Code/real\n"), 0o600)
	if m, err := loadRepoAliases(filepath.Join(dir, "a.yaml")); err != nil || m["console"] != "~/Code/real" {
		t.Fatalf("read: %v %v", m, err)
	}
}
