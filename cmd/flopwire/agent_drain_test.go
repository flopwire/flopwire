package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
)

func TestDrainSyncDoesNotWaitForCaptureConnection(t *testing.T) {
	for _, mode := range []string{"deadline", "cancelled", "cancel-during-wait", "empty-cancelled", "empty"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "sync.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			store, err := devicesync.NewStore(db)
			if err != nil {
				t.Fatal(err)
			}
			spool, err := devicesync.OpenSpool(filepath.Join(dir, "spool"), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			sy, err := devicesync.NewSyncer(devicesync.Config{Logger: log}, store, spool, &syncproto.Client{})
			if err != nil {
				t.Fatal(err)
			}
			defer sy.Close()
			sc := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{})
			if !strings.HasPrefix(mode, "empty") {
				sc.Flush(devicesync.SourceSpec{Path: filepath.Join(dir, "pending.jsonl")})
			}
			held, err := db.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			before := db.Stats().WaitCount
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if strings.HasSuffix(mode, "cancelled") {
				cancel()
			}
			wait := 20 * time.Millisecond
			if mode == "cancel-during-wait" {
				wait = time.Minute
				timer := time.AfterFunc(20*time.Millisecond, cancel)
				defer timer.Stop()
			}
			done := make(chan error, 1)
			go func() { done <- drainSync(ctx, sc, wait, log) }()
			select {
			case err := <-done:
				switch mode {
				case "deadline":
					if err == nil || !strings.Contains(err.Error(), "1 sources queued") {
						t.Fatalf("deadline: %v", err)
					}
				case "empty":
					if err != nil {
						t.Fatalf("empty: %v", err)
					}
				default:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled: %v", err)
					}
				}
			case <-time.After(time.Second):
				cancel()
				held.Close()
				<-done
				t.Fatal("drain ignored wait/cancellation while capture held the connection")
			}
			if db.Stats().WaitCount != before {
				t.Fatal("drain queried optional diagnostics")
			}
		})
	}
}
