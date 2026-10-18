package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/recoveryreceipt"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

const importedReceiptID = "11111111-2222-4333-8444-555555555555"

func exportReceiptFixture(t *testing.T, originalPaths ...string) (string, cassimport.Manifest) {
	t.Helper()
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "cass.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents(id INTEGER,slug TEXT);CREATE TABLE workspaces(id INTEGER,path TEXT);CREATE TABLE conversations(id INTEGER,agent_id INTEGER,workspace_id INTEGER,external_id TEXT,title TEXT,source_path TEXT,started_at INTEGER,ended_at INTEGER,source_id TEXT,origin_host TEXT,metadata_json TEXT,metadata_bin BLOB);CREATE TABLE messages(id INTEGER,conversation_id INTEGER,idx INTEGER,role TEXT,author TEXT,created_at INTEGER,content TEXT,extra_json TEXT,extra_bin BLOB);INSERT INTO agents VALUES(1,'claude_code');INSERT INTO workspaces VALUES(1,'/work/recovered');`)
	if err != nil {
		t.Fatal(err)
	}
	original := "/gone/original.jsonl"
	if len(originalPaths) > 0 {
		original = originalPaths[0]
	}
	_, err = db.Exec(`INSERT INTO conversations VALUES(42,1,1,?,'private title',?,1700000000000,1700000001000,'local','original-mac',NULL,NULL);`, importedReceiptID, original)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO messages VALUES(4,42,7,'user',NULL,1700000000001,'private transcript content never belongs in receipts',NULL,NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "export")
	manifest, err := cassimport.Export(context.Background(), filepath.Join(root, "cass.db"), out, "original-mac", []int64{42})
	if err != nil {
		t.Fatal(err)
	}
	return out, *manifest
}
func receiptSyncer(t *testing.T, transport syncproto.Transport) (*devicesync.Store, *devicesync.Syncer) {
	t.Helper()
	root := t.TempDir()
	store, err := devicesync.OpenStore(filepath.Join(root, "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	spool, err := devicesync.OpenSpool(filepath.Join(root, "spool"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, store, spool, transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sy.Close)
	return store, sy
}
func TestRecoveryImportReceiptUsesVerifiedSnapshotAndActualAcknowledgement(t *testing.T) {
	ctx := context.Background()
	root, manifest := exportReceiptFixture(t)
	server := synctest.New("current-device-token")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	transport := &syncproto.Client{Server: httpServer.URL, Token: server.Token, HTTP: httpServer.Client()}
	store, sy := receiptSyncer(t, transport)
	configDir := t.TempDir()
	binding := recoveryreceipt.Binding{Server: httpServer.URL, DeviceID: "current-device"}
	err := withRecoverySnapshots(ctx, root, manifest, func(snapshots []recoverySnapshot) error {
		// Change the original after verification. Provenance and capture must use
		// the staged export rather than this unrelated replacement.
		if err := os.WriteFile(filepath.Join(root, manifest.Entries[0].File), []byte("unrelated replacement"), 0600); err != nil {
			return err
		}
		return uploadRecoverySnapshot(ctx, root, configDir, binding, store, sy, snapshots[0])
	})
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := recoveryreceipt.Read(configDir, binding, importedReceiptID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%+v,%v", receipts, err)
	}
	r := receipts[0]
	if r.OriginalPath != "/gone/original.jsonl" || r.Source.Path != filepath.Join(root, manifest.Entries[0].File) || r.Source.Generation != 0 {
		t.Fatalf("receipt=%+v", r)
	}
	body, err := server.Reconstruct(r.Source.Path, r.Source.FileID, r.Source.Generation)
	if err != nil || !strings.Contains(string(body), "private transcript content") {
		t.Fatalf("actual uploaded source=%s,%v", body, err)
	}
	if _, err := recoveryreceipt.Read(configDir, recoveryreceipt.Binding{Server: binding.Server, DeviceID: "other-recovery-device"}, importedReceiptID); !errors.Is(err, recoveryreceipt.ErrAbsent) {
		t.Fatalf("wrong device receipt=%v", err)
	}
}
func TestRecoveryImportReceiptFailureReportsSuccessfulUpload(t *testing.T) {
	ctx := context.Background()
	root, manifest := exportReceiptFixture(t)
	server := synctest.New("device-token")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	store, sy := receiptSyncer(t, &syncproto.Client{Server: httpServer.URL, Token: server.Token, HTTP: httpServer.Client()})
	configDir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(configDir, "recovery-policy-receipts")); err != nil {
		t.Fatal(err)
	}
	binding := recoveryreceipt.Binding{Server: httpServer.URL, DeviceID: "device"}
	err := withRecoverySnapshots(ctx, root, manifest, func(snapshots []recoverySnapshot) error {
		return uploadRecoverySnapshot(ctx, root, configDir, binding, store, sy, snapshots[0])
	})
	if err == nil || !strings.Contains(err.Error(), "upload succeeded but restriction provenance was not recorded") {
		t.Fatalf("failure diagnostic=%v", err)
	}
	spec := devicesync.SourceSpec{Path: filepath.Join(root, manifest.Entries[0].File), Agent: manifest.Entries[0].Agent, StorageKind: cassimport.StorageKind, SessionKey: importedReceiptID, Parser: cassimport.Name}
	identity, identityErr := transcript.StatIdentity(spec.Path)
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	refs, ackErr := store.AcknowledgedSourceRefs(ctx, spec, identity)
	if ackErr != nil || len(refs) != 1 {
		t.Fatalf("successful upload was lost: %+v,%v", refs, ackErr)
	}
}
func TestRecoveryImportRefusesCredentialDeviceReplacement(t *testing.T) {
	ctx := context.Background()
	root, manifest := exportReceiptFixture(t)
	server := synctest.New("new-device-token")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	initial := client.Config{Server: httpServer.URL, DeviceID: "original-device", Token: "old-token"}
	binding, _ := recoveryreceipt.NormalizeBinding(recoveryreceipt.Binding{Server: initial.Server, DeviceID: initial.DeviceID})
	replacement := initial
	replacement.DeviceID = "other-device"
	replacement.Token = server.Token
	loader := recoveryCredentialLoader(binding, func() (client.Config, error) { return replacement, nil })
	if _, err := loader(); err == nil {
		t.Fatal("device replacement accepted")
	}
	transport := newSyncTransport(initial, loader, slog.New(slog.NewTextHandler(io.Discard, nil)))
	store, sy := receiptSyncer(t, transport)
	configDir := t.TempDir()
	err := withRecoverySnapshots(ctx, root, manifest, func(snapshots []recoverySnapshot) error {
		for range 2 {
			if err := uploadRecoverySnapshot(ctx, root, configDir, binding, store, sy, snapshots[0]); err == nil {
				return errors.New("replacement credential received content on retry")
			}
		}
		return errors.New("replacement credential refused")
	})
	if err == nil || strings.Contains(err.Error(), "received content") {
		t.Fatalf("upload with replacement device succeeded: %v", err)
	}
	if _, err := recoveryreceipt.Read(configDir, binding, importedReceiptID); !errors.Is(err, recoveryreceipt.ErrAbsent) {
		t.Fatalf("wrongly bound receipt=%v", err)
	}
	if chunks, bytes := server.UniqueChunks(); chunks != 0 || bytes != 0 {
		t.Fatalf("replacement device received %d chunks/%d bytes", chunks, bytes)
	}
	replacement = initial
	replacement.Token = server.Token
	if got, err := loader(); err != nil || got.DeviceID != initial.DeviceID {
		t.Fatalf("same-device rotation refused: %+v,%v", got.DeviceID, err)
	}
}

func TestRecoveryImportUnavailableProvenancePreservesUpload(t *testing.T) {
	for _, original := range []string{"", "relative/original.jsonl"} {
		t.Run(original, func(t *testing.T) {
			ctx := context.Background()
			root, manifest := exportReceiptFixture(t, original)
			server := synctest.New("device-token")
			httpServer := httptest.NewServer(server)
			defer httpServer.Close()
			store, sy := receiptSyncer(t, &syncproto.Client{Server: httpServer.URL, Token: server.Token, HTTP: httpServer.Client()})
			binding := recoveryreceipt.Binding{Server: httpServer.URL, DeviceID: "device"}
			configDir := t.TempDir()
			err := withRecoverySnapshots(ctx, root, manifest, func(snapshots []recoverySnapshot) error {
				err := uploadRecoverySnapshot(ctx, root, configDir, binding, store, sy, snapshots[0])
				if !errors.Is(err, recoveryreceipt.ErrNotQualifying) {
					return errors.New("missing provenance changed upload behavior")
				}
				spec := devicesync.SourceSpec{Path: filepath.Join(root, manifest.Entries[0].File), Agent: manifest.Entries[0].Agent, StorageKind: cassimport.StorageKind, SessionKey: importedReceiptID, Parser: cassimport.Name}
				refs, ackErr := store.AcknowledgedSourceRefs(ctx, spec, snapshots[0].identity)
				if ackErr != nil || len(refs) != 1 {
					return errors.New("otherwise valid recovery was not acknowledged")
				}
				return err
			})
			if !errors.Is(err, recoveryreceipt.ErrNotQualifying) || !strings.Contains(err.Error(), "unreconciled") {
				t.Fatalf("unavailable provenance diagnostic=%v", err)
			}
			if _, err := recoveryreceipt.Read(configDir, binding, importedReceiptID); !errors.Is(err, recoveryreceipt.ErrAbsent) {
				t.Fatalf("missing provenance produced receipt=%v", err)
			}
		})
	}
}
func TestRecoveryImportHeaderManifestMismatchNeverUploads(t *testing.T) {
	ctx := context.Background()
	root, manifest := exportReceiptFixture(t)
	manifest.Entries[0].SessionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	server := synctest.New("device-token")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	store, sy := receiptSyncer(t, &syncproto.Client{Server: httpServer.URL, Token: server.Token, HTTP: httpServer.Client()})
	err := withRecoverySnapshots(ctx, root, manifest, func(snapshots []recoverySnapshot) error {
		return uploadRecoverySnapshot(ctx, root, t.TempDir(), recoveryreceipt.Binding{Server: httpServer.URL, DeviceID: "device"}, store, sy, snapshots[0])
	})
	if err == nil || errors.Is(err, recoveryreceipt.ErrNotQualifying) {
		t.Fatalf("mismatched header accepted: %v", err)
	}
	if chunks, bytes := server.UniqueChunks(); chunks != 0 || bytes != 0 {
		t.Fatalf("mismatched header uploaded %d chunks/%d bytes", chunks, bytes)
	}
}
