package backup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the operator script with real pg_dump, object storage and restic.
// Its downloaded result then goes through the existing application restore test.
func offsiteRoundTrip(t *testing.T, databaseURL, bucket string) string {
	t.Helper()
	for _, tool := range []string{"restic", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatal(err)
		}
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	stage := filepath.Join(work, "stage")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(work, "flopwire")
	build := exec.Command("go", "build", "-o", binary, "./cmd/flopwire")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	env := append(os.Environ(),
		"DATABASE_URL="+databaseURL,
		"S3_ENDPOINT="+os.Getenv("FLOPWIRE_TEST_S3_ENDPOINT"),
		"S3_ACCESS_KEY="+os.Getenv("FLOPWIRE_TEST_S3_ACCESS_KEY"),
		"S3_SECRET_KEY="+os.Getenv("FLOPWIRE_TEST_S3_SECRET_KEY"),
		"S3_SECURE=false", "S3_BUCKET="+bucket,
		"RESTIC_REPOSITORY="+filepath.Join(work, "repository"), "RESTIC_PASSWORD=synthetic-restic-test-password",
	)
	run := func(name string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, output)
		}
		return output
	}
	run("restic", "init")
	config := map[string]any{"encrypted_staging": true, "staging_dir": stage, "flopwire_command": []string{binary}}
	raw, _ := json.Marshal(config)
	configPath := filepath.Join(work, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var firstID string
	for i := 0; i < 2; i++ {
		run("python3", filepath.Join(root, "scripts/offsite-backup.py"), "--config", configPath)
		raw, err := os.ReadFile(filepath.Join(stage, "status.json"))
		if err != nil {
			t.Fatal(err)
		}
		var status struct {
			Status string `json:"status"`
			ID     string `json:"snapshot_id"`
		}
		if err := json.Unmarshal(raw, &status); err != nil || status.Status != "complete" {
			t.Fatalf("status %s: %v", raw, err)
		}
		if i == 0 {
			firstID = status.ID
		}
		if _, err := os.Stat(filepath.Join(stage, "snapshot")); !os.IsNotExist(err) {
			t.Fatal("staging backup remains after upload")
		}
	}
	var snapshots []map[string]any
	if err := json.Unmarshal(run("restic", "snapshots", "--json"), &snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshots=%d, want both recovery points", len(snapshots))
	}
	run("restic", "check", "--read-data")
	// Recover the older snapshot after a second run, without the staging files.
	target := filepath.Join(work, "download")
	run("restic", "restore", firstID, "--target", target, "--verify")
	recovered := filepath.Join(target, "snapshot")
	if _, err := Verify(recovered); err != nil {
		t.Fatal(err)
	}
	return recovered
}
