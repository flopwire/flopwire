package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

// TestCorpusAppendLatency copies the largest recent real Claude session
// (read-only) into a scratch home, runs the agent on it, appends 20 lines
// the way a live session does, and reports time-to-findable in the local
// index. The real harness directories are never written.
//
//	FLOPWIRE_CORPUS=1 go test -run TestCorpusAppendLatency -v ./internal/agent/
func TestCorpusAppendLatency(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	home, _ := os.UserHomeDir()
	sessions, err := claude.Discover(claude.ProjectsRoot(os.Getenv, home))
	if err != nil {
		t.Fatal(err)
	}
	// A recent session of moderate size: the live case.
	var pick string
	var best time.Time
	for _, s := range sessions {
		if s.Transcript == "" {
			continue
		}
		fi, err := os.Stat(s.Transcript)
		if err != nil || fi.Size() < 1<<20 || fi.Size() > 64<<20 {
			continue
		}
		if fi.ModTime().After(best) {
			pick, best = s.Transcript, fi.ModTime()
		}
	}
	if pick == "" {
		t.Skip("no Claude session between 1MB and 64MB")
	}
	scratch := t.TempDir()
	proj := filepath.Join(scratch, "projects", filepath.Base(filepath.Dir(pick)))
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(proj, filepath.Base(pick))
	sessionID, cwd := copyFile(t, pick, dst)
	fi, _ := os.Stat(dst)
	t.Logf("session %s (%.1fMB)", filepath.Base(pick), float64(fi.Size())/1e6)

	store, err := localindex.Open(filepath.Join(scratch, "index.db"), localindex.Options{DeferCommit: true, ReadConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := New(store, Config{ClaudeProjects: filepath.Join(scratch, "projects"), CodexHome: filepath.Join(scratch, "nocodex"), DevinDB: "-",
		Sweep: time.Hour, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	start := time.Now()
	waitIndexed(t, store, sessionID)
	t.Logf("initial index of the copy: %s", time.Since(start).Round(time.Millisecond))

	var lat []time.Duration
	for i := range 20 {
		needle := fmt.Sprintf("flopwire latency probe %d %d", time.Now().UnixNano(), i)
		line := fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":%q,"version":"2.1.0","type":"user","message":{"role":"user","content":%q},"uuid":"f0000000-0000-4000-8000-%012d","timestamp":%q}`+"\n",
			cwd, sessionID, needle, i, time.Now().UTC().Format(time.RFC3339Nano))
		appendFile(t, dst, line)
		t0 := time.Now()
		for {
			hits, err := store.Find(ctx, needle, localindex.FindOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) == 1 {
				break
			}
			if time.Since(t0) > 10*time.Second {
				t.Fatalf("append %d not findable after 10s", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		lat = append(lat, time.Since(t0))
		time.Sleep(time.Duration(100+37*i%400) * time.Millisecond) // appends at varied phases of the fast lane
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration { return lat[int(q*float64(len(lat)-1)+0.5)].Round(time.Millisecond) }
	t.Logf("time to findable over %d appends: p50 %s p95 %s max %s", len(lat), p(0.5), p(0.95), lat[len(lat)-1].Round(time.Millisecond))
	if p(0.95) > 2*time.Second {
		t.Errorf("p95 %s over 2s", p(0.95))
	}
}

// copyFile copies src to dst and returns the session id and cwd of its
// first line that has them.
func copyFile(t *testing.T, src, dst string) (session, cwd string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			out.Write(line)
			if session == "" {
				var rec struct {
					SessionID string `json:"sessionId"`
					Cwd       string `json:"cwd"`
				}
				if json.Unmarshal(line, &rec) == nil && rec.SessionID != "" {
					session, cwd = rec.SessionID, rec.Cwd
				}
			}
		}
		if err != nil {
			break // a trailing partial line is left out
		}
	}
	return session, cwd
}

func waitIndexed(t *testing.T, s *localindex.Store, session string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		var n int
		s.DB().QueryRow(`SELECT count(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.session_id = ?`, session).Scan(&n)
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("copied session never indexed")
}

// TestCorpusPlacement places every real Claude and Codex transcript
// (read-only) the way the agent does and reports how each was placed and
// what it cost. It writes only a scratch index.
//
//	FLOPWIRE_CORPUS=1 go test -run TestCorpusPlacement -v ./internal/agent/
func TestCorpusPlacement(t *testing.T) {
	if os.Getenv("FLOPWIRE_CORPUS") == "" {
		t.Skip("set FLOPWIRE_CORPUS=1")
	}
	home, _ := os.UserHomeDir()
	store, err := localindex.Open(filepath.Join(t.TempDir(), "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := New(store, Config{ClaudeProjects: claude.ProjectsRoot(os.Getenv, home), CodexHome: codex.Home(), DevinDB: "-",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	f, err := a.discoverAll()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	byAgent := map[string]int{}
	var gone, goneMain, withMain, withRemote, worktrees, folderExists int
	var scan, resolve time.Duration
	cwds := map[string]bool{}
	t0 := time.Now()
	for _, tg := range f.targets {
		if tg.kind != kindTranscript {
			continue
		}
		key, path := tg.placeKeyOf()
		s0 := time.Now()
		h := scanHints(path)
		scan += time.Since(s0)
		r0 := time.Now()
		p, _ := a.placeOf(key, path)
		pl := p.pl
		resolve += time.Since(r0)
		a.mu.Lock()
		how := a.places[key].how
		a.mu.Unlock()
		counts[how]++
		byAgent[string(key.agent)+" "+how]++
		if pl.Cwd != "" {
			cwds[pl.Cwd] = true
			if _, err := os.Stat(pl.Cwd); err == nil && how == localindex.PlacedByFolder {
				folderExists++
			}
			if _, err := os.Stat(pl.Cwd); err != nil {
				gone++
				if pl.Main != "" || pl.Remote != "" {
					goneMain++
				}
			}
		}
		if pl.Main != "" {
			withMain++
			if pl.Main != pl.Worktree {
				worktrees++
			}
		}
		if pl.Remote != "" {
			withRemote++
		}
		_ = h
	}
	total := time.Since(t0)
	n := 0
	for _, c := range counts {
		n += c
	}
	t.Logf("transcripts %d, placed by: %v", n, counts)
	t.Logf("by agent: %v; folder decoded to an existing directory %d", byAgent, folderExists)
	t.Logf("with a main checkout %d (in a linked worktree %d), with a remote %d; cwd gone %d (still placed by repo or remote %d)",
		withMain, worktrees, withRemote, gone, goneMain)
	t.Logf("time: total %s (%.0fus/transcript); scanning transcripts %s (counted twice: once alone, once in placeOf); distinct cwds %d",
		total.Round(time.Millisecond), float64(total.Microseconds())/float64(max(n, 1)), scan.Round(time.Millisecond), len(cwds))

	// The recovery pass over deleted directories (as Run does it, in the
	// background). Sessions in temporary directories are counted apart.
	isTemp := func(c string) bool {
		for _, p := range []string{"/private/", "/tmp/", "/var/"} {
			if strings.HasPrefix(c, p) {
				return true
			}
		}
		return false
	}
	type goneKey struct{ agent, kind string }
	goneBefore := map[placeKey]goneKey{}
	a.mu.Lock()
	for _, tg := range f.targets {
		if tg.kind == kindTranscript {
			a.targets[tg.path] = tg
		}
	}
	for k, p := range a.places {
		if needsRecovery(p) {
			kind := "no-remote"
			if p.pl.Remote != "" {
				kind = "remote"
			}
			if p.pl.Cwd != "" && isTemp(p.pl.Cwd) {
				kind = "temp-" + kind
			} else if p.pl.Cwd == "" {
				kind = "no-cwd-" + kind
			}
			goneBefore[k] = goneKey{string(k.agent), kind}
		}
	}
	a.mu.Unlock()
	pass := a.recoverPass(context.Background())
	after := map[string]int{}
	byHow := map[string]int{}
	for k, g := range goneBefore {
		p, _ := a.storedPlace(k)
		after[g.agent+" "+g.kind]++
		if p.pl.Main != "" {
			after[g.agent+" "+g.kind+" found"]++
			byHow[g.agent+" "+g.kind+" "+p.how]++
			if p.how != localindex.PlacedByRemote && p.how != localindex.PlacedByCwd && os.Getenv("FLOPWIRE_CORPUS_VERBOSE") != "" {
				t.Logf("  %s %s -> %s", p.how, p.pl.Cwd, p.pl.Main)
			}
		}
	}
	t.Logf("recovery pass: %s, %d candidate repositories, %d sessions looked at, found %v, ambiguous %d",
		pass.Took.Round(time.Millisecond), pass.Repos, pass.Checked, pass.Found, pass.Ambiguous)
	t.Logf("sessions needing recovery by agent and kind (and found): %v", after)
	t.Logf("found by method: %v", byHow)
	final := map[string]int{}
	for _, p := range a.places {
		final[p.how]++
	}
	t.Logf("placements by method after the pass: %v", final)
	pass2 := a.recoverPass(context.Background())
	t.Logf("second pass (nothing due): %s, %d looked at", pass2.Took.Round(time.Millisecond), pass2.Checked)

	// Reading git's files against running git, over the distinct cwds.
	var list []string
	for c := range cwds {
		if _, err := os.Stat(c); err == nil {
			list = append(list, c)
		}
	}
	sort.Strings(list)
	g0 := time.Now()
	for _, c := range list {
		localindex.ResolveRepo(c)
	}
	files := time.Since(g0)
	sample := list[:min(len(list), 100)]
	g1 := time.Now()
	mismatch := 0
	for _, c := range sample {
		out, err := exec.Command("git", "-C", c, "rev-parse", "--show-toplevel").Output()
		top := strings.TrimSpace(string(out))
		if got := localindex.ResolveRepo(c).Worktree; err == nil && got != top {
			if r, e := filepath.EvalSymlinks(got); e != nil || r != top {
				mismatch++
				t.Logf("worktree mismatch %s: files %s, git %s", c, got, top)
			}
		}
	}
	perGit := time.Since(g1) / time.Duration(max(len(sample), 1))
	t.Logf("repo resolution over %d existing cwds: git files %s (%.0fus each); git rev-parse %s each (sample %d, %d mismatches)",
		len(list), files.Round(time.Millisecond), float64(files.Microseconds())/float64(max(len(list), 1)), perGit.Round(time.Microsecond), len(sample), mismatch)
}
