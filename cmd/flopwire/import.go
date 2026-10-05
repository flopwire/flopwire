package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/flopwire/flopwire/internal/cassimport"
	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
)

func importCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flopwire import cass|upload [flags]")
	}
	fs := flag.NewFlagSet("import "+args[0], flag.ContinueOnError)
	switch args[0] {
	case "cass":
		database := fs.String("db", "", "read-only CASS database snapshot")
		dest := fs.String("out", "", "new private export directory")
		origin := fs.String("origin", "", "stable origin machine name")
		selection := fs.String("selection", "", "JSON array of CASS conversation IDs confirmed absent from native history")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *database == "" || *dest == "" || *origin == "" || *selection == "" || fs.NArg() != 0 {
			return fmt.Errorf("import cass requires --db, --out, --origin and --selection")
		}
		b, err := os.ReadFile(*selection)
		if err != nil {
			return err
		}
		var ids []int64
		if err = json.Unmarshal(b, &ids); err != nil {
			return err
		}
		m, err := cassimport.Export(ctx, *database, *dest, *origin, ids)
		if err != nil {
			return err
		}
		var messages int64
		for _, e := range m.Entries {
			messages += e.Messages
		}
		return json.NewEncoder(out).Encode(map[string]any{"exported_conversations": len(m.Entries), "exported_messages": messages, "origin": m.Origin, "uploaded": false})
	case "upload":
		dir := fs.String("dir", "", "private CASS export directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *dir == "" || fs.NArg() != 0 {
			return fmt.Errorf("import upload requires --dir")
		}
		root, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
		if err != nil {
			return err
		}
		var m cassimport.Manifest
		if err = json.Unmarshal(b, &m); err != nil {
			return err
		}
		if m.Version != 1 || m.Origin == "" || len(m.Entries) == 0 {
			return fmt.Errorf("invalid recovery manifest")
		}
		return withRecoverySnapshots(ctx, root, m, func(snapshots []recoverySnapshot) error {
			cfg, err := client.Load()
			if err != nil {
				return err
			}
			if cfg.DeviceID == "" || cfg.Token == "" {
				return fmt.Errorf("enroll a recovery device before uploading")
			}
			// Per-device state prevents acknowledged work under one credential from
			// being mistaken for work uploaded by a different device.
			stateKey := sha256.Sum256([]byte(cfg.Server + "\x00" + cfg.DeviceID))
			state := filepath.Join(root, ".sync-"+hex.EncodeToString(stateKey[:]))
			if err = os.MkdirAll(state, 0700); err != nil {
				return err
			}
			st, err := devicesync.OpenStore(filepath.Join(state, "sync.db"))
			if err != nil {
				return err
			}
			defer st.Close()
			spool, err := devicesync.OpenSpool(filepath.Join(state, "spool"), 1<<30)
			if err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			sy, err := devicesync.NewSyncer(devicesync.Config{Logger: log}, st, spool, newSyncTransport(cfg, client.Load, log))
			if err != nil {
				return err
			}
			defer sy.Close()
			var uploaded int
			for _, snapshot := range snapshots {
				e := snapshot.entry
				if err = sy.SyncSnapshot(ctx, devicesync.SourceSpec{Path: filepath.Join(root, e.File), Agent: e.Agent, StorageKind: cassimport.StorageKind, SessionKey: e.SessionID, Parser: cassimport.Name}, snapshot.path, snapshot.identity); err != nil {
					return err
				}
				uploaded++
			}
			_, n := sy.Refused()
			if n > 0 {
				return fmt.Errorf("server collection policy refused %d recovery sources", n)
			}
			return json.NewEncoder(out).Encode(map[string]any{"uploaded_conversations": uploaded, "origin": m.Origin, "indexing": "asynchronous; verify server counts before retiring CASS"})
		})
	default:
		return fmt.Errorf("unknown import command %q", args[0])
	}
}
