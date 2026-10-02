package codex

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/transcript"
)

// Home returns the Codex data directory: $CODEX_HOME, else ~/.codex.
func Home() string {
	if h := strings.TrimSpace(os.Getenv("CODEX_HOME")); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

// Rollout directories under Home. Archiving moves a rollout from sessions/
// to archived_sessions/ with the same file name; both are read.
const (
	SessionsDir = "sessions"
	ArchivedDir = "archived_sessions"
)

// Discover lists every rollout under home: sessions/YYYY/MM/DD/rollout-*.jsonl
// and archived_sessions/ (flat or nested). Sources are sorted by path.
//
// SessionKey is the session id from the file name, so a rollout that moved
// from sessions/ to archived_sessions/ keeps its key: the indexer treats
// the same key at a new path as the same conversation, and the parser reads
// the same session_meta id from its content. FileID is the file's
// dev+inode, which a rename within one volume also keeps.
//
// Legacy single-document rollouts (rollout-*.json) are not listed; none
// exist on the reference machine.
func Discover(home string) ([]transcript.Source, error) {
	var out []transcript.Source
	for _, dir := range []string{SessionsDir, ArchivedDir} {
		root := filepath.Join(home, dir)
		err := fsprobe.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				return err
			}
			if d.IsDir() || !IsRollout(d.Name()) {
				return nil
			}
			src := transcript.Source{
				Agent:       transcript.AgentCodex,
				Path:        path,
				SessionKey:  SessionIDFromPath(path),
				StorageKind: transcript.StorageJSONLAppend,
				Parser:      Name,
			}
			if id, err := transcript.StatIdentity(path); err == nil {
				src.FileID = id.ID
			}
			out = append(out, src)
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return out, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// IsRollout reports whether a file name is a JSONL rollout.
func IsRollout(name string) bool {
	return strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
}

var uuidTail = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.jsonl$`)

// SessionIDFromPath returns the session id a rollout file name ends with
// (rollout-<timestamp>-<uuid>.jsonl), or the file name without extension.
func SessionIDFromPath(path string) string {
	if path == "" {
		return ""
	}
	base := filepath.Base(path)
	if m := uuidTail.FindStringSubmatch(base); m != nil {
		return strings.ToLower(m[1])
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}
