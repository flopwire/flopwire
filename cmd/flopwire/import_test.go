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
	"time"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/transcript"
	"golang.org/x/sys/unix"
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

func TestRecoverySnapshotsSurviveOriginalReplacement(t *testing.T) {
	for _, replacement := range []string{"rewrite", "symlink", "remove"} {
		t.Run(replacement, func(t *testing.T) {
			root := t.TempDir()
			original := filepath.Join(root, "evidence.jsonl")
			want := []byte("verified evidence\n")
			if err := os.WriteFile(original, want, 0600); err != nil {
				t.Fatal(err)
			}
			id, err := transcript.StatIdentity(original)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256(want)
			m := cassimport.Manifest{Entries: []cassimport.Entry{{File: "evidence.jsonl", SHA256: hex.EncodeToString(h[:])}}}
			var staged string
			err = withRecoverySnapshots(context.Background(), root, m, func(snapshots []recoverySnapshot) error {
				if len(snapshots) != 1 || snapshots[0].identity != id {
					t.Fatal("lost original identity")
				}
				staged = snapshots[0].path
				if err := os.Remove(original); err != nil {
					return err
				}
				switch replacement {
				case "rewrite":
					if err := os.WriteFile(original, []byte("unverified replacement\n"), 0600); err != nil {
						return err
					}
				case "symlink":
					outside := filepath.Join(t.TempDir(), "private.jsonl")
					if err := os.WriteFile(outside, []byte("unrelated private data\n"), 0600); err != nil {
						return err
					}
					if err := os.Symlink(outside, original); err != nil {
						return err
					}
				}
				got, err := os.ReadFile(staged)
				if err == nil && string(got) != string(want) {
					t.Fatal("replacement changed checked upload bytes")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatal("private staging survived import completion")
			}
		})
	}
}

func TestRecoverySnapshotsVerifyAllBeforeUpload(t *testing.T) {
	root := t.TempDir()
	want := []byte("checked\n")
	h := sha256.Sum256(want)
	m := cassimport.Manifest{}
	for _, name := range []string{"first.jsonl", "last.jsonl"} {
		if err := os.WriteFile(filepath.Join(root, name), want, 0600); err != nil {
			t.Fatal(err)
		}
		m.Entries = append(m.Entries, cassimport.Entry{File: name, SHA256: hex.EncodeToString(h[:])})
	}
	m.Entries[1].SHA256 = strings.Repeat("0", 64)
	uploads := 0
	err := withRecoverySnapshots(context.Background(), root, m, func([]recoverySnapshot) error {
		uploads++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") || uploads != 0 {
		t.Fatalf("err=%v uploads=%d", err, uploads)
	}
}

func TestRecoverySnapshotsRejectFIFOWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "pipe.jsonl")
	if err := unix.Mkfifo(p, 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- withRecoverySnapshots(context.Background(), root, cassimport.Manifest{Entries: []cassimport.Entry{{File: "pipe.jsonl"}}}, func([]recoverySnapshot) error {
			return nil
		})
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("FIFO error: %v", err)
		}
	case <-time.After(time.Second):
		// Unblock a regressed blocking open before failing the test.
		if fd, err := unix.Open(p, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
			unix.Close(fd)
		}
		t.Fatal("FIFO preflight blocked waiting for a writer")
	}
}
