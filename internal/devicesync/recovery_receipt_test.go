package devicesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flopwire/flopwire/internal/transcript"
)

const recoveryReceiptID = "11111111-2222-4333-8444-555555555555"

func receiptSpec(e *env) SourceSpec {
	return SourceSpec{Path: e.path("cass.jsonl"), Agent: transcript.AgentClaude, StorageKind: "cass_export", Parser: "cass@1", SessionKey: recoveryReceiptID}
}
func TestAcknowledgedRecoveryRequiresActualSuccessfulSnapshot(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	spec := receiptSpec(e)
	ctx := context.Background()
	data := jsonlLines(19, 50, 100)
	if err := os.WriteFile(spec.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := transcript.StatIdentity(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "verified.jsonl")
	if err := os.WriteFile(snapshot, data, 0400); err != nil {
		t.Fatal(err)
	}
	e.srv.SetDropAck(true)
	if err := e.sy.SyncSnapshot(ctx, spec, snapshot, identity); err == nil {
		t.Fatal("expected missing acknowledgement")
	}
	refs, err := e.store.AcknowledgedSourceRefs(ctx, spec, identity)
	if err != nil || len(refs) != 0 {
		t.Fatalf("unacknowledged receipt=%+v,%v", refs, err)
	}
	e.srv.SetDropAck(false)
	if err := e.sy.SyncSnapshot(ctx, spec, snapshot, identity); err != nil {
		t.Fatal(err)
	}
	refs, err = e.store.AcknowledgedSourceRefs(ctx, spec, identity)
	if err != nil || len(refs) != 1 {
		t.Fatalf("acknowledged receipt=%+v,%v", refs, err)
	}
	if refs[0].FileID != identity.ID.String() || refs[0].Generation != 0 {
		t.Fatalf("receipt identity=%+v", refs[0])
	}
	e.requireServerHas(spec.Path, identity.ID.String(), 0, data)
	changed := spec
	changed.Parent = "/unrelated/native.jsonl"
	if _, err = e.store.AcknowledgedSourceRefs(ctx, changed, identity); err == nil {
		t.Fatal("changed source association accepted")
	}
	// A newer unshared snapshot cannot inherit the old acknowledged generation.
	if err := os.Chmod(snapshot, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, append(data, []byte("new bytes\n")...), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec.Path, append(data, []byte("new bytes\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err = transcript.StatIdentity(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	e.srv.SetDown(true)
	if err := e.sy.SyncSnapshot(ctx, spec, snapshot, identity); err == nil {
		t.Fatal("expected failed new snapshot")
	}
	refs, err = e.store.AcknowledgedSourceRefs(ctx, spec, identity)
	if err != nil || len(refs) != 0 {
		t.Fatalf("older acknowledgement reused: %+v,%v", refs, err)
	}
}
func TestAcknowledgedRecoveryEmptyExportFileID(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	spec := receiptSpec(e)
	spec.Export = true
	data := []byte("{\"verified\":true}\n")
	if err := os.WriteFile(spec.Path, data, 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := transcript.StatIdentity(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	identity.ID = transcript.FileID{}
	if err := e.sy.SyncSnapshot(context.Background(), spec, spec.Path, identity); err != nil {
		t.Fatal(err)
	}
	refs, err := e.store.AcknowledgedSourceRefs(context.Background(), spec, identity)
	if err != nil || len(refs) != 1 || refs[0].FileID != "" {
		t.Fatalf("empty export identity=%+v,%v", refs, err)
	}
}
func TestAcknowledgedRecoveryRejectsUnrelatedSource(t *testing.T) {
	e := newEnv(t, Config{}, 1<<20)
	spec := receiptSpec(e)
	for _, alter := range []func(*SourceSpec){func(s *SourceSpec) { s.Agent = transcript.AgentCodex }, func(s *SourceSpec) { s.StorageKind = transcript.StorageJSONLAppend }, func(s *SourceSpec) { s.Parser = "claude@1" }, func(s *SourceSpec) { s.SessionKey = "" }} {
		changed := spec
		alter(&changed)
		if _, err := e.store.AcknowledgedSourceRefs(context.Background(), changed, transcript.Identity{}); err == nil {
			t.Fatal("unrelated source accepted")
		}
	}
	refs, err := e.store.AcknowledgedSourceRefs(context.Background(), spec, transcript.Identity{})
	if err != nil || len(refs) != 0 {
		t.Fatalf("uncaptured source=%+v,%v", refs, err)
	}
}
