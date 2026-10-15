package local

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/transcript"
)

func TestRawRequiresUnchangedIndexedFile(t *testing.T) {
	for _, change := range []string{"unchanged", "rewrite", "append", "replace", "missing watermark", "unindexed range"} {
		t.Run(change, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.jsonl")
			original := []byte("original evidence\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			identity, err := transcript.StatIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			wm, err := transcript.NewWatermark(bytes.NewReader(original), identity, time.Now(), transcript.Cursor{Offset: int64(len(original)), LineNo: 1})
			if err != nil {
				t.Fatal(err)
			}
			watermark := &wm
			length := int64(len(original))
			switch change {
			case "rewrite":
				err = os.WriteFile(path, []byte("unrelated record!\n"), 0600)
			case "append":
				var f *os.File
				f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err == nil {
					_, err = f.WriteString("more\n")
					f.Close()
				}
			case "replace":
				replacement := path + ".new"
				err = os.WriteFile(replacement, original, 0600)
				if err == nil {
					err = os.Rename(replacement, path)
				}
			case "missing watermark":
				watermark = nil
			case "unindexed range":
				length++
			}
			if err != nil {
				t.Fatal(err)
			}
			data, why := readLocal(path, identity.ID, watermark, true, 0, length)
			if change == "unchanged" {
				if why != "" || !bytes.Equal(data, original) {
					t.Fatalf("unchanged: %q %s", data, why)
				}
			} else if why == "" || data != nil {
				t.Fatalf("%s accepted: %q %s", change, data, why)
			}
		})
	}
}

// Simulate a same-tick rewrite by retaining the stat tuple in the indexed
// watermark while its persisted digest still describes the original bytes.
func TestRawRacyTupleRequiresCoveredDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "racy.jsonl")
	original := bytes.Repeat([]byte("a"), 128<<10)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := transcript.StatIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	wm, err := transcript.NewWatermark(bytes.NewReader(original), identity, time.Unix(0, identity.CTime), transcript.Cursor{Offset: int64(len(original))})
	if err != nil {
		t.Fatal(err)
	}
	if !wm.Racy() {
		t.Fatal("fixture is not racy")
	}
	if data, why := readLocal(path, identity.ID, &wm, true, 0, 10); why != "" || string(data) != "aaaaaaaaaa" {
		t.Fatalf("covered original: %q %s", data, why)
	}
	if data, why := readLocal(path, identity.ID, &wm, true, 32<<10, 10); data != nil || why == "" {
		t.Fatalf("uncovered middle: %q %s", data, why)
	}
	changed := bytes.Repeat([]byte("b"), len(original))
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	wm.Identity, err = transcript.StatIdentity(path) // mimic unchanged coarse tuple
	if err != nil {
		t.Fatal(err)
	}
	wm.SampledAt = wm.Identity.CTime
	for _, off := range []int64{0, int64(len(original)) - 10} {
		if data, why := readLocal(path, wm.Identity.ID, &wm, true, off, 10); data != nil || why != "indexed bytes changed" {
			t.Fatalf("same-tick rewrite at %d: %q %s", off, data, why)
		}
	}
}
