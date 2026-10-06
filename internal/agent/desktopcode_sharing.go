package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/flopwire/flopwire/internal/devicesync"
	"github.com/flopwire/flopwire/internal/localindex"
	"github.com/flopwire/flopwire/internal/pathpolicy"
	"github.com/flopwire/flopwire/internal/transcript"
	"github.com/flopwire/flopwire/internal/transcript/cowork"
	"github.com/flopwire/flopwire/internal/transcript/desktopcode"
)

var errDesktopCodeHeld = errors.New("agent: scoped Local Code indexed authorization unavailable")

func (a *Agent) desktopCodeSharingConfigured() bool {
	_, ok := a.cfg.Sync.(interface {
		SetAuthorize(func(context.Context, devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error))
	})
	return ok
}

// The standalone Local Code dispatcher also handles recovered scheduler jobs.
// If a configured container disappears or changes, nil authorization leaves the
// sync store's durable protected-source marker responsible for rejecting them.
// The Cowork integration dispatches to the locked helper after its own refresh.
func (a *Agent) authorizeDesktopCode(ctx context.Context, spec devicesync.SourceSpec) (*devicesync.CaptureAuthorization, error) {
	a.refreshCowork(ctx)
	a.refreshPolicy(ctx, false)
	a.captureScopeMu.Lock()
	release := a.captureScopeMu.Unlock
	if !a.allowUpload(spec) {
		release()
		return nil, errDesktopCodeHeld
	}
	a.mu.Lock()
	t := a.targets[spec.Path]
	a.mu.Unlock()
	if !a.desktopCodeScoped(spec.Path) {
		release()
		return nil, nil
	}
	if t == nil {
		release()
		return nil, errDesktopCodeHeld
	}
	keys, _ := a.coworkCaptureScope(t)
	protectedCowork := a.coworkScopePresent(keys)
	for _, key := range keys {
		if placement, ok := a.storedPlace(key); ok && localindex.IsCoworkPlacement(placement.how) {
			protectedCowork = true
		}
	}
	if protectedCowork {
		release()
		return nil, errDesktopCodeHeld
	}
	return a.desktopCodeAuthorizationLocked(ctx, spec, t, release)
}

// desktopCodeAuthorizationLocked assumes fresh desktop/policy snapshots and an
// exclusive captureScopeMu lease. The caller must exclude Cowork-origin sources.
// No target, placement or database lock remains held during network handovers.
func (a *Agent) desktopCodeAuthorizationLocked(ctx context.Context, spec devicesync.SourceSpec, t *target, release func()) (*devicesync.CaptureAuthorization, error) {
	success := false
	defer func() {
		if !success {
			release()
		}
	}()
	root := a.cfg.DesktopCodeRoot
	if !a.desktopCodeSharingConfigured() || !a.desktopCodeScoped(spec.Path) || spec.Export {
		return nil, errDesktopCodeHeld
	}
	t.mu.Lock()
	a.mu.Lock()
	want, identity, offset := t.spec(), t.seen, t.scanned
	indexedFresh := t.parser != nil && transcript.ReparseKey(t.indexedWith) == indexingVersion(t.parser)
	a.mu.Unlock()
	t.mu.Unlock()
	if spec.Path != want.Path || spec.Agent != want.Agent || spec.SessionKey != want.SessionKey || spec.StorageKind != want.StorageKind || spec.Parent != want.Parent || spec.Parser != want.Parser || spec.Agent != transcript.AgentClaude || identity.ID == (transcript.FileID{}) || identity.CTime == 0 {
		return nil, errDesktopCodeHeld
	}
	if spec.StorageKind == transcript.StorageJSONLAppend && !indexedFresh {
		return nil, errDesktopCodeHeld
	}
	if spec.StorageKind == transcript.StorageCompanion {
		offset = identity.Size
	}
	if offset < 0 || offset > identity.Size {
		return nil, errDesktopCodeHeld
	}
	if mode, known := a.modeOf(t); !known || mode != pathpolicy.Allow || !a.allowUpload(spec) {
		return nil, errDesktopCodeHeld
	}
	signature, err := desktopCodeSourceSignature(root, a.cfg.ClaudeProjects, spec)
	if err != nil {
		return nil, err
	}
	policyKey := a.policy().key
	userStamp, adminStamp := stampOf(a.cfg.UserRules), stampOf(a.cfg.AdminRulesCache)
	digest := sha256.Sum256([]byte("desktop-code\x00" + root + "\x00" + signature + "\x00" + policyKey))
	auth := &devicesync.CaptureAuthorization{
		Origin: "desktop-code", Root: root,
		Proof: devicesync.CaptureProof{Origin: "desktop-code", Root: root, PolicyRequestDigest: fmt.Sprintf("%x", digest), Identity: identity, Offset: offset},
		Open: func(ctx context.Context, sp devicesync.SourceSpec) (*os.File, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if sp.Path != spec.Path {
				return nil, errDesktopCodeHeld
			}
			return cowork.OpenFile(root, sp.Path)
		}, Release: release,
	}
	if spec.StorageKind == transcript.StorageCompanion {
		auth.Proof.ContentSHA, err = a.store.CompanionDigest(ctx, spec.Path)
		if err != nil || len(auth.Proof.ContentSHA) != sha256.Size {
			return nil, errDesktopCodeHeld
		}
	}
	auth.Check = func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if a.cfg.DesktopCodeRoot != root || a.policy().key != policyKey || stampOf(a.cfg.UserRules) != userStamp || stampOf(a.cfg.AdminRulesCache) != adminStamp {
			return errDesktopCodeHeld
		}
		current, err := desktopCodeSourceSignature(root, a.cfg.ClaudeProjects, spec)
		if err != nil || current != signature {
			return errDesktopCodeHeld
		}
		if spec.StorageKind == transcript.StorageJSONLAppend {
			t.mu.Lock()
			a.mu.Lock()
			fresh := t.parser != nil && transcript.ReparseKey(t.indexedWith) == indexingVersion(t.parser)
			a.mu.Unlock()
			t.mu.Unlock()
			if !fresh {
				return errDesktopCodeHeld
			}
		}
		if mode, known := a.modeOf(t); !known || mode != pathpolicy.Allow || !a.allowUpload(spec) {
			return errDesktopCodeHeld
		}
		file, err := cowork.OpenFile(root, spec.Path)
		if err != nil {
			return err
		}
		defer file.Close()
		fi, err := file.Stat()
		if err != nil {
			return err
		}
		if transcript.IdentityOf(fi) != identity {
			return devicesync.ErrSourceChanged
		}
		return nil
	}
	if err := auth.Check(ctx); err != nil {
		return nil, err
	}
	success = true
	return auth, nil
}

// The current verified metadata must still name this exact native source and
// owner. A stale target, changed backend or unrelated scoped parent cannot gain
// authorization merely because its pathname remains under the app container.
func desktopCodeSourceSignature(root, normalRoot string, spec devicesync.SourceSpec) (string, error) {
	r, err := desktopcode.Discover(root, normalRoot)
	if err != nil {
		return "", errDesktopCodeHeld
	}
	for _, entry := range r.Sessions {
		s := entry.Session
		matched := false
		if spec.StorageKind == transcript.StorageJSONLAppend {
			for _, src := range s.Sources() {
				matched = matched || (src.Path == spec.Path && src.SessionKey == spec.SessionKey && src.Parser == spec.Parser)
			}
		} else if spec.StorageKind == transcript.StorageCompanion {
			main := s.Transcript
			if main == "" {
				main = s.ProjectDir + string(os.PathSeparator) + s.SessionID + ".jsonl"
			}
			for _, companion := range s.Companions {
				owner, parent := s.SessionID, main
				for _, child := range s.Subagents {
					if child.MetaPath == companion.Path {
						owner, parent = "agent-"+child.AgentID, child.Path
					}
				}
				matched = matched || (companion.Path == spec.Path && owner == spec.SessionKey && parent == spec.Parent)
			}
		}
		if !matched {
			continue
		}
		b, err := json.Marshal(entry)
		if err != nil {
			return "", errDesktopCodeHeld
		}
		return fmt.Sprintf("%x", sha256.Sum256(b)), nil
	}
	return "", errDesktopCodeHeld
}
