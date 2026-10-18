package cowork

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParserFSConfinesSideReads(t *testing.T) {
	root := t.TempDir()
	project, transcript := fixture(t, root, "a", "w", "app", goodMetadata("app"))
	f := FS{Root: root}
	file, err := f.Open(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if file.Size() == 0 {
		t.Fatal("empty transcript")
	}
	file.Close()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	put(t, outside, "secret")
	link := filepath.Join(project, "linked.txt")
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	audit := filepath.Join(root, "a", "w", "app", "audit.jsonl")
	put(t, audit, "private")
	for _, path := range []string{outside, link, audit, filepath.Join(root, "a", "w", "app.json"), filepath.Join(root, "a", "w", "app", ".claude", "projects", "..", "..", "..", "audit.jsonl")} {
		if file, err = f.Open(path); !errors.Is(err, fs.ErrNotExist) {
			if file != nil {
				file.Close()
			}
			t.Fatalf("unsafe open %s: %v", path, err)
		}
	}
	matches, err := f.Glob(filepath.Join(project, "*"))
	if err != nil || !reflect.DeepEqual(matches, []string{transcript}) {
		t.Fatalf("filtered glob: %v %v", matches, err)
	}
	if matches, err = f.Glob(filepath.Join(filepath.Dir(outside), "*")); err != nil || len(matches) != 0 {
		t.Fatal("outside glob")
	}
	// Glob with wildcard ancestors cannot descend into a symlink project.
	if err = os.Symlink(filepath.Dir(outside), filepath.Join(filepath.Dir(project), "evil")); err != nil {
		t.Fatal(err)
	}
	matches, err = f.Glob(filepath.Join(filepath.Dir(project), "*", "*"))
	if err != nil || !reflect.DeepEqual(matches, []string{transcript}) {
		t.Fatalf("symlink ancestor glob: %v %v", matches, err)
	}
	if !SafeFile(root, transcript) || SafeFile(root, link) || SafeFile(root, outside) || SafeFile("", transcript) {
		t.Fatal("SafeFile boundary")
	}
}
