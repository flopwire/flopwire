package localindex

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaintenanceLockRequiresExistingIndexAndSharesWriterOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if _, err := AcquireMaintenanceLock(path); err == nil {
		t.Fatal("maintenance created a missing index")
	}
	body := []byte("literal existing index bytes")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	writer, err := acquireLock(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireMaintenanceLock(path)
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("maintenance bypassed writer lock: %v", err)
	}
	writer.Close()
	maintenance, err := AcquireMaintenanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireLock(path, nil)
	if !errors.As(err, &locked) {
		t.Fatal("writer bypassed maintenance ownership")
	}
	maintenance.Close()
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, body) {
		t.Fatal("maintenance modified index contents")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireMaintenanceLock(path); err == nil {
		t.Fatal("maintenance accepted nonregular index")
	}
}
