package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/client"
	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

func TestPersistedUploadCannotRestartUnderAnotherAuthority(t *testing.T) {
	dir := t.TempDir()
	db, err := localindex.Open(filepath.Join(dir, "index.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, b := synctest.New("a"), synctest.New("b")
	var blocked atomic.Bool
	blocked.Store(true)
	sa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocked.Load() && strings.Contains(r.URL.Path, "flush") {
			http.Error(w, "A offline", 503)
			return
		}
		a.ServeHTTP(w, r)
	}))
	defer sa.Close()
	sb := httptest.NewServer(b)
	defer sb.Close()
	ca := client.Config{Server: sa.URL, Token: "a", DeviceID: "device-a"}
	open := func(cc client.Config) (*devicesync.Scheduler, func(), func(), error) {
		sc, _, start, stop, err := startSyncFrom(t.Context(), db, dir, 4<<20, 1, syncproto.DeviceDirs{}, quiet, cc, func() (client.Config, error) { return cc, nil })
		return sc, start, stop, err
	}
	path := filepath.Join(dir, "a-only.jsonl")
	content := []byte(strings.Repeat("{\"text\":\"historical A only body\"}\n", 20000))
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	spec := devicesync.SourceSpec{Path: path, Agent: transcript.AgentDevin, StorageKind: transcript.StorageSQLite, Parser: "devin@1", Export: true}
	sc, start, stop, err := open(ca)
	if err != nil {
		t.Fatal(err)
	}
	sc.NotifyExport(spec, content)
	sc.Flush(spec)
	start()
	deadline := time.Now().Add(5 * time.Second)
	for sc.Status().LastError == "" {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("A did not persist backlog")
		}
		time.Sleep(5 * time.Millisecond)
	}
	before := sc.Status().SpoolBytes
	if before == 0 {
		stop()
		t.Fatal("fixture has no durable spooled bytes")
	}
	stop()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, cc := range []client.Config{{Server: sb.URL, Token: "b", DeviceID: "device-b"}, {Server: sa.URL, Token: "b", DeviceID: "different-device"}} {
		_, _, _, err := open(cc)
		if err == nil || !strings.Contains(err.Error(), "belongs to another") {
			t.Fatalf("foreign authority restart = %v", err)
		}
	}
	b.Lock()
	requests := b.Requests
	b.Unlock()
	if requests != 0 {
		t.Fatalf("foreign server received %d requests", requests)
	}

	// A separate index under the same config and enrollment must not sweep
	// the old queue's chunks or reuse its source-id/generation tail names.
	fresh, err := localindex.Open(filepath.Join(dir, "fresh.db"), localindex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, _, startFresh, stopFresh, err := startSyncFrom(t.Context(), fresh, dir, 4<<20, 1, syncproto.DeviceDirs{}, quiet, ca, func() (client.Config, error) { return ca, nil })
	if err != nil {
		fresh.Close()
		t.Fatal(err)
	}
	startFresh()
	stopFresh()
	fresh.Close()
	// A's actual source is gone. Only its retained capture can complete now.
	blocked.Store(false)
	sc, start, stop, err = open(ca)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status().SpoolBytes != before {
		stop()
		t.Fatal("foreign restart modified A spool")
	}
	start()
	defer stop()
	deadline = time.Now().Add(5 * time.Second)
	for {
		got, err := a.Reconstruct(path, "", 0)
		if err == nil && bytes.Equal(got, content) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("A restart lost original capture: %v %+v", err, sc.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
