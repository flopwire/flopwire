//go:build darwin || linux

package cowork

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileRejectsSymlinksAndKeepsDescriptorStable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	path := filepath.Join(root, "inside", "file.txt")
	put(t, path, "original")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	put(t, outside, "secret")
	f, err := OpenFile(root, path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = os.Rename(filepath.Join(root, "inside"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Dir(outside), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(f)
	if err != nil || string(content) != "original" {
		t.Fatalf("descriptor changed: %q %v", content, err)
	}
	for _, unsafe := range []string{outside, filepath.Join(root, "inside", "secret.txt"), root} {
		if opened, err := OpenFile(root, unsafe); err == nil {
			opened.Close()
			t.Fatalf("unsafe file opened: %s", unsafe)
		}
	}
	link := filepath.Join(root, "final.txt")
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenFile(root, link); err == nil {
		opened.Close()
		t.Fatal("final symlink opened")
	}
	rootLink := filepath.Join(t.TempDir(), "root-link")
	if err = os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenFile(rootLink, filepath.Join(rootLink, "old", "file.txt")); err == nil {
		opened.Close()
		t.Fatal("root symlink opened")
	}
	// A replaced final path also cannot change an already-open descriptor.
	stable := filepath.Join(root, "old", "file.txt")
	f2, err := OpenFile(root, stable)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	if err = os.Remove(stable); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, stable); err != nil {
		t.Fatal(err)
	}
	content, err = io.ReadAll(f2)
	if err != nil || string(content) != "original" {
		t.Fatalf("final descriptor changed: %q %v", content, err)
	}
}
