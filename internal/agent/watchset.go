package agent

import (
	"path/filepath"
	"sort"
	"time"

	"github.com/flopwire/flopwire/internal/fsprobe"
)

// watchEvent is a directory event: a directory whose entries changed, or
// (inotify) a file created or, with modify, written in a watched directory.
type watchEvent struct {
	path   string
	modify bool
}

// recentProject is how recently a Claude project directory must have
// changed to be watched.
const recentProject = 7 * 24 * time.Hour

// rewatch points the watcher at the directories where new files are
// likely: the harness roots, today's and yesterday's Codex day
// directories, recently active Claude projects, and the session
// directories of hot transcripts. It returns directories newly watched.
func (a *Agent) rewatch(w *watcher) []string {
	return w.set(a.watchDirs(time.Now()))
}

func (a *Agent) watchDirs(now time.Time) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(d string) {
		if d == "" || seen[d] || len(dirs) >= a.cfg.MaxWatch {
			return
		}
		if fi, err := fsprobe.Stat(d); err != nil || !fi.IsDir() {
			return
		}
		seen[d] = true
		dirs = append(dirs, d)
	}
	a.mu.Lock()
	cw := a.coworkResult
	dc := a.desktopCodeResult
	a.mu.Unlock()
	add(a.cfg.ClaudeProjects)
	sessions := filepath.Join(a.cfg.CodexHome, "sessions")
	add(sessions)
	for _, day := range []time.Time{now, now.Add(-24 * time.Hour)} {
		y, m, d := day.Format("2006"), day.Format("01"), day.Format("02")
		add(filepath.Join(sessions, y))
		add(filepath.Join(sessions, y, m))
		add(filepath.Join(sessions, y, m, d))
	}
	add(filepath.Join(a.cfg.CodexHome, "archived_sessions"))
	for _, d := range a.stores() {
		add(filepath.Dir(d.path))
	}

	for _, d := range cw.WatchDirs {
		add(d)
	}
	for _, d := range dc.WatchDirs {
		add(d)
	}
	// Hot transcripts: their directory, and the session directory beside a
	// main transcript where subagents and tool-results appear.
	a.mu.Lock()
	var hot []string
	hotSince := now.Add(-a.cfg.HotWindow).UnixNano()
	for _, t := range a.targets {
		if t.hotUntil.After(now) || t.seen.CTime > hotSince {
			hot = append(hot, t.path)
		}
	}
	a.mu.Unlock()
	sort.Strings(hot)
	for _, p := range hot {
		dir := filepath.Dir(p)
		add(dir)
		if rel, ok := under(a.cfg.ClaudeProjects, p); ok && filepath.Dir(rel) == firstElem(rel) {
			sess := p[:len(p)-len(filepath.Ext(p))]
			add(sess)
			add(filepath.Join(sess, "subagents"))
			add(filepath.Join(sess, "tool-results"))
		}
	}

	// Recently active Claude projects, newest first.
	entries, _ := fsprobe.ReadDir(a.cfg.ClaudeProjects)
	type proj struct {
		path string
		mod  time.Time
	}
	var projects []proj
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fsprobe.Note(fsprobe.OpStat, filepath.Join(a.cfg.ClaudeProjects, e.Name())) // Info is an lstat
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) < recentProject {
			projects = append(projects, proj{filepath.Join(a.cfg.ClaudeProjects, e.Name()), fi.ModTime()})
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].mod.After(projects[j].mod) })
	for _, p := range projects {
		add(p.path)
	}
	return dirs
}
