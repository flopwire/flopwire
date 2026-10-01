package synthcorpus

import (
	"context"
	"crypto/sha256"
	enchex "encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

// small is a corpus a test can generate and parse in seconds. BigMin is
// lowered so it still has big files and the big-file query.
func small(seed uint64) Config { return Config{Seed: seed, Bytes: 40 << 20, BigMin: 3 << 20} }

func gen(t *testing.T, cfg Config) (string, *Manifest) {
	t.Helper()
	root := t.TempDir()
	m, err := Generate(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return root, m
}

// treeHash hashes every file's relative path, bytes and mtime.
func treeHash(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		io.WriteString(h, rel+"\x00")
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if filepath.Ext(p) == ".jsonl" { // transcripts carry their last line's time
			io.WriteString(h, fi.ModTime().UTC().String())
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(h, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return enchex.EncodeToString(h.Sum(nil))
}

func TestGenerateDeterministic(t *testing.T) {
	a, ma := gen(t, small(7))
	b, mb := gen(t, small(7))
	c, _ := gen(t, small(8))
	if ha, hb := treeHash(t, a), treeHash(t, b); ha != hb {
		t.Fatalf("same seed, different trees: %s vs %s", ha, hb)
	}
	if ma.Bytes != mb.Bytes || ma.Files != mb.Files {
		t.Fatalf("same seed, different manifests: %+v vs %+v", ma, mb)
	}
	if treeHash(t, a) == treeHash(t, c) {
		t.Fatal("different seeds, same tree")
	}
}

func TestGenerateParsesCleanly(t *testing.T) {
	root, m := gen(t, small(1))
	t.Logf("manifest: files %d (claude %d + %d subagents, codex %d, devin %d sessions), %.1fMB, big %d (%.1fMB), companions %d, lines %d p50 %dB p99 %dB max %dB, %.0f%% of bytes in lines over 100KB",
		m.Files, m.ClaudeFiles, m.Subagents, m.CodexFiles, m.DevinSess, float64(m.Bytes)/1e6, m.BigFiles, float64(m.BigBytes)/1e6,
		m.Companions, m.Lines, m.LineP50, m.LineP99, m.LineMax, 100*m.HugeShare)
	if m.Bytes < 36<<20 || m.Bytes > 48<<20 {
		t.Errorf("corpus is %d bytes, want about 40MiB", m.Bytes)
	}
	if m.ClaudeFiles == 0 || m.Subagents == 0 || m.CodexFiles == 0 || m.DevinSess == 0 || m.BigFiles == 0 || m.Companions == 0 {
		t.Errorf("a file kind is missing: %+v", m)
	}
	rep, err := Verify(context.Background(), root, m.NeedleList())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed %d sources, %d messages, %d truncations", rep.Sources, rep.Messages, rep.Truncated)
	if rep.Sources != m.Files {
		t.Errorf("parsed %d sources, generated %d", rep.Sources, m.Files)
	}
	if err := rep.Check(m); err != nil {
		t.Fatal(err)
	}

	// The query set names every needle query and decodes like the bench reads it.
	data, err := os.ReadFile(QueriesPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var qs []Query
	if err := yaml.Unmarshal(data, &qs); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, q := range qs {
		names[q.Name] = true
		if q.Until == "" || q.Expect.MinHits == 0 {
			t.Errorf("query %s: until %q, min_hits %d", q.Name, q.Until, q.Expect.MinHits)
		}
	}
	for _, n := range []string{"session-uuid", "rare-phrase", "exact-path", "repo-filter", "since-filter", "agent-filter", "regex-code", "subagent", "big-file", "tool-output", "devin", "common-term"} {
		if !names[n] {
			t.Errorf("query %s missing", n)
		}
	}
}

// TestCheckCatchesMiscount proves Check fails when a needle is in more
// messages than were planted (a duplicated row).
func TestCheckCatchesMiscount(t *testing.T) {
	m := &Manifest{Needles: map[string]int{"quokka": 2}}
	r := &Report{Needles: map[string]int{"quokka": 3}}
	if err := r.Check(m); err == nil {
		t.Fatal("Check passed a needle counted 3 times but planted twice")
	}
	r.Needles["quokka"] = 2
	if err := r.Check(m); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateRefusesNonEmptyRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, small(1)); err == nil {
		t.Fatal("generated into a non-empty directory")
	}
}
