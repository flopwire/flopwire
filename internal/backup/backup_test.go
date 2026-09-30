package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyDetectsChangedObject(t *testing.T) {
	dir := t.TempDir()
	writeCompleteBackupState(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "postgres.dump"), []byte("database"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "objects", "trace"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	dbHash, _, err := fileHash(filepath.Join(dir, "postgres.dump"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := `{"version":4,"database_file":"postgres.dump","database_size":8,"database_sha256":"` + dbHash + `","objects":[{"key":"trace","size":8,"blake3":"0000","sha256":"0000"}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil {
		t.Fatal("expected hash verification failure")
	}
}

func TestVerifyRejectsManifestWhenBackupStateIsIncomplete(t *testing.T) {
	dir := t.TempDir()
	state := State{Operation: "backup", Status: "incomplete", StartedAt: time.Now().UTC()}
	if err := writeState(dir, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("error=%v", err)
	}
}

func TestManifestPublicationFailureNeverLeavesValidityMarker(t *testing.T) {
	for _, boundary := range []string{"after_manifest_sync", "after_manifest_rename"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			writeCompleteBackupState(t, dir)
			err := publishManifest(dir, []byte(`{"version":3}`), func(got string) error {
				if got == boundary {
					return errors.New("injected publication failure")
				}
				return nil
			})
			if err == nil {
				t.Fatal("expected publication failure")
			}
			if _, statErr := os.Stat(filepath.Join(dir, "manifest.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("final manifest survived: %v", statErr)
			}
			if _, verifyErr := Verify(dir); verifyErr == nil {
				t.Fatal("failed publication verified")
			}
		})
	}
}

func TestManifestIsFinalCommitPoint(t *testing.T) {
	dir := t.TempDir()
	writeCompleteBackupState(t, dir)
	if _, err := Verify(dir); err == nil {
		t.Fatal("state without manifest verified")
	}
	if err := publishManifest(dir, []byte(`{"version":3}`), func(boundary string) error {
		raw, readErr := os.ReadFile(filepath.Join(dir, "state.json"))
		if readErr != nil {
			return readErr
		}
		var state State
		if decodeErr := json.Unmarshal(raw, &state); decodeErr != nil {
			return decodeErr
		}
		if state.Status != "complete" {
			return errors.New("manifest publication preceded complete state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRecordsFailedStateBeforeTargetMutation(t *testing.T) {
	input := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "restore-state.json")
	if err := Restore(t.Context(), "unused", nil, "unused", input, statePath); err == nil {
		t.Fatal("expected invalid backup failure")
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Operation != "restore" || state.Status != "failed" || state.Error == "" {
		t.Fatalf("state=%+v", state)
	}
}

func TestCanonicalInventoryCollapsesDuplicateReferencesAndRejectsConflict(t *testing.T) {
	entries := []Entry{{Key: "b", Size: 2, BLAKE3: "bb"}, {Key: "a", Size: 1, BLAKE3: "aa"}, {Key: "a", Size: 1, BLAKE3: "aa"}}
	canonical, err := canonicalInventory(entries)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) != 2 || canonical[0].Key != "a" || canonical[1].Key != "b" {
		t.Fatalf("canonical=%+v", canonical)
	}
	if _, err = canonicalInventory([]Entry{{Key: "a", Size: 1, BLAKE3: "aa"}, {Key: "a", Size: 2, BLAKE3: "bb"}}); err == nil {
		t.Fatal("expected inconsistent duplicate rejection")
	}
}
func TestValidateOutputRejectsBroadPaths(t *testing.T) {
	for _, path := range []string{"", ".", "/"} {
		if ValidateOutput(path) == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if err := ValidateOutput(filepath.Join(t.TempDir(), "backup")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRequiresEncryptedDestinationAcknowledgement(t *testing.T) {
	if _, err := Create(t.Context(), "unused", nil, "unused", filepath.Join(t.TempDir(), "backup"), false); err == nil {
		t.Fatal("expected encrypted destination acknowledgement error")
	}
}

func TestSafeKeyRejectsEscapes(t *testing.T) {
	for _, key := range []string{"", "../x", "/abs", "a/../../x"} {
		if safeKey(key) {
			t.Fatalf("accepted %q", key)
		}
	}
	if !safeKey("chunks/ab/abcdef") {
		t.Fatal("rejected a normal key")
	}
}

func writeCompleteBackupState(t *testing.T, dir string) {
	t.Helper()
	now := time.Now().UTC()
	if err := writeState(dir, State{Operation: "backup", Status: "complete", StartedAt: now.Add(-time.Second), EndedAt: now}); err != nil {
		t.Fatal(err)
	}
}
