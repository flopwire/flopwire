package ingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/agent"
	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/domain"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/recoveryreceipt"
	"github.com/flopwire/flopwire/internal/retrieval/format"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/google/uuid"
)

const receiptChainText = "same device recovered receipt lifecycle evidence"
const receiptChainOriginal = "/gone/receipt-native/" + chainNativeID + ".jsonl"

type receiptChainArchive struct {
	ref      syncproto.PolicySource
	original string
	body     []byte
	sourceID string
}

// This database is a synthetic CASS input. Recovery provenance is generated
// by the real exporter and parsed from uploaded bytes, never inserted into the
// local index or PostgreSQL evidence tables by the test.
func receiptChainExport(t *testing.T) (string, *cassimport.Manifest) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "cass.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents(id INTEGER,slug TEXT);CREATE TABLE workspaces(id INTEGER,path TEXT);CREATE TABLE conversations(id INTEGER,agent_id INTEGER,workspace_id INTEGER,external_id TEXT,title TEXT,source_path TEXT,started_at INTEGER,ended_at INTEGER,source_id TEXT,origin_host TEXT,metadata_json TEXT,metadata_bin BLOB);CREATE TABLE messages(id INTEGER,conversation_id INTEGER,idx INTEGER,role TEXT,author TEXT,created_at INTEGER,content TEXT,extra_json TEXT,extra_bin BLOB);INSERT INTO agents VALUES(1,'claude_code');INSERT INTO workspaces VALUES(1,'/synthetic/recovery-workspace');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO conversations VALUES(42,1,1,?,'Synthetic recovery',?,1700000000000,1700000001000,'local','synthetic-original',NULL,NULL)`, chainNativeID, receiptChainOriginal)
	if err == nil {
		_, err = db.Exec(`INSERT INTO messages VALUES(4,42,0,'user',NULL,1700000000001,?,NULL,NULL)`, receiptChainText)
	}
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "export")
	manifest, err := cassimport.Export(context.Background(), dbPath, output, "synthetic-original", []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	return output, manifest
}

func receiptChainUpload(t *testing.T, e *chainEnv, d *chainDevice, deviceID, receiptDir string, writeReceipt bool) receiptChainArchive {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root, manifest := receiptChainExport(t)
	entry := manifest.Entries[0]
	originalPath := filepath.Join(root, entry.File)
	original, err := os.Open(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := original.Stat()
	if err != nil {
		original.Close()
		t.Fatal(err)
	}
	identity := transcript.IdentityOf(info)
	stagedPath := filepath.Join(t.TempDir(), "verified.jsonl")
	staged, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		original.Close()
		t.Fatal(err)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(staged, hash), original)
	after, statErr := original.Stat()
	closeErr := errors.Join(staged.Close(), original.Close())
	if err = errors.Join(copyErr, statErr, closeErr); err != nil {
		t.Fatal(err)
	}
	if size != identity.Size || transcript.IdentityOf(after) != identity || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		t.Fatal("synthetic staged recovery checksum or identity changed")
	}
	if err := os.Chmod(stagedPath, 0400); err != nil {
		t.Fatal(err)
	}
	verified, err := os.Open(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	originalHost, err := recoveryreceipt.SnapshotProvenance(verified, entry)
	closeErr = verified.Close()
	if err = errors.Join(err, closeErr); err != nil {
		t.Fatal(err)
	}
	if originalHost != receiptChainOriginal {
		t.Fatalf("verified original path=%q", originalHost)
	}
	expectedBody, err := os.ReadFile(stagedPath)
	if err != nil {
		t.Fatal(err)
	}
	// Import acknowledgement state is separate from the agent's index database.
	// The receipt bridge is therefore required to discover this already-shared copy.
	state := t.TempDir()
	syncStore, err := devicesync.OpenStore(filepath.Join(state, "export-sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer syncStore.Close()
	spool, err := devicesync.OpenSpool(filepath.Join(state, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: d.config.Logger, SealAfter: -1}, syncStore, spool, d.client)
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	spec := devicesync.SourceSpec{Path: originalPath, Agent: entry.Agent, StorageKind: cassimport.StorageKind, SessionKey: entry.SessionID, Parser: cassimport.Name}
	if err = sy.SyncSnapshot(ctx, spec, stagedPath, identity); err != nil {
		t.Fatal(err)
	}
	refs, err := syncStore.AcknowledgedSourceRefs(ctx, spec, identity)
	if err != nil || len(refs) != 1 {
		t.Fatalf("actual import acknowledgement=%+v,%v", refs, err)
	}
	if writeReceipt {
		receipt := recoveryreceipt.Receipt{NativeSessionID: entry.SessionID, OriginalPath: originalHost, Source: refs[0]}
		if err = recoveryreceipt.Write(receiptDir, recoveryreceipt.Binding{Server: d.client.Server, DeviceID: deviceID}, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err = e.queue.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	var sourceID string
	if err = e.pool.QueryRow(ctx, `SELECT id::text FROM sources WHERE device_id=$1 AND path=$2 AND file_id=$3`, deviceID, refs[0].Path, refs[0].FileID).Scan(&sourceID); err != nil {
		t.Fatal(err)
	}
	var storedRecovered bool
	var storedExternal, storedOriginal string
	if err = e.pool.QueryRow(ctx, `SELECT (extra->>'recovered_history')::boolean,extra->>'cass_external_id',extra->>'cass_source_path' FROM conversations WHERE device_id=$1 AND session_id=$2`, deviceID, chainNativeID).Scan(&storedRecovered, &storedExternal, &storedOriginal); err != nil {
		t.Fatal(err)
	}
	if !storedRecovered || storedExternal != chainNativeID || storedOriginal != receiptChainOriginal {
		t.Fatalf("generated uploaded provenance=%t,%q,%q", storedRecovered, storedExternal, storedOriginal)
	}
	return receiptChainArchive{ref: refs[0], original: originalHost, body: expectedBody, sourceID: sourceID}
}

func receiptChainReader(configDir string, binding recoveryreceipt.Binding, attempts *atomic.Int64) func(context.Context, string) ([]syncproto.PolicyRecoverySource, error) {
	return func(ctx context.Context, native string) ([]syncproto.PolicyRecoverySource, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attempts.Add(1)
		receipts, err := recoveryreceipt.Read(configDir, binding, native)
		// Match the production command callback: absence supplies no guessed
		// references; genuine Cowork metadata can still be reconciled.
		if errors.Is(err, recoveryreceipt.ErrAbsent) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		refs := make([]syncproto.PolicyRecoverySource, 0, len(receipts))
		for _, r := range receipts {
			refs = append(refs, syncproto.PolicyRecoverySource{Source: r.Source, OriginalPath: r.OriginalPath})
		}
		return refs, nil
	}
}

func receiptChainRaw(t *testing.T, d *chainDevice, archive receiptChainArchive, visible bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := client.HTTP{Server: d.client.Server, Token: d.client.Token, Client: d.client.HTTP}
	got, err := c.Raw(ctx, archive.sourceID, archive.ref.Generation, 0, int64(len(archive.body)))
	if visible {
		if err != nil || !bytes.Equal(got, archive.body) {
			t.Fatalf("literal recovered raw bytes=%d,%v; want %d", len(got), err, len(archive.body))
		}
		return
	}
	var refused *client.APIError
	if !errors.As(err, &refused) || (refused.StatusCode != 403 && refused.StatusCode != 404) {
		t.Fatalf("hidden recovered raw must be inaccessible: %v", err)
	}
}

func receiptChainHits(t *testing.T, d *chainDevice, deviceID string, want int) {
	t.Helper()
	chainEventually(t, "device-filtered recovery search", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c := client.HTTP{Server: d.client.Server, Token: d.client.Token, Client: d.client.HTTP}
		page, err := c.Grep(ctx, format.GrepQuery{Pattern: receiptChainText, Fixed: true}, format.Filters{Device: deviceID})
		if err != nil {
			return err
		}
		if len(page.Hits) != want {
			return fmt.Errorf("device recovery hits=%d, want %d", len(page.Hits), want)
		}
		return nil
	})
}

func receiptChainHistory(t *testing.T, d *chainDevice, deviceID string, want bool) {
	t.Helper()
	if err := d.index.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A separate read-only connection verifies an on-disk fact, rather than a
	// current Agent object or its in-memory placement cache.
	disk, err := localindex.Open(d.index.Path(), localindex.Options{DeviceID: deviceID, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	have, err := disk.CoworkHistoricalUnknown(context.Background(), chainNativeID)
	if err != nil || have != want {
		t.Fatalf("durable historical unknown=%t,%v; want %t", have, err, want)
	}
}

func receiptChainReopen(t *testing.T, d *chainDevice, deviceID string) {
	t.Helper()
	d.halt(t)
	path := d.index.Path()
	if err := d.index.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := localindex.Open(path, localindex.Options{DeviceID: deviceID, DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	d.index = reopened
}

// Qualification uses synthetic exports on the currently authenticated device.
// Existing production recovery-device archives remain deferred and unreconciled.
func TestCoworkAgentRecoveredReceiptLifecycle(t *testing.T) {
	e := newChainEnv(t)
	h := newChainAPI(t, e, chainCapabilityOverride())
	firstID, firstToken := e.credential(t, "receipt-current")
	otherID, otherToken := e.credential(t, "receipt-other")
	first := newChainDevice(t, firstID, firstToken, h)
	other := newChainDevice(t, otherID, otherToken, h)
	// Keep genuine Cowork metadata but no live original transcript/descendants.
	// The CASS copy cannot be found through a guessed live native path.
	for _, d := range []*chainDevice{first, other} {
		if err := os.RemoveAll(filepath.Dir(d.main)); err != nil {
			t.Fatal(err)
		}
	}
	receiptsDir := t.TempDir()
	firstArchive := receiptChainUpload(t, e, first, firstID, receiptsDir, true)
	otherArchive := receiptChainUpload(t, e, other, otherID, receiptsDir, false)
	receiptChainRaw(t, first, firstArchive, true)
	receiptChainRaw(t, other, otherArchive, true)
	receiptChainHits(t, first, firstID, 1)
	receiptChainHits(t, other, otherID, 1)
	var agentCassCaptures int
	if err := first.syncDB.QueryRow(`SELECT count(*) FROM devsync_sources WHERE json_extract(spec,'$.StorageKind')='cass_export'`).Scan(&agentCassCaptures); err != nil || agentCassCaptures != 0 {
		t.Fatalf("import state leaked into agent DB: %d,%v", agentCassCaptures, err)
	}

	// Neither an absent receipt nor another device's namespace can establish a
	// reference or claim that the old shared copy has been reconciled.
	var attempts atomic.Int64
	absentDir := t.TempDir()
	currentBinding := recoveryreceipt.Binding{Server: h.URL, DeviceID: firstID}
	first.config.RecoveredPolicySources = receiptChainReader(absentDir, currentBinding, &attempts)
	first.start(t)
	first.once(t)
	if attempts.Load() == 0 {
		t.Fatal("agent did not consult the configured receipt bridge")
	}
	if _, err := recoveryreceipt.Read(absentDir, currentBinding, chainNativeID); !errors.Is(err, recoveryreceipt.ErrAbsent) || !strings.Contains(err.Error(), "unreconciled prior recovery") {
		t.Fatalf("absent receipt diagnostic=%v", err)
	}
	first.halt(t)
	attempts.Store(0)
	wrongBinding := recoveryreceipt.Binding{Server: h.URL, DeviceID: otherID}
	first.config.RecoveredPolicySources = receiptChainReader(receiptsDir, wrongBinding, &attempts)
	first.start(t)
	first.once(t)
	if attempts.Load() == 0 {
		t.Fatal("agent did not read exact wrong-device namespace")
	}
	if _, err := recoveryreceipt.Read(receiptsDir, wrongBinding, chainNativeID); !errors.Is(err, recoveryreceipt.ErrAbsent) {
		t.Fatalf("other device inherited current-device receipt: %v", err)
	}
	var registered int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM source_policy_placements WHERE device_id=$1 AND path=$2`, firstID, firstArchive.ref.Path).Scan(&registered); err != nil || registered != 0 {
		t.Fatalf("absent/wrong-device receipt fabricated source ownership: %d,%v", registered, err)
	}
	// The server can legitimately retain its own canonical same-device history
	// fact and hold the copy without treating a missing receipt as source proof.
	var scope string
	if err := e.pool.QueryRow(context.Background(), `SELECT evidence_scope FROM session_policy_placements WHERE device_id=$1 AND session_id=$2`, firstID, chainNativeID).Scan(&scope); err != nil || scope != syncproto.EvidenceUnmapped {
		t.Fatalf("genuine old same-device history was widened: %q,%v", scope, err)
	}
	receiptChainRaw(t, other, otherArchive, true)
	first.halt(t)

	// Only the exact current-device receipt crosses the real authenticated
	// endpoint. Stored source binding proves the exported path/FileID/generation
	// survived the Agent callback and metadata request unchanged.
	attempts.Store(0)
	first.config.RecoveredPolicySources = receiptChainReader(receiptsDir, currentBinding, &attempts)
	first.start(t)
	first.once(t)
	chainEventually(t, "matching receipt device-scoped source binding", func() error {
		var bindings int
		err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM source_policy_placements WHERE device_id=$1 AND session_id=$2 AND path=$3 AND file_id=$4 AND generation=$5`, firstID, chainNativeID, firstArchive.ref.Path, firstArchive.ref.FileID, firstArchive.ref.Generation).Scan(&bindings)
		if err != nil {
			return err
		}
		if bindings != 1 {
			return fmt.Errorf("exact recovered bindings=%d, want 1", bindings)
		}
		var historical string
		var known bool
		if err = e.pool.QueryRow(context.Background(), `SELECT evidence_scope,current_mapping_known FROM session_policy_placements WHERE device_id=$1 AND session_id=$2`, firstID, chainNativeID).Scan(&historical, &known); err != nil {
			return err
		}
		if historical != syncproto.EvidenceUnmapped || !known {
			return fmt.Errorf("current known/historical scope=%t,%q", known, historical)
		}
		return nil
	})
	receiptChainHistory(t, first, firstID, true)
	receiptChainRaw(t, first, firstArchive, false)
	receiptChainHits(t, first, firstID, 0)
	receiptChainRaw(t, other, otherArchive, true)
	receiptChainHits(t, other, otherID, 1)

	// Reopen the actual local index and remove the bridge callback. A current
	// complete mapping and new native bytes must retain historical uncertainty.
	receiptChainReopen(t, first, firstID)
	first.config.RecoveredPolicySources = nil
	newNativeText := "new mapped receipt history stays local after restart"
	chainWrite(t, first.main, chainNativeRecord(chainNativeID, "receipt-after-restart", newNativeText))
	first.start(t)
	first.once(t)
	receiptChainHistory(t, first, firstID, true)
	var localMessages int
	if err := first.index.DB().QueryRow(`SELECT count(*) FROM messages WHERE native_id='receipt-after-restart'`).Scan(&localMessages); err != nil || localMessages != 1 {
		t.Fatalf("new native content was not retained locally: %d,%v", localMessages, err)
	}
	chainEventually(t, "historical recovery scheduler completion", func() error {
		st := first.scheduler.Status()
		if st.Queued != 0 {
			return fmt.Errorf("scheduler has %d queued/running captures", st.Queued)
		}
		if st.ServerDown || st.Stopped != "" || len(st.Failing) > 0 || st.SpoolBlocked {
			return fmt.Errorf("scheduler completion is not clean: %+v", st)
		}
		return nil
	})
	var localNativeCaptures int
	if err := first.syncDB.QueryRow(`SELECT count(*) FROM devsync_sources`).Scan(&localNativeCaptures); err != nil || localNativeCaptures != 0 {
		t.Fatalf("historical receipt scheduled unqualified capture: %d,%v", localNativeCaptures, err)
	}
	var nativeUploads int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM sources WHERE device_id=$1 AND storage_kind<>'cass_export'`, firstID).Scan(&nativeUploads); err != nil || nativeUploads != 0 {
		t.Fatalf("historical receipt widened native upload: %d,%v", nativeUploads, err)
	}
	receiptChainRaw(t, first, firstArchive, false)
	receiptChainHits(t, first, firstID, 0)
	receiptChainRaw(t, other, otherArchive, true)

	// Apply the real collection-policy update and fetch it through the typed
	// admin-rule client. Global exclude must cover historical unknown evidence
	// even while current folders are known. The other device's placed CASS
	// history has no Cowork unknown ledger and remains accessible.
	var credentialID string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM credentials WHERE device_id=$1`, firstID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.store.UpdatePolicyWithAudit(ctx, credentialID, domain.Policy{PathRules: []string{}, Unplaceable: "exclude", UpdatedBy: e.userID}, domain.AuditEvent{ID: uuid.NewString(), ActorID: e.userID, Action: "policy.update", TargetType: "policy", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	receiptChainReopen(t, first, firstID)
	first.config.AdminRules = agent.FetchAdminRules(h.URL, firstToken, h.Client())
	first.start(t)
	first.once(t)
	receiptChainHistory(t, first, firstID, true)
	if err := first.index.DB().QueryRow(`SELECT count(*) FROM messages WHERE native_id='receipt-after-restart'`).Scan(&localMessages); err != nil || localMessages != 0 {
		t.Fatalf("global exclude retained denied local content: %d,%v", localMessages, err)
	}
	var clientMode, historical string
	if err := e.pool.QueryRow(ctx, `SELECT client_mode,evidence_scope FROM session_policy_placements WHERE device_id=$1 AND session_id=$2`, firstID, chainNativeID).Scan(&clientMode, &historical); err != nil || clientMode != syncproto.ClientModeDeny || historical != syncproto.EvidenceUnmapped {
		t.Fatalf("historical exclusion ledger=%q,%q,%v", clientMode, historical, err)
	}
	// Aging only an actual hidden conversation exercises the existing retention
	// purge. All content/provenance still originated from uploaded export bytes.
	if _, err := e.pool.Exec(ctx, `UPDATE conversations SET hidden_at=now()-interval '8 days' WHERE device_id=$1 AND session_id=$2 AND hidden_at IS NOT NULL`, firstID, chainNativeID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.queue.EnforceRules(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM conversations WHERE device_id=$1 AND session_id=$2`, firstID, chainNativeID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("explicitly excluded recovered history was not purged: %d,%v", remaining, err)
	}
	receiptChainHistory(t, first, firstID, true)
	receiptChainRaw(t, other, otherArchive, true)
	receiptChainHits(t, other, otherID, 1)
}
