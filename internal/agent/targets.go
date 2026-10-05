package agent

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/fsprobe"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/claude"
	"github.com/flopwire/flopwire/internal/transcript/codex"
)

type kind uint8

const (
	kindTranscript kind = iota // a JSONL transcript, indexed
	kindCompanion              // a file archived beside a Claude session
)

// target is one tracked file. Fields below mu are the gate state, guarded
// by Agent.mu; mu serializes indexing of the target.
type target struct {
	path   string
	kind   kind
	src    transcript.Source // transcripts: agent, parser, session key, storage kind
	parser transcript.Parser
	parent string // companion: the transcript it belongs to (sync Parent)
	owner  string // companion: the conversation it belongs to
	role   claude.CompanionRole
	// root is the session a Claude subagent transcript belongs to: its
	// verdict is at least that session's (a subagent goes with it).
	root string

	mu sync.Mutex // held while indexing

	seen     transcript.Identity // identity at the last index or verified check
	seenAt   int64               // unix ns when seen was sampled
	sourceID int64
	// scanned is how far the transcript was parsed, so how far the
	// directories it names are known: sync reads no further while path
	// rules are in force (uploadBound).
	scanned  int64
	queued   bool
	hotUntil time.Time // re-stat on the fast lane until then
	listed   uint64    // last discovery pass that listed it

	mode    pathpolicy.Mode // path rules' verdict while modeGen is the rules' generation
	modeGen uint64

	inBackground bool // queued in Agent.background
	// The queue t is in and its neighbours there (queue), guarded by
	// Agent.mu.
	in            *queue
	qprev, qnext  *target
	indexedWith   string // effective extraction contract (or legacy parser version) last indexed
	reparseFailed string // parser version whose full re-parse failed; not retried until restart
}

// stale reports whether the source was indexed by another parser version
// and should be re-parsed from its bytes (D16). Called with Agent.mu held.
func (t *target) stale() bool {
	if t.kind != kindTranscript || t.parser == nil || t.indexedWith == "" {
		return false
	}
	name := indexingVersion(t.parser)
	return transcript.ReparseKey(t.indexedWith) != name && t.reparseFailed != name
}

// racy reports whether an unchanged tuple cannot be trusted yet (git's racy
// rule, transcript.Watermark.Racy): the change time is too close to the
// moment it was sampled.
func (t *target) racy() bool {
	return t.seen.CTime != 0 && t.seenAt-t.seen.CTime < int64(transcript.RacyWindow)
}

// specOf is t.spec with Agent.mu held: a discovery pass may update a
// companion's owner and parent. A transcript's spec carries its
// session's repository as placed (issue #102): its own placement, else
// its root session's (a subagent's).
func (a *Agent) specOf(t *target) devicesync.SourceSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	sp := t.spec()
	if t.kind == kindTranscript {
		key, _ := t.placeKeyOf()
		p, ok := a.places[key]
		if !ok && t.root != "" {
			p, ok = a.places[placeKey{t.src.Agent, t.root}]
		}
		if ok {
			sp.Checkout, sp.Remote = p.pl.Main, p.pl.Remote
		}
	}
	return sp
}

// spec describes t to sync. Called with Agent.mu held.
func (t *target) spec() devicesync.SourceSpec {
	if t.kind == kindCompanion {
		return devicesync.SourceSpec{Path: t.path, Agent: transcript.AgentClaude, StorageKind: transcript.StorageCompanion,
			SessionKey: t.owner, Parser: claude.ParserName, Parent: t.parent}
	}
	return devicesync.SourceSpec{Path: t.path, Agent: t.src.Agent, StorageKind: t.src.StorageKind,
		SessionKey: t.src.SessionKey, Parser: t.src.Parser}
}

// found is one discovery pass's output.
type found struct {
	targets []*target
	stubs   []*transcript.Conversation // orphaned Claude sessions (companions, no transcript)
	stubAt  map[string]string          // orphaned session -> the path its transcript had
}

func (f *found) claudeSessions(sessions []*claude.Session, p transcript.Parser) {
	for _, s := range sessions {
		if c := s.Stub(); c != nil {
			f.stubs = append(f.stubs, c)
			if f.stubAt == nil {
				f.stubAt = map[string]string{}
			}
			f.stubAt[s.SessionID] = filepath.Join(s.ProjectDir, s.SessionID+".jsonl")
		}
		main := s.Transcript
		if main == "" {
			// Orphaned: the companions still name the transcript they came from.
			main = filepath.Join(s.ProjectDir, s.SessionID+".jsonl")
		}
		for _, src := range s.Sources() {
			t := &target{path: src.Path, kind: kindTranscript, src: src, parser: p}
			if src.SessionKey != s.SessionID {
				t.root = s.SessionID
			}
			f.targets = append(f.targets, t)
		}
		metaOwner := map[string]*claude.SubagentFile{}
		for i := range s.Subagents {
			if sa := &s.Subagents[i]; sa.MetaPath != "" {
				metaOwner[sa.MetaPath] = sa
			}
		}
		for _, c := range s.Companions {
			t := &target{path: c.Path, kind: kindCompanion, parent: main, owner: s.SessionID, role: c.Role}
			if sa := metaOwner[c.Path]; sa != nil {
				t.parent, t.owner = sa.Path, claude.SubagentSessionID(sa.AgentID)
			}
			f.targets = append(f.targets, t)
		}
	}
}

func (f *found) codexSources(srcs []transcript.Source, p transcript.Parser) {
	for _, src := range srcs {
		f.targets = append(f.targets, &target{path: src.Path, kind: kindTranscript, src: src, parser: p})
	}
}

// discoverAll lists every Claude and Codex file.
func (a *Agent) discoverAll() (*found, error) {
	f := &found{}
	sessions, err := claude.Discover(a.cfg.ClaudeProjects)
	if err != nil {
		return nil, err
	}
	f.claudeSessions(sessions, a.claude)
	srcs, err := codex.Discover(a.cfg.CodexHome)
	if err != nil {
		return nil, err
	}
	f.codexSources(srcs, a.codex)
	return f, nil
}

// discoverDir lists the files a directory event may have added: the
// Claude project the directory belongs to, or the Codex rollouts directly
// in it. It reports whether dir is under a known root.
func (a *Agent) discoverDir(dir string) (*found, bool) {
	f := &found{}
	if rel, ok := under(a.cfg.ClaudeProjects, dir); ok {
		if rel == "." {
			return nil, true // the project list changed: the caller runs a full pass
		}
		sessions, err := claude.DiscoverProject(filepath.Join(a.cfg.ClaudeProjects, firstElem(rel)))
		if err != nil {
			return f, true
		}
		f.claudeSessions(sessions, a.claude)
		return f, true
	}
	if _, ok := under(a.cfg.CodexHome, dir); ok {
		entries, err := fsprobe.ReadDir(dir)
		if err != nil {
			return f, true
		}
		var srcs []transcript.Source
		for _, e := range entries {
			if e.Type().IsRegular() && codex.IsRollout(e.Name()) {
				// Discover of the file's own directory fills the source the
				// same way a full pass does.
				srcs = append(srcs, codexSource(filepath.Join(dir, e.Name())))
			}
		}
		f.codexSources(srcs, a.codex)
		return f, true
	}
	return f, false
}

// codexSource builds a rollout source as codex.Discover does.
func codexSource(path string) transcript.Source {
	return transcript.Source{Agent: transcript.AgentCodex, Path: path, SessionKey: codex.SessionIDFromPath(path),
		StorageKind: transcript.StorageJSONLAppend, Parser: codex.Name}
}

// under reports whether path is root or below it, and the relative path.
func under(root, path string) (string, bool) {
	if root == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", false
	}
	return rel, true
}

func firstElem(rel string) string {
	for i := 0; i < len(rel); i++ {
		if rel[i] == filepath.Separator {
			return rel[:i]
		}
	}
	return rel
}

func indexingVersion(p transcript.Parser) string {
	if contract := transcript.ParserContract(p); contract != "" {
		return contract
	}
	return transcript.ReparseKey(p.Name())
}
