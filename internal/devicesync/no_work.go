package devicesync

import (
	"context"

	"github.com/flopwire/flopwire/internal/transcript"
)

// noWork proves an ordinary file capture has no local capture or upload work.
// The caller holds the source operation lease. This reads only stored state and
// file metadata: a failed proof must fall through to fresh authorization before
// opening content. Protected sources retain their authorization policy checks.
func (s *Syncer) noWork(ctx context.Context, spec SourceSpec) bool {
	src, err := s.store.source(ctx, spec.Path, nil)
	if err != nil || src.Spec != spec || src.RepoSent != spec.repoKey() || src.Gen < 0 || src.ProtectedOrigin != "" || src.ProtectedRoot != "" {
		return false
	}
	w := src.Watermark
	if w == nil || w.Racy() || w.Identity.CTime == 0 || w.Identity.ID.IsZero() {
		return false
	}
	g, err := s.store.gen(ctx, src.ID, src.Gen)
	if err != nil || g == nil || g.Lost || g.Proof != nil || g.Acked != g.Entries || !g.TailAcked || g.Tail.Size != 0 || g.Size != w.Identity.Size {
		return false
	}
	pending, err := s.store.pendingGens(ctx, src.ID)
	if err != nil || len(pending) != 0 {
		return false
	}
	id, err := transcript.StatIdentity(spec.Path)
	return err == nil && id == w.Identity
}
