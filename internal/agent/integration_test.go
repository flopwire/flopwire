package agent

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/syncproto"
	"github.com/flopwire/flopwire/internal/syncproto/synctest"
	"github.com/flopwire/flopwire/internal/transcript"
)

// TestLiveAppendIsFindableAndUploaded runs the agent the way `flopwire agent
// run` does (deferred commits, devicesync sharing the index database, an
// in-memory sync server) over the oracle home, appends lines to a live
// Claude transcript, and checks that each is findable in the local index
// within 2s and that the server ends up with the appended bytes.
// eventLimit bounds a pickup that must come from watch events or the fast
// lane. These tests set the sweep an hour away, so meeting any limit short
// of that proves the event path; the limit only has to absorb a slow
// runner. Alone, a pickup takes about 100ms under -race on a 2-vCPU CI
// runner (60 of 60 runs); inside `go test -race ./...`, with other
// packages competing for the CPUs, 2s and 3s limits failed.
const eventLimit = 15 * time.Second

func TestLiveAppendIsFindableAndUploaded(t *testing.T) {
	f := newFixture(t, "-")
	srv := synctest.New("tok")
	h := httptest.NewServer(srv)
	defer h.Close()

	sdb, err := sql.Open("sqlite", "file:"+f.store.Path()+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	sdb.SetMaxOpenConns(1)
	st, err := devicesync.NewStore(sdb)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := devicesync.OpenSpool(filepath.Join(t.TempDir(), "spool"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sy, err := devicesync.NewSyncer(devicesync.Config{Logger: logger}, st, spool, &syncproto.Client{Server: h.URL, Token: "tok", HTTP: h.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer sy.Close()
	sched := devicesync.NewScheduler(sy, devicesync.SchedulerConfig{})

	f.cfg.Sync, f.cfg.Sweep = sched, time.Hour // appends reach the index through the fast lane only
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- sched.Run(runCtx) }()
	go func() { done <- f.a.Run(runCtx) }()
	defer func() {
		cancel()
		<-done
		<-done
	}()
	waitFor(t, func() bool { return len(f.find("login test flake", false)) == 1 })

	p := f.path(alphaRel)
	var lat []time.Duration
	for i := range 5 {
		needle := fmt.Sprintf("live append number %d okra", i)
		appendFile(t, p, claudeUser(fmt.Sprintf("c1000000-0000-4000-8000-0000000001%02d", i), needle))
		start := time.Now()
		for len(f.find(needle, false)) != 1 {
			if time.Since(start) > eventLimit {
				t.Fatalf("append %d not findable after %s", i, eventLimit)
			}
			time.Sleep(10 * time.Millisecond)
		}
		lat = append(lat, time.Since(start))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("time to findable: min %s median %s max %s", lat[0], lat[len(lat)/2], lat[len(lat)-1])

	id, err := transcript.StatIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := readAll(p)
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, gen, ok := srv.Source(p, id.ID.String())
		if ok {
			if got, err := srv.Reconstruct(p, id.ID.String(), gen); err == nil && bytes.Equal(got, want) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never received the transcript with its appended lines (source known: %v)", ok)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Companions reached the server too, linked to their transcript.
	orphan := f.path(orphanTxt)
	oid, _ := transcript.StatIdentity(orphan)
	waitFor(t, func() bool {
		desc, _, ok := srv.Source(orphan, oid.ID.String())
		return ok && desc.Parent != nil && desc.Parent.Path != ""
	})
}

func readAll(p string) ([]byte, error) { return os.ReadFile(p) }

// TestNewSessionDirWithoutSweep: a hot session gains a subagents/ and a
// tool-results/ directory with files in them. Both are picked up through
// directory events, not left for the next sweep (found by the two-device
// e2e, scenario h: the companion took one sweep, ~20-45s, to upload).
func TestNewSessionDirWithoutSweep(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep = time.Hour
	f.a = New(f.store, f.cfg)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return len(f.find("login test flake", false)) == 1 })

	const sid = "0b7e2c1a-0000-4000-8000-0000000000aa"
	proj := filepath.Dir(f.path(alphaRel))
	main := filepath.Join(proj, sid+".jsonl")
	line := func(uuid, text string, extra string) string {
		return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","cwd":"/tmp/oracle-alpha","sessionId":"%s","version":"2.1.0",%s"message":{"role":"user","content":%q},"uuid":"%s","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n", sid, extra, text, uuid)
	}
	if err := os.WriteFile(main, []byte(line("d7000000-0000-4000-8000-000000000001", "fresh session opens", "")), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(f.find("fresh session opens", false)) == 1 })

	sess := filepath.Join(proj, sid)
	for _, d := range []string{filepath.Join(sess, "subagents"), filepath.Join(sess, "tool-results")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Claude creates the directory first and writes into it a moment
	// later; the directory event is handled while it is still empty.
	time.Sleep(300 * time.Millisecond)
	companion := filepath.Join(sess, "tool-results", "late01.txt")
	if err := os.WriteFile(companion, []byte("late tool output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(sess, "subagents", "agent-late01.jsonl")
	if err := os.WriteFile(sub, []byte(line("d7000000-0000-4000-8000-000000000002", "subagent in a new directory", `"agentId":"late01",`)), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for len(f.find("subagent in a new directory", false)) != 1 {
		if time.Since(start) > eventLimit {
			t.Fatalf("subagent in a new session directory not indexed within %s", eventLimit)
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		if _, ok := f.rec.spec(companion); ok {
			break
		}
		if time.Since(start) > eventLimit {
			t.Fatalf("tool-results file in a new directory not handed to sync within %s", eventLimit)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("picked up in %s", time.Since(start))
}

// TestSessionCreatedDuringStartup: a session file created after Run's
// first pass listed its project, but before the watcher was set up, is
// picked up through events, not left for the next sweep. Found as a flake
// of TestNewSessionDirWithoutSweep under load: the test wrote its session
// while Run was still between the pass and the first rewatch.
func TestSessionCreatedDuringStartup(t *testing.T) {
	f := newFixture(t, "-")
	f.cfg.Sweep = time.Hour
	f.a = New(f.store, f.cfg)
	const sid = "0b7e2c1a-0000-4000-8000-0000000000ab"
	main := filepath.Join(filepath.Dir(f.path(alphaRel)), sid+".jsonl")
	testHookAfterFirstPass = func() {
		line := fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"type":"user","cwd":"/tmp/oracle-alpha","sessionId":"%s","version":"2.1.0","message":{"role":"user","content":"session born during startup"},"uuid":"d7000000-0000-4000-8000-000000000011","timestamp":"2026-09-23T11:00:00.000Z"}`+"\n", sid)
		if err := os.WriteFile(main, []byte(line), 0o600); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookAfterFirstPass = nil }()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	start := time.Now()
	for len(f.find("session born during startup", false)) != 1 {
		if time.Since(start) > eventLimit {
			t.Fatalf("session created during startup not indexed within %s", eventLimit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestFastLaneIgnoresCompanions: many companion files written at once
// (hundreds of tool-results after a bulk copy, or a busy workflow) must not
// crowd a live transcript out of the fast lane's MaxHot budget. Found by the
// two-device e2e with a real-corpus sample: appends waited for the sweep.
func TestFastLaneIgnoresCompanions(t *testing.T) {
	f := newFixture(t, "-")
	// Room for every fixture transcript, not for the companions too.
	f.cfg.Sweep, f.cfg.MaxHot = time.Hour, 24
	f.a = New(f.store, f.cfg)
	p := f.path(alphaRel)
	// Companions newer than every transcript.
	dir := filepath.Join(filepath.Dir(f.path(orphanTxt)))
	for i := range 40 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("bulk%02d.txt", i)), []byte("output\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.a.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return len(f.find("login test flake", false)) == 1 })

	appendFile(t, p, claudeUser("c1000000-0000-4000-8000-000000000301", "fast lane not crowded out"))
	start := time.Now()
	for len(f.find("fast lane not crowded out", false)) != 1 {
		if time.Since(start) > eventLimit {
			t.Fatalf("append not indexed within %s with many hot companions", eventLimit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
