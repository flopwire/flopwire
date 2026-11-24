package agent

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/antigravity"
)

var errAntigravityCatalogPending = errors.New("agent: Antigravity workspace catalog reconciliation pending")

func antigravityPath(root, id string) string {
	return filepath.Join(root, id, ".system_generated", "logs", "transcript.jsonl")
}

func antigravitySource(root, id string) (transcript.Source, bool) {
	dir := filepath.Join(root, id)
	path := antigravityPath(root, id)
	if antigravity.SessionID(path) != id {
		return transcript.Source{}, false
	}
	// The CLI creates ordinary nested directories. A linked artifact or
	// session directory can lead outside the transcript store.
	for _, d := range []string{dir, filepath.Join(dir, ".system_generated"), filepath.Dir(path)} {
		if fi, err := fsprobe.Lstat(d); err != nil || !fi.IsDir() {
			return transcript.Source{}, false
		}
	}
	if fi, err := fsprobe.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return transcript.Source{}, false
	}
	return transcript.Source{
		Agent: transcript.AgentAntigravity, Path: path, SessionKey: id,
		StorageKind: transcript.StorageJSONLAppend, Parser: antigravity.ParserName,
	}, true
}

func (a *Agent) discoverAntigravity() ([]transcript.Source, error) {
	root := a.cfg.AntigravityRoot
	if root == "" {
		return nil, nil
	}
	entries, err := fsprobe.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sources []transcript.Source
	for _, entry := range entries {
		if entry.IsDir() {
			if src, ok := antigravitySource(root, entry.Name()); ok {
				sources = append(sources, src)
			}
		}
	}
	return sources, nil
}

// watchAntigravity watches each level of recently changed conversation
// directories so a new transcript can arrive after its parent is discovered.
func (a *Agent) watchAntigravity(now time.Time, add func(string)) {
	root := a.cfg.AntigravityRoot
	if root == "" {
		return
	}
	entries, _ := fsprobe.ReadDir(root)
	type conversationDir struct {
		path string
		mod  time.Time
	}
	var recent []conversationDir
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		fsprobe.Note(fsprobe.OpStat, dir)
		if fi, err := entry.Info(); err == nil && now.Sub(fi.ModTime()) < recentProject {
			recent = append(recent, conversationDir{dir, fi.ModTime()})
		}
	}
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].mod.After(recent[j].mod) })
	for _, entry := range recent {
		generated := filepath.Join(entry.path, ".system_generated")
		for _, dir := range []string{entry.path, generated, filepath.Join(generated, "logs")} {
			if fi, err := fsprobe.Lstat(dir); err != nil || !fi.IsDir() {
				break
			}
			add(dir)
		}
	}
}

type antigravityCatalog struct {
	stamp [3]transcript.Identity
	ready bool
	rows  map[string]antigravity.Metadata
}

func (a *Agent) antigravityCatalogPath() string {
	return filepath.Join(filepath.Dir(a.cfg.AntigravityRoot), "conversation_summaries.db")
}

func (a *Agent) antigravityCatalogStamp() ([3]transcript.Identity, error) {
	catalog := a.antigravityCatalogPath()
	var stamp [3]transcript.Identity
	for i, path := range []string{catalog, catalog + "-wal", filepath.Join(filepath.Dir(catalog), "cache", "conversation_metadata.json")} {
		id, err := transcript.StatIdentity(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return stamp, err
		}
		stamp[i] = id
	}
	return stamp, nil
}

func (a *Agent) antigravityCatalogCurrent() bool {
	if a.cfg.AntigravityRoot == "" {
		return false
	}
	stamp, err := a.antigravityCatalogStamp()
	a.mu.Lock()
	defer a.mu.Unlock()
	return err == nil && a.agyCatalog.ready && stamp == a.agyCatalog.stamp
}

func (a *Agent) antigravityMetadata(_ context.Context, id string) (antigravity.Metadata, error) {
	a.mu.Lock()
	catalog := a.agyCatalog
	a.mu.Unlock()
	return catalog.rows[id], nil
}

func (a *Agent) antigravityPlacementReady(id string) bool {
	a.mu.Lock()
	catalog := a.agyCatalog
	a.mu.Unlock()
	_, ready := catalog.workspaces(id)
	if _, listed := catalog.rows[id]; ready && !listed {
		// A workstation with no vendor metadata uses the ordinary
		// unplaceable policy. A previously catalogued session needs
		// fresh metadata to keep authorizing its old placement.
		p, _ := a.storedPlace(placeKey{transcript.AgentAntigravity, id})
		return p.pl.Cwd == ""
	}
	return ready
}

// workspaces attributes both the current conversation's workspace and
// the workspaces of every parent. A root without any workspace uses the
// ordinary unplaceable policy.
func (c antigravityCatalog) workspaces(id string) ([]string, bool) {
	if !c.ready {
		return nil, false
	}
	row := c.rows[id]
	if row.Invalid || row.ParentSessionID == "" && row.Depth > 0 {
		return nil, false
	}
	if row.ParentSessionID == "" {
		if row.Cwd == "" {
			return nil, true
		}
		return append([]string{row.Cwd}, row.OtherCwds...), true
	}

	var dirs []string
	seen := make(map[string]bool)
	for id != "" {
		if seen[id] {
			return nil, false
		}
		row, ok := c.rows[id]
		if row.Invalid || (!ok && len(seen) > 0) || (row.ParentSessionID == "" && row.Depth > 0) {
			return nil, false
		}
		if row.Cwd != "" {
			dirs = append(dirs, row.Cwd)
			dirs = append(dirs, row.OtherCwds...)
		}
		seen[id] = true
		id = row.ParentSessionID
	}
	return dirs, len(dirs) > 0
}

// refreshAntigravityMetadata reconciles catalog edits independently of
// transcript watermarks, before the ordinary gates can index or offer a file.
func (a *Agent) refreshAntigravityMetadata(ctx context.Context, f *found) {
	if a.cfg.AntigravityRoot == "" {
		return
	}
	stamp, err := a.antigravityCatalogStamp()
	a.captureScopeMu.Lock()
	defer a.captureScopeMu.Unlock()
	a.mu.Lock()
	old := a.agyCatalog
	a.mu.Unlock()
	current := old
	if err != nil || !old.ready || stamp != old.stamp {
		current = antigravityCatalog{stamp: stamp}
		if err == nil {
			current.rows, err = antigravity.ReadAllMetadata(ctx, a.antigravityCatalogPath())
		}
		if err == nil {
			if after, statErr := a.antigravityCatalogStamp(); statErr == nil && stamp == after {
				current.ready = true
			}
		} else {
			a.log.Warn("agent: reading Antigravity workspace catalog", "err", err)
		}
	}
	changed := old.ready != current.ready || old.stamp != current.stamp
	a.mu.Lock()
	pending := make(map[string]*target)
	if changed {
		for path, t := range a.targets {
			if t.src.Agent == transcript.AgentAntigravity {
				pending[path] = t
			}
		}
	}
	for _, t := range f.targets {
		if t.src.Agent == transcript.AgentAntigravity && a.targets[t.path] == nil {
			pending[t.path] = t
		}
	}
	a.mu.Unlock()
	for path, t := range pending {
		dirs, ready := current.workspaces(t.src.SessionKey)
		previous, wasReady := old.workspaces(t.src.SessionKey)
		a.mu.Lock()
		isNew := a.targets[path] == nil
		a.mu.Unlock()
		if !isNew && ready == wasReady && slices.Equal(dirs, previous) {
			continue
		}
		if ready {
			tt, err := a.recordCwds(ctx, t, dirs)
			if err == nil {
				_, err = a.tighten(ctx, t, t.sourceID, tt)
			}
			if err != nil {
				current.ready = false
				a.log.Warn("agent: reconciling Antigravity workspace policy", "err", err)
			}
		}
		a.mu.Lock()
		delete(a.notified, path)
		t.modeGen = 0
		if ready && !wasReady && t.indexedWith == "" {
			t.seen = transcript.Identity{} // recheck a transcript previously held
		}
		a.mu.Unlock()
	}
	a.mu.Lock()
	a.agyCatalog = current
	a.mu.Unlock()
}

// authorizeAntigravityCapture keeps the reconciled policy in force across
// rooted capture and upload. The scheduler rechecks vendor changes before
// a filesystem or network handover.
func (a *Agent) authorizeAntigravityCapture(ctx context.Context, spec devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error) {
	a.captureScopeMu.RLock()
	check := func(context.Context) error {
		if !a.antigravityCatalogCurrent() || !a.allowUpload(spec) {
			return errAntigravityCatalogPending
		}
		return nil
	}
	if err := check(ctx); err != nil {
		a.captureScopeMu.RUnlock()
		return nil, err
	}
	return &devicesync.CaptureAuthorization{LeaseOnly: true, Check: check, Release: a.captureScopeMu.RUnlock}, nil
}
