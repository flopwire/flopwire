package devicesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// This opt-in test launches externally qualified binaries; it never builds one.
// Its synthetic pending exports qualify CLI conversion and old-process resume,
// not rollback of every schema, provider, policy, or deployed user's history.
func TestLegacyTailCLIRollback(t *testing.T) {
	candidate, legacy := os.Getenv("FLOPWIRE_TAIL_CANDIDATE_CLI"), os.Getenv("FLOPWIRE_TAIL_LEGACY_CLI")
	if candidate == "" && legacy == "" {
		t.Skip("set FLOPWIRE_TAIL_CANDIDATE_CLI and FLOPWIRE_TAIL_LEGACY_CLI for isolated CLI qualification")
	}
	if runtime.GOOS != "linux" {
		t.Skip("CLI rollback qualification runs in the private Linux VM")
	}
	for _, binary := range []string{candidate, legacy} {
		info, err := os.Lstat(binary)
		if !filepath.IsAbs(binary) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			t.Fatalf("qualification binary must be an absolute regular executable: %v", err)
		}
		body, err := os.ReadFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("qualification binary %s SHA256 %x", filepath.Base(binary), sha256.Sum256(body))
	}
	dir := t.TempDir()
	home, configDir := filepath.Join(dir, "home"), filepath.Join(dir, "config")
	binDir, emptyClaude, emptyCodex := filepath.Join(dir, "bin"), filepath.Join(dir, "claude"), filepath.Join(dir, "codex")
	for _, path := range []string{home, configDir, binDir, emptyClaude, emptyCodex} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	canary := filepath.Join(dir, "provider-invoked")
	// PATH contains only these canaries. Neither collector may invoke a
	// provider/helper or discover history outside the explicit empty roots.
	for _, name := range []string{"claude", "codex", "devin", "opencode", "cass"} {
		script := "#!/bin/sh\nprintf invoked >> '" + canary + "'\nexit 97\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	dbPath, spoolDir := filepath.Join(dir, "index.db"), filepath.Join(configDir, "spool")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	spool, err := OpenSpool(spoolDir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := NewSyncer(Config{Chunk: small, SealAfter: -1}, store, spool, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	type fixture struct {
		spec       SourceSpec
		sid        int64
		tail       syncproto.Tail
		payload    []byte
		state      []byte
		watermark  []byte
		storedSpec []byte
		entries    []syncproto.Entry
	}
	var fixtures []fixture
	for _, name := range []string{"stale-canonical", "absent-canonical"} {
		f := fixture{spec: SourceSpec{Path: filepath.Join(dir, "synthetic.db") + "#" + name, Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin-export@2", Export: true, SessionKey: name}, state: []byte("synthetic-continued-export")}
		f.payload = bytes.Repeat([]byte("{\"synthetic\":\"literal pending export record\"}\n"), 1000)
		f.payload = append(f.payload, []byte("{\"synthetic\":\"final provisional fragment\"}\n")...)
		src := tailCaptureSource(t, store, f.spec)
		if err := sy.capture(t.Context(), src, func(context.Context, []byte) (Export, error) {
			return Export{Data: f.payload, State: f.state}, nil
		}, -1, nil); err != nil {
			t.Fatal(err)
		}
		g, err := store.gen(t.Context(), src.ID, 0)
		if err != nil || g == nil || g.Size != int64(len(f.payload)) || g.Entries == 0 || g.Tail.Size == 0 || g.Acked != 0 || g.TailAcked || g.Lost || g.Closed || g.FileID != "" {
			t.Fatalf("actual pending capture: %+v, %v", g, err)
		}
		f.sid, f.tail = src.ID, g.Tail
		f.entries, err = store.entries(t.Context(), src.ID, 0, 0, 10000)
		if err != nil || int64(len(f.entries)) != g.Entries {
			t.Fatalf("captured entries: %v", err)
		}
		var offset int64
		for _, entry := range f.entries {
			if entry.Offset != offset || entry.Size <= 0 || entry.End() > int64(len(f.payload)) || entry.Hash != syncproto.Sum(f.payload[entry.Offset:entry.End()]) {
				t.Fatal("manifest differs from literal contiguous payload")
			}
			offset = entry.End()
		}
		if f.tail.Offset != offset || f.tail.Size != int64(len(f.payload))-offset || f.tail.Hash != syncproto.Sum(f.payload[offset:]) {
			t.Fatal("tail differs from literal remaining payload")
		}
		if err := store.db.QueryRow(`SELECT spec,watermark FROM devsync_sources WHERE id=?`, f.sid).Scan(&f.storedSpec, &f.watermark); err != nil {
			t.Fatal(err)
		}
		version := reconcileVersionPath(spool, f.sid, 0, f.tail.Hash)
		assertReconcileFile(t, version, f.payload[offset:])
		if err := spool.PutTailVersion(f.sid, 0, syncproto.Sum([]byte("uncommitted attempt")), []byte("uncommitted attempt")); err != nil {
			t.Fatal(err)
		}
		if name == "stale-canonical" {
			if err := spool.PutTail(f.sid, 0, []byte("stale canonical bytes")); err != nil {
				t.Fatal(err)
			}
		}
		fixtures = append(fixtures, f)
	}
	sy.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.json")
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + configDir, "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"), "XDG_DATA_HOME=" + filepath.Join(dir, "data"), "TMPDIR=" + dir, "PATH=" + binDir, "FLOPWIRE_CONFIG=" + configPath, "FLOPWIRE_INDEX=" + dbPath, "FLOPWIRE_CLOUD=off"}
	runCLI := func(binary string, args ...string) ([]byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env, cmd.Dir, cmd.WaitDelay = env, home, 3*time.Second
		return cmd.CombinedOutput()
	}
	beforeDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	prepareArgs := []string{"agent", "prepare-legacy-tails", "--db", dbPath, "--spool", spoolDir, "--json"}
	// Exercise the real command's exclusive maintenance ownership first.
	lock, err := localindex.AcquireMaintenanceLock(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, refused := runCLI(candidate, prepareArgs...)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if refused == nil {
		t.Fatal("CLI preparation ignored an existing index owner")
	}
	for pass := 0; pass < 2; pass++ {
		output, err := runCLI(candidate, prepareArgs...)
		if err != nil {
			t.Fatalf("candidate CLI preparation: %v, %s", err, output)
		}
		var result LegacyTailPreparation
		if err := json.Unmarshal(output, &result); err != nil || result.Needed != 2 || result.Unspooled != 0 || pass == 0 && result.Converted != 2 || pass == 1 && result.Converted != 0 {
			t.Fatalf("candidate CLI aggregate result: %+v, %v, %s", result, err, output)
		}
		afterDB, err := os.ReadFile(dbPath)
		if err != nil || !bytes.Equal(beforeDB, afterDB) {
			t.Fatalf("preparation changed SQLite bytes: %v", err)
		}
		for _, f := range fixtures {
			assertReconcileFile(t, filepath.Join(spoolDir, "tails", fmt.Sprintf("%d-0", f.sid)), f.payload[f.tail.Offset:])
		}
	}

	const token = "synthetic-cli-rollback-token"
	srv := synctest.New(token)
	var requestsMu sync.Mutex
	var unexpected []string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "synthetic credential required", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/policy":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"path_rules":[],"unplaceable":"upload"}`)
		case syncproto.PathHas, syncproto.PathFlush:
			srv.ServeHTTP(w, r)
		default:
			requestsMu.Lock()
			unexpected = append(unexpected, r.URL.Path)
			requestsMu.Unlock()
			http.NotFound(w, r)
		}
	}))
	defer httpServer.Close()
	config, err := json.Marshal(map[string]string{"server": httpServer.URL, "token": token, "device_id": "00000000-0000-4000-8000-000000000001", "unplaceable": "upload"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"agent", "run", "--once", "--db", dbPath, "--socket", filepath.Join(dir, "agent.sock"), "--sync-timeout", "20s", "--workers", "1", "--claude-projects", emptyClaude, "--codex-home", emptyCodex, "--devin-db", "-", "--opencode-db", "-", "--desktop-code-root", "-", "--cowork-root", "-"}
	for pass := 0; pass < 2; pass++ {
		output, err := runCLI(legacy, args...)
		if err != nil {
			t.Fatalf("legacy CLI pass %d: %v, %s", pass, err, output)
		}
		if !bytes.Contains(output, []byte("pass done")) {
			t.Fatalf("legacy CLI did not complete its collection pass: %s", output)
		}
		db := tailPreparationReadOnly(t, dbPath)
		for _, f := range fixtures {
			got, err := srv.Reconstruct(f.spec.Path, "", 0)
			if err != nil || !bytes.Equal(got, f.payload) {
				t.Fatalf("legacy CLI original-generation payload: %v", err)
			}
			var current, size, entries, acked, genCount, manifestCount int64
			var tailAcked, lost bool
			var spec, watermark []byte
			if err := db.QueryRow(`SELECT s.generation,s.spec,s.watermark,g.size,g.entries,g.acked,g.tail_acked,g.lost FROM devsync_sources s JOIN devsync_gens g ON g.source_id=s.id AND g.generation=s.generation WHERE s.id=?`, f.sid).Scan(&current, &spec, &watermark, &size, &entries, &acked, &tailAcked, &lost); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM devsync_gens WHERE source_id=?`, f.sid).Scan(&genCount); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM devsync_manifest WHERE source_id=?`, f.sid).Scan(&manifestCount); err != nil {
				t.Fatal(err)
			}
			// RepoSent may be added to the stored spec after flush; compare
			// the immutable source descriptor rather than JSON formatting.
			var original, resumed storedSpec
			if err := json.Unmarshal(f.storedSpec, &original); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(spec, &resumed); err != nil {
				t.Fatal(err)
			}
			if current != 0 || genCount != 1 || size != int64(len(f.payload)) || entries != int64(len(f.entries)) || acked != entries || !tailAcked || lost || manifestCount != entries || !bytes.Equal(watermark, f.watermark) || !reflect.DeepEqual(original.SourceSpec, resumed.SourceSpec) {
				t.Fatal("legacy CLI changed capture identity/continuation or did not durably acknowledge the original generation")
			}
			assertReconcileFile(t, filepath.Join(spoolDir, "tails", fmt.Sprintf("%d-0", f.sid)), f.payload[f.tail.Offset:])
		}
		var pending int
		if err := db.QueryRow(`SELECT count(*) FROM devsync_gens WHERE lost=0 AND (acked<entries OR tail_acked=0)`).Scan(&pending); err != nil || pending != 0 {
			t.Fatalf("legacy CLI pending bytes remain: %d, %v", pending, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(canary); !os.IsNotExist(err) {
			t.Fatalf("legacy CLI invoked an external provider/helper: %v", err)
		}
		if !bytes.Equal(config, mustTailCLIRead(t, configPath)) {
			t.Fatal("legacy CLI changed isolated credential/config state")
		}
	}
	requestsMu.Lock()
	defer requestsMu.Unlock()
	if len(unexpected) != 0 {
		t.Fatalf("legacy CLI made unexpected HTTP requests: %v", unexpected)
	}
}

func mustTailCLIRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
