package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestImportRejectsUnverifiedEvidenceBeforeUpload(t *testing.T) {
	for _, mode := range []string{"checksum", "traversal", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			name := "evidence.jsonl"
			b := []byte("private unrelated data")
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(b)
			checksum := hex.EncodeToString(h[:])
			want := "checksum mismatch"
			switch mode {
			case "checksum":
				checksum = strings.Repeat("0", 64)
			case "traversal":
				name = "../evidence.jsonl"
				want = "invalid recovery filename"
			case "symlink":
				name = "link.jsonl"
				want = "not a regular file"
				if err := os.Symlink(p, filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			m := cassimport.Manifest{Version: 1, Origin: "original", Entries: []cassimport.Entry{{File: name, Agent: transcript.AgentClaude, SessionID: "original", SHA256: checksum}}}
			f, err := os.Create(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(f).Encode(m)
			f.Close()
			err = importCommand(context.Background(), []string{"upload", "--dir", dir}, io.Discard)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}
