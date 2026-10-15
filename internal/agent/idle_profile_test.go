package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/perfguard"
)

// BenchmarkFastLaneUnchanged profiles one default fast-lane tick over 1024
// recently created, actually indexed Claude files. One operation includes the
// full candidate selection/sort and the default 512 unchanged-file gate checks.
// Discovery, parsing, database setup and the racy-window wait precede the timer.
// It deliberately excludes sweeps, watchers, sync, recovery and idle trimming;
// use a bounded benchtime well below the default ten-minute hot window.
func BenchmarkFastLaneUnchanged(b *testing.B) {
	const files = 1024
	ctx := context.Background()
	root := b.TempDir()
	cwd := filepath.Join(root, "work")
	projects := filepath.Join(root, "claude", "projects")
	project := filepath.Join(projects, pathpolicy.ClaudeFolderName(cwd))
	for _, dir := range []string{cwd, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
	}
	for i := range files {
		sid := fmt.Sprintf("5e550000-0000-4000-8000-%012d", i)
		s := perfguard.ClaudeSession{SessionID: sid, Cwd: cwd}
		if err := os.WriteFile(filepath.Join(project, sid+".jsonl"), s.Lines(0, 4), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	store, err := localindex.Open(filepath.Join(root, "index.db"), localindex.Options{DeferCommit: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	a := New(store, Config{Home: root, ClaudeProjects: projects,
		CodexHome: filepath.Join(root, "codex"), DesktopCodeRoot: "-", CoworkRoot: "-",
		DevinDB: "-", OpencodeDB: "-", Workers: 3,
		UserRuleList: []string{"local " + cwd}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	// Let the actual inode ctimes leave the racy window before indexing. Do not
	// replace saved identities, parser versions or effective extraction contracts.
	waitRacy()
	if err := a.Once(ctx); err != nil {
		b.Fatal(err)
	}
	if len(a.targets) != files || a.stats.Indexed.Load() != files {
		b.Fatalf("fixture: targets=%d indexed=%d, want %d", len(a.targets), a.stats.Indexed.Load(), files)
	}
	checkStable := func() {
		b.Helper()
		if a.urgent.len()+a.normal.len()+a.background.len() != 0 || a.busy != 0 {
			b.Fatal("unchanged fast lane queued work")
		}
		for _, target := range a.targets {
			if target.racy() || target.stale() || target.seen.CTime <= time.Now().Add(-a.cfg.HotWindow).UnixNano() {
				b.Fatal("fixture left the settled hot-file state")
			}
		}
	}
	a.fastLane(ctx)
	checkStable()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.fastLane(ctx)
	}
	b.StopTimer()
	checkStable()
}
