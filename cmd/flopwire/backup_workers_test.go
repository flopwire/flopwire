package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupCommandRejectsInvalidWorkersBeforeEnvironmentAccess(t *testing.T) {
	for _, workers := range []string{"0", "-1", "33"} {
		err := backupCommand(t.Context(), []string{"--output", filepath.Join(t.TempDir(), "backup"), "--object-workers", workers})
		if err == nil || !strings.Contains(err.Error(), "object workers must be between 1 and 32") {
			t.Fatalf("workers=%s err=%v", workers, err)
		}
	}
}
