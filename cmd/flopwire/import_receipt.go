package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/flopwire/flopwire/internal/cassimport"
	"os"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/recoveryreceipt"
	"github.com/flopwire/flopwire/internal/transcript"
)

// recoveryCredentialLoader prevents a later device login from changing the
// credential ownership of the import's persisted acknowledgement state.
func recoveryCredentialLoader(binding recoveryreceipt.Binding, load func() (client.Config, error)) func() (client.Config, error) {
	return func() (client.Config, error) {
		cfg, err := load()
		if err != nil {
			return client.Config{}, err
		}
		got, err := recoveryreceipt.NormalizeBinding(recoveryreceipt.Binding{Server: cfg.Server, DeviceID: cfg.DeviceID})
		if err != nil || got != binding {
			return client.Config{}, errors.New("recovery upload credential device/server changed; restart import with that device")
		}
		return cfg, nil
	}
}

func persistRecoveryReceipt(ctx context.Context, configDir string, binding recoveryreceipt.Binding, store *devicesync.Store, spec devicesync.SourceSpec, original string, identity transcript.Identity) error {
	if original == "" {
		return nil
	}
	refs, err := store.AcknowledgedSourceRefs(ctx, spec, identity)
	if err != nil {
		return fmt.Errorf("recovery upload succeeded but restriction provenance was not recorded: %w", err)
	}
	if len(refs) == 0 {
		return errors.New("recovery upload returned without a fully acknowledged source; restriction provenance was not recorded")
	}
	for _, ref := range refs {
		receipt := recoveryreceipt.Receipt{NativeSessionID: spec.SessionKey, OriginalPath: original, Source: ref}
		if err := recoveryreceipt.Write(configDir, binding, receipt); err != nil {
			return fmt.Errorf("recovery upload succeeded but restriction provenance was not recorded: %w", err)
		}
	}
	return nil
}

func uploadRecoverySnapshot(ctx context.Context, root, configDir string, binding recoveryreceipt.Binding, store *devicesync.Store, sy *devicesync.Syncer, snapshot recoverySnapshot) error {
	staged, err := os.Open(snapshot.path)
	if err != nil {
		return err
	}
	original, provenanceErr := recoveryreceipt.SnapshotProvenance(staged, snapshot.entry)
	closeErr := staged.Close()
	if closeErr != nil {
		return closeErr
	}
	if provenanceErr != nil && !errors.Is(provenanceErr, recoveryreceipt.ErrNotQualifying) {
		return provenanceErr
	}
	e := snapshot.entry
	spec := devicesync.SourceSpec{Path: filepath.Join(root, e.File), Agent: e.Agent, StorageKind: cassimport.StorageKind, SessionKey: e.SessionID, Parser: cassimport.Name}
	if err = sy.SyncSnapshot(ctx, spec, snapshot.path, snapshot.identity); err != nil {
		return err
	}
	if errors.Is(provenanceErr, recoveryreceipt.ErrNotQualifying) {
		return recoveryreceipt.ErrNotQualifying
	}
	return persistRecoveryReceipt(ctx, configDir, binding, store, spec, original, snapshot.identity)
}
