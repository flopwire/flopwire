package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
)

// backupRestore takes a coordinated backup with the server image (it
// carries pg_dump 17), restores it into a new database and bucket, starts a
// second server on the restored targets and queries it with device d's
// credential.
func (h *harness) backupRestore(t *testing.T, r *result, d *device, needle string) {
	pw, port, project := os.Getenv("FLOPWIRE_E2E_POSTGRES_PASSWORD"), os.Getenv("FLOPWIRE_E2E_RESTORE_PORT"), os.Getenv("FLOPWIRE_E2E_PROJECT")
	if pw == "" || port == "" || project == "" {
		t.Fatal("FLOPWIRE_E2E_POSTGRES_PASSWORD, FLOPWIRE_E2E_RESTORE_PORT and FLOPWIRE_E2E_PROJECT are required")
	}
	dir := filepath.Join(h.cfg.root, "backup")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777) // the container runs as uid 10001
	mount := dir + ":/backup"

	start := time.Now()
	if _, err := h.compose("run", "--rm", "--no-deps", "-v", mount, "flopwire", "backup", "--encrypted-destination", "--output", "/backup/b1"); err != nil {
		t.Fatal(err)
	}
	r.Timings["backup"] = ms(time.Since(start))
	out, err := h.compose("run", "--rm", "--no-deps", "-v", mount, "flopwire", "backup-verify", "--input", "/backup/b1")
	if err != nil || !strings.Contains(out, `"verified": true`) {
		t.Fatalf("backup-verify: %v %s", err, out)
	}
	var manifest struct{ Objects []json.RawMessage }
	raw, _ := os.ReadFile(filepath.Join(dir, "b1", "manifest.json"))
	_ = json.Unmarshal(raw, &manifest)
	r.Notes = append(r.Notes, fmt.Sprintf("%d chunk objects backed up", len(manifest.Objects)))

	// Empty targets: a new database and a new bucket.
	if _, err := h.compose("exec", "-T", "postgres", "createdb", "-U", "flopwire", "flopwire_restore"); err != nil {
		t.Fatal(err)
	}
	if err := h.s3.MakeBucket(h.ctx, "flopwire-restore", minio.MakeBucketOptions{}); err != nil {
		t.Fatal(err)
	}
	restoreDB := fmt.Sprintf("postgres://flopwire:%s@postgres:5432/flopwire_restore?sslmode=disable", pw)
	start = time.Now()
	if _, err := h.compose("run", "--rm", "--no-deps", "-v", mount, "-e", "DATABASE_URL="+restoreDB, "-e", "S3_BUCKET=flopwire-restore",
		"flopwire", "restore", "--input", "/backup/b1"); err != nil {
		t.Fatal(err)
	}
	r.Timings["restore"] = ms(time.Since(start))

	name := project + "-restored"
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if _, err := h.compose("run", "-d", "--no-deps", "--name", name, "-p", "127.0.0.1:"+port+":8080",
		"-e", "DATABASE_URL="+restoreDB, "-e", "S3_BUCKET=flopwire-restore", "flopwire", "serve"); err != nil {
		t.Fatal(err)
	}
	// The restored server shares the TLS volume, so devices keep their pin.
	url := "https://127.0.0.1:" + port
	eventually(t, 90*time.Second, 250*time.Millisecond, "restored server ready", func() (bool, error) { return h.ready(url), nil })

	// Device d's credential works against the restored server.
	var cfg map[string]any
	raw, err = os.ReadFile(d.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["server"] = url
	restoredCfg := filepath.Join(dir, "restored-client.json")
	raw, _ = json.Marshal(cfg)
	writeFile(t, restoredCfg, string(raw))
	env := append(d.env(), "FLOPWIRE_CONFIG="+restoredCfg)
	restored := func(args ...string) (string, error) { return h.run(env, "", args...) }

	// The same queries answer the same on both servers: a live row, and a
	// superseded one (needle's line was dropped by scenario e's rewrite).
	for _, q := range [][]string{{d.codexNeedle}, {"--include-superseded", needle}, {needle}} {
		args := append([]string{"grep", "--server", "--json", "-F"}, q...)
		want, err := d.cli(args...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := restored(args...)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("grep %v: restored server answers\n%s\nprimary answers\n%s", q, got, want)
		}
	}
	var fr struct{ Hits []json.RawMessage }
	out, _ = restored("grep", "--server", "--json", "-F", d.codexNeedle)
	if err := json.Unmarshal([]byte(out), &fr); err != nil || len(fr.Hits) != 1 {
		t.Fatalf("find on the restored server: %v %s", err, out)
	}
	if out, err = restored("search", "--server", "--json", "login", "test", "flake"); err != nil || !strings.Contains(out, "message_id") {
		t.Fatalf("search on the restored server: %v %s", err, out)
	}
	// raw() reads restored chunks (and provisional tails) byte for byte.
	id, gen, size, err := h.latestGen(d.id, d.claudePath)
	if err != nil {
		t.Fatal(err)
	}
	want, err := d.cli("raw", "--server", id, fmt.Sprint(gen), "0", fmt.Sprint(size))
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored("raw", "--server", id, fmt.Sprint(gen), "0", fmt.Sprint(size))
	if err != nil {
		t.Fatal(err)
	}
	if got != want || int64(len(got)) != size {
		t.Fatalf("raw on the restored server: %d bytes, primary %d, size %d", len(got), len(want), size)
	}
}
