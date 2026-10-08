package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func cliTailFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("literal synthetic SQLite export tail\n")
	hash := syncproto.Sum(body)
	// This existing state deliberately has no index schema/migration. The
	// maintenance command must query it without creating unrelated tables.
	_, err = db.Exec(`CREATE TABLE sentinel(value TEXT); INSERT INTO sentinel VALUES ('retained');
 CREATE TABLE devsync_sources(id INTEGER PRIMARY KEY,generation INTEGER,spec TEXT,watermark TEXT);
 CREATE TABLE devsync_gens(source_id INTEGER,generation INTEGER,size INTEGER,tail_offset INTEGER,tail_size INTEGER,tail_hash BLOB,tail_acked INTEGER,closed INTEGER,lost INTEGER);`)
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"Path":"synthetic-only-export","StorageKind":"sqlite","Export":true}`
	wm, _ := json.Marshal(map[string]any{"Offset": len(body)})
	if _, err := db.Exec(`INSERT INTO devsync_sources VALUES(1,0,?,?)`, spec, string(wm)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO devsync_gens VALUES(1,0,?,0,?,?,0,0,0)`, len(body), len(body), hash[:]); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	spoolDir := filepath.Join(dir, "spool")
	spool, err := devicesync.OpenSpool(spoolDir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.PutTailVersion(1, 0, hash, body); err != nil {
		t.Fatal(err)
	}
	return path, spoolDir, body
}

func TestAgentPrepareLegacyTailsOfflineAndAggregateOutput(t *testing.T) {
	path, spoolDir, body := cliTailFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := agentPrepareLegacyTails(t.Context(), []string{"--db", path, "--spool", spoolDir, "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var result devicesync.LegacyTailPreparation
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Converted != 1 || result.Needed != 1 {
		t.Fatalf("aggregate result: %s %v", out.String(), err)
	}
	if strings.Contains(out.String(), path) || strings.Contains(out.String(), string(body)) || strings.Contains(out.String(), "synthetic-only-export") {
		t.Fatal("maintenance exposed source identity or bytes")
	}
	spool, err := devicesync.OpenSpool(spoolDir, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := spool.Tail(1, 0); err != nil || !found || !bytes.Equal(got, body) {
		t.Fatal("old Tail reader cannot read prepared canonical bytes")
	}
	out.Reset()
	if err := agentPrepareLegacyTails(t.Context(), []string{"--db", path, "--spool", spoolDir}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "converted 0; canonical 1") || !strings.Contains(out.String(), "policy and schema compatibility require separate qualification") {
		t.Fatal("idempotent text result lacks bounded format statement")
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("offline command changed live SQLite bytes")
	}
}

func TestAgentPrepareLegacyTailsRefusesLiveLock(t *testing.T) {
	path, spoolDir, _ := cliTailFixture(t)
	lock, err := localindex.AcquireMaintenanceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out bytes.Buffer
	if err := agentPrepareLegacyTails(t.Context(), []string{"--db", path, "--spool", spoolDir}, &out); err == nil || !strings.Contains(err.Error(), "stopped collector") {
		t.Fatalf("live ownership not refused: %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("refused preparation reported success")
	}
	if _, err := os.Stat(filepath.Join(spoolDir, "tails", "1-0")); !os.IsNotExist(err) {
		t.Fatal("lock refusal converted the spool")
	}
}

func TestAgentPrepareLegacyTailsDefaultsAndInvalidArguments(t *testing.T) {
	path, spoolDir, _ := cliTailFixture(t)
	t.Setenv("FLOPWIRE_INDEX", path)
	t.Setenv("FLOPWIRE_CONFIG", filepath.Join(filepath.Dir(spoolDir), "config.json"))
	var out bytes.Buffer
	if err := agentPrepareLegacyTails(t.Context(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "converted 1") {
		t.Fatal("default index/config spool were not selected")
	}
	if err := agentPrepareLegacyTails(t.Context(), []string{"extra"}, &out); err == nil {
		t.Fatal("positional argument ignored")
	}
	if err := agentPrepareLegacyTails(t.Context(), []string{"--db", filepath.Join(t.TempDir(), "missing.db"), "--spool", spoolDir}, &out); err == nil {
		t.Fatal("missing database was created")
	}
}
